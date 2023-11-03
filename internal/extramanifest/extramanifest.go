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

package extramanifest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-logr/logr"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const extraManifestPath = "extra-manifests/"

// EMHandler handles the extra manifests
type EMHandler struct {
	Client client.Client
	Log    logr.Logger
}

// ExportExtraManifestToDir extracts the extra manifests from configmaps
// and writes them to the given directory
func (h *EMHandler) ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef, toDir string) error {
	if extraManifestCMs == nil {
		h.Log.Info("No extra manifests configmap is provided.")
		return nil
	}

	configmaps, err := getManifestConfigmaps(ctx, h.Client, extraManifestCMs)
	if err != nil {
		return err
	}

	// Create the directory for the extra manifests
	if err := os.MkdirAll(filepath.Join(toDir, extraManifestPath), 0o700); err != nil {
		return err
	}

	for i, cm := range configmaps {
		for _, value := range cm.Data {
			manifest := unstructured.Unstructured{}
			err := yaml.Unmarshal([]byte(value), &manifest)
			if err != nil {
				return err
			}

			fileName := strconv.Itoa(i) + "_" + manifest.GetName() + "_" + manifest.GetNamespace() + ".yaml"
			filePath := filepath.Join(toDir, extraManifestPath, fileName)
			err = writeManifestToFile(&manifest, filePath)
			if err != nil {
				return err
			}
			h.Log.Info("Exported manifest to file", "path", filePath)
		}
	}

	return nil
}

// ApplyExtraManifestsFromDir applies the extra manifests from the given directory
func (h *EMHandler) ApplyExtraManifestsFromDir(ctx context.Context, fromDir string) error {
	manifestDir := filepath.Join(fromDir, extraManifestPath)
	manifestYamls, err := os.ReadDir(manifestDir)
	if err != nil {
		return err
	}

	for _, manifestYaml := range manifestYamls {
		if manifestYaml.IsDir() {
			h.Log.Info("Unexpected directory found, skipping...", "directory",
				filepath.Join(manifestDir, manifestYaml.Name()))
			continue
		}

		manifestBytes, err := os.ReadFile(filepath.Join(manifestDir, manifestYaml.Name()))
		if err != nil {
			return err
		}

		manifest := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(manifestBytes, manifest); err != nil {
			return err
		}

		manifest.SetUID("")
		manifest.SetResourceVersion("")
		if err := h.Client.Create(ctx, manifest); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return err
			}

			// Update if it already exists
			existingManifest := &unstructured.Unstructured{}
			existingManifest.SetGroupVersionKind(manifest.GroupVersionKind())
			if err := h.Client.Get(ctx, types.NamespacedName{
				Name:      manifest.GetName(),
				Namespace: manifest.GetNamespace(),
			}, existingManifest); err != nil {
				return err
			}

			manifest.SetResourceVersion(existingManifest.GetResourceVersion())
			manifest.SetUID(existingManifest.GetUID())
			if err := h.Client.Update(ctx, manifest); err != nil {
				return err
			}
			h.Log.Info("Updated manifest", "name", manifest.GetName(), "namespace", manifest.GetNamespace())
		} else {
			h.Log.Info("Created manifest", "name", manifest.GetName(), "namespace", manifest.GetNamespace())
		}
	}

	return nil
}

func getManifestConfigmaps(ctx context.Context, c client.Client, configMaps []lcav1alpha1.ConfigMapRef) ([]*corev1.ConfigMap, error) {
	var cms []*corev1.ConfigMap

	for _, cm := range configMaps {
		existingCm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{
			Name:      cm.Name,
			Namespace: cm.Namespace,
		}, existingCm)
		if err != nil {
			return nil, err
		}
		cms = append(cms, existingCm)
	}

	return cms, nil
}

func writeManifestToFile(manifest *unstructured.Unstructured, filePath string) error {
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, manifestBytes, 0o644); err != nil {
		return err
	}
	return nil
}
