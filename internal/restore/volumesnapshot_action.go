/*
Copyright 2019, 2020 the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package restore

import (
	"context"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	snapshotv1api "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	core_v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vmware-tanzu/velero-plugin-for-csi/internal/util"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/label"
	"github.com/vmware-tanzu/velero/pkg/plugin/velero"
	riav2 "github.com/vmware-tanzu/velero/pkg/plugin/velero/restoreitemaction/v2"
	"github.com/vmware-tanzu/velero/pkg/util/boolptr"
)

// VolumeSnapshotRestoreItemAction is a Velero restore item action plugin for VolumeSnapshots
type VolumeSnapshotRestoreItemAction struct {
	Log logrus.FieldLogger
}

// AppliesTo returns information indicating that VolumeSnapshotRestoreItemAction should be invoked while restoring
// volumesnapshots.snapshot.storage.k8s.io resrouces.
func (p *VolumeSnapshotRestoreItemAction) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"volumesnapshots.snapshot.storage.k8s.io"},
	}, nil
}

func resetVolumeSnapshotSpecForRestore(vs *snapshotv1api.VolumeSnapshot, vscName *string) {
	// Spec of the backed-up object used the PVC as the source of the volumeSnapshot.
	// Restore operation will however, restore the volumesnapshot from the volumesnapshotcontent
	vs.Spec.Source.PersistentVolumeClaimName = nil
	vs.Spec.Source.VolumeSnapshotContentName = vscName
}

func resetVolumeSnapshotAnnotation(vs *snapshotv1api.VolumeSnapshot) {
	vs.ObjectMeta.Annotations[util.CSIVSCDeletionPolicy] = string(snapshotv1api.VolumeSnapshotContentRetain)
}

// Execute uses the data such as CSI driver name, storage snapshot handle, snapshot deletion secret (if any) from the annotations
// to recreate a volumesnapshotcontent object and statically bind the Volumesnapshot object being restored.
func (p *VolumeSnapshotRestoreItemAction) Execute(input *velero.RestoreItemActionExecuteInput) (*velero.RestoreItemActionExecuteOutput, error) {
	p.Log.Info("Starting VolumeSnapshotRestoreItemAction")
	if boolptr.IsSetToFalse(input.Restore.Spec.RestorePVs) {
		p.Log.Infof("Restore did not request for PVs to be restored %s/%s", input.Restore.Namespace, input.Restore.Name)
		return &velero.RestoreItemActionExecuteOutput{SkipRestore: true}, nil
	}
	var vs snapshotv1api.VolumeSnapshot

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(input.Item.UnstructuredContent(), &vs); err != nil {
		return &velero.RestoreItemActionExecuteOutput{}, errors.Wrapf(err, "failed to convert input.Item from unstructured")
	}

	// If cross-namespace restore is configured, change the namespace
	// for VolumeSnapshot object to be restored
	newNamespace, ok := input.Restore.Spec.NamespaceMapping[vs.GetNamespace()]
	if !ok {
		// Use original namespace
		newNamespace = vs.Namespace
	}

	_, snapClient, err := util.GetClients()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if !util.IsVolumeSnapshotExists(newNamespace, vs.Name, snapClient.SnapshotV1()) {
		snapHandle, exists := vs.Annotations[util.VolumeSnapshotHandleAnnotation]
		if !exists {
			return nil, errors.Errorf("Volumesnapshot %s/%s does not have a %s annotation", vs.Namespace, vs.Name, util.VolumeSnapshotHandleAnnotation)
		}

		csiDriverName, exists := vs.Annotations[util.CSIDriverNameAnnotation]
		if !exists {
			return nil, errors.Errorf("Volumesnapshot %s/%s does not have a %s annotation", vs.Namespace, vs.Name, util.CSIDriverNameAnnotation)
		}

		p.Log.Debugf("Set VolumeSnapshotContent %s/%s DeletionPolicy to Retain to make sure VS deletion in namespace will not delete Snapshot on cloud provider.",
			newNamespace, vs.Name)

		// TODO: generated name will be like velero-velero-something. Fix that.
		vsc := snapshotv1api.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "velero-" + vs.Name + "-",
				Labels: map[string]string{
					velerov1api.RestoreNameLabel: label.GetValidName(input.Restore.Name),
				},
			},
			Spec: snapshotv1api.VolumeSnapshotContentSpec{
				DeletionPolicy: snapshotv1api.VolumeSnapshotContentRetain,
				Driver:         csiDriverName,
				VolumeSnapshotRef: core_v1.ObjectReference{
					Kind:      util.VolumeSnapshotKindName,
					Namespace: newNamespace,
					Name:      vs.Name,
				},
				Source: snapshotv1api.VolumeSnapshotContentSource{
					SnapshotHandle: &snapHandle,
				},
			},
		}

		// we create the volumesnapshotcontent here instead of relying on the restore flow because we want to statically
		// bind this volumesnapshot with a volumesnapshotcontent that will be used as its source for pre-populating the
		// volume that will be created as a result of the restore. To perform this static binding, a bi-didrectional link
		// between the volumesnapshotcontent and volumesnapshot objects have to be setup.
		// Further, it is disallowed to convert a dynamically created volumesnapshotcontent for static binding.
		// See: https://github.com/kubernetes-csi/external-snapshotter/issues/274
		vscupd, err := snapClient.SnapshotV1().VolumeSnapshotContents().Create(context.TODO(), &vsc, metav1.CreateOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create volumesnapshotcontents %s", vsc.GenerateName)
		}
		p.Log.Infof("Created VolumesnapshotContents %s with static binding to volumesnapshot %s/%s", vscupd, newNamespace, vs.Name)

		// Reset Spec to convert the volumesnapshot from using the dyanamic volumesnapshotcontent to the static one.
		resetVolumeSnapshotSpecForRestore(&vs, &vscupd.Name)

		// Reset VolumeSnapshot annotation. By now, only change DeletionPolicy to Retain.
		resetVolumeSnapshotAnnotation(&vs)
	}

	vsMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&vs)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	p.Log.Infof("Returning from VolumeSnapshotRestoreItemAction with no additionalItems")

	return &velero.RestoreItemActionExecuteOutput{
		UpdatedItem:     &unstructured.Unstructured{Object: vsMap},
		AdditionalItems: []velero.ResourceIdentifier{},
	}, nil
}

func (p *VolumeSnapshotRestoreItemAction) Name() string {
	return "VolumeSnapshotRestoreItemAction"
}

func (p *VolumeSnapshotRestoreItemAction) Progress(operationID string, restore *velerov1api.Restore) (velero.OperationProgress, error) {
	progress := velero.OperationProgress{}

	if operationID == "" {
		return progress, riav2.InvalidOperationIDError(operationID)
	}

	return progress, nil
}

func (p *VolumeSnapshotRestoreItemAction) Cancel(operationID string, restore *velerov1api.Restore) error {
	return nil
}

func (p *VolumeSnapshotRestoreItemAction) AreAdditionalItemsReady(additionalItems []velero.ResourceIdentifier, restore *velerov1api.Restore) (bool, error) {
	return true, nil
}
