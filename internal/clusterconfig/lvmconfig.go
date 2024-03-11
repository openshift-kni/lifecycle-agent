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

package clusterconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	cp "github.com/otiai10/copy"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:rbac:groups=local.storage.openshift.io,resources=localvolumes,verbs=get;list;watch

func (r *UpgradeClusterConfigGather) FetchLvmConfig(ctx context.Context, ostreeDir string) error {
	r.Log.Info("Fetching node lvm configuration")
	lvmConfigPath := filepath.Join(ostreeDir, common.OptOpenshift, common.LvmConfigDir)
	if err := os.MkdirAll(lvmConfigPath, 0o700); err != nil {
		return fmt.Errorf("failed to make dir in %s: %w", lvmConfigPath, err)
	}

	if err := r.fetchLvmDevices(lvmConfigPath); err != nil {
		return err
	}

	manifestsDir := filepath.Join(ostreeDir, common.OptOpenshift, common.ClusterConfigDir, manifestDir)
	if err := r.fetchLocalVolumes(ctx, manifestsDir); err != nil {
		return err
	}

	r.Log.Info("Successfully fetched lvm configuration")
	return nil
}

func (r *UpgradeClusterConfigGather) fetchLvmDevices(lvmConfigPath string) error {
	r.Log.Info("Copying lvm devices file", "file", common.LvmDevicesPath, "to", lvmConfigPath)
	err := cp.Copy(
		filepath.Join(hostPath, common.LvmDevicesPath),
		filepath.Join(lvmConfigPath, filepath.Base(common.LvmDevicesPath)))
	if err != nil {
		if os.IsNotExist(err) {
			r.Log.Info("lvm devices file does not exist")
			return nil
		}
		return fmt.Errorf("failed to lvm devices file: %w", err)
	}

	return nil
}

func (r *UpgradeClusterConfigGather) fetchLocalVolumes(ctx context.Context, manifestsDir string) error {
	r.Log.Info("Fetching local volumes")

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: "localvolumes.local.storage.openshift.io"}, crd); err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("LocalVolume CRD is not found. Skipping")
			return nil
		}
		return fmt.Errorf("failed to get LocalVolume CRD: %w", err)
	}

	// List localvolumes
	lvsList := &unstructured.UnstructuredList{}
	lvsList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "local.storage.openshift.io",
		Kind:    "LocalVolumeList",
		Version: "v1",
	})
	if err := r.List(ctx, lvsList); err != nil {
		return fmt.Errorf("failed to list localvolumes: %w", err)
	}

	for _, lv := range lvsList.Items {
		// Unset uid and resource version
		lv.SetUID("")
		lv.SetResourceVersion("")

		filePath := filepath.Join(manifestsDir, lv.GetName()+"_"+lv.GetNamespace()+".json")
		r.Log.Info("Writing LocalVolume to file", "path", filePath)
		err := utils.MarshalToFile(lv.Object, filePath)
		if err != nil {
			return fmt.Errorf("failed to write localvolume to %s: %w", filePath, err)
		}
	}

	return nil
}
