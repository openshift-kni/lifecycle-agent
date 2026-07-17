/*
Copyright 2023.

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

package backuprestore

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch;update

// PatchPVsReclaimPolicy - when LVMS is configured, we need to patch PVs with Retain as
// the persistentVolumeReclaimPolicy to prevent data loss during upgrade.
func (h *BRHandler) PatchPVsReclaimPolicy(ctx context.Context) error {
	pvList := &corev1.PersistentVolumeList{}
	if err := h.List(ctx, pvList); err != nil {
		return fmt.Errorf("failed to list PersistentVolumes: %w", err)
	}

	if len(pvList.Items) == 0 {
		h.Log.Info("No PersistentVolumes found in the cluster, skipping.")
		return nil
	}

	for _, pv := range pvList.Items {
		if pvAnnotations := pv.GetAnnotations(); pvAnnotations != nil {
			hasLvmsAnnotation := pvAnnotations[topolvmAnnotation] == topolvmValue

			if hasLvmsAnnotation && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimDelete {
				h.Log.Info("Patching persistentVolumeReclaimPolicy to Retain", "pv-name", pv.Name)

				pvPatched := pv
				pvPatched.Spec.PersistentVolumeReclaimPolicy = "Retain"
				pvPatched.Annotations[updatedReclaimPolicyAnnotation] = "true" //nolint:goconst
				if err := h.Update(ctx, &pvPatched); err != nil {
					return fmt.Errorf("failed to update PersistentVolume %s: %w", pv.Name, err)
				}
			}
		}
	}

	return nil
}

// RestorePVsReclaimPolicy restores the persistentVolumeReclaimPolicy field back to its original value,
// in PVs created by LVMS (this field was updated during pre-pivot by LCA).
func (h *BRHandler) RestorePVsReclaimPolicy(ctx context.Context) error {
	pvList := &corev1.PersistentVolumeList{}
	if err := h.List(ctx, pvList); err != nil {
		return fmt.Errorf("failed to list PersistentVolumes: %w", err)
	}

	if len(pvList.Items) == 0 {
		h.Log.Info("No PersistentVolumes found in the cluster, skipping.")
		return nil
	}

	for _, pv := range pvList.Items {
		if pvAnnotations := pv.GetAnnotations(); pvAnnotations != nil {
			hasReclaimPolicyAnnotation := pvAnnotations[updatedReclaimPolicyAnnotation] == "true"

			if hasReclaimPolicyAnnotation {
				h.Log.Info("Restoring back the persistentVolumeReclaimPolicy to Delete", "pv-name", pv.Name)

				pvPatched := pv
				pvPatched.Spec.PersistentVolumeReclaimPolicy = "Delete"
				delete(pvPatched.Annotations, updatedReclaimPolicyAnnotation)
				if err := h.Update(ctx, &pvPatched); err != nil {
					return fmt.Errorf("failed to update PersistentVolume %s: %w", pv.Name, err)
				}
			}
		}
	}

	return nil
}
