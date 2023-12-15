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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-logr/logr"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ExtraManifestPath = "/opt/extra-manifests"

type EManifestHandler interface {
	ApplyExtraManifests(ctx context.Context, fromDir string) error
	ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef, toDir string) error
}

// EMHandler handles the extra manifests
type EMHandler struct {
	Client client.Client
	Log    logr.Logger
}

// EMStatusError type
type EMStatusError struct {
	Reason     string
	ErrMessage string
}

func (e *EMStatusError) Error() string {
	return fmt.Sprintf(e.ErrMessage)
}

func NewEMFailedError(msg string) *EMStatusError {
	return &EMStatusError{
		Reason:     "Failed",
		ErrMessage: msg,
	}
}

func IsEMFailedError(err error) bool {
	var emErr *EMStatusError
	if errors.As(err, &emErr) {
		return emErr.Reason == "Failed"
	}
	return false
}

// ExportExtraManifestToDir extracts the extra manifests from configmaps
// and writes them to the given directory
func (h *EMHandler) ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef, toDir string) error {
	if extraManifestCMs == nil {
		h.Log.Info("No extra manifests configmap is provided.")
		return nil
	}

	configmaps, err := common.GetConfigMaps(ctx, h.Client, extraManifestCMs)
	if err != nil {
		return err
	}

	// Create the directory for the extra manifests
	if err := os.MkdirAll(filepath.Join(toDir, ExtraManifestPath), 0o700); err != nil {
		return err
	}

	for i, cm := range configmaps {
		for _, value := range cm.Data {
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(value), 4096)
			for {
				manifest := unstructured.Unstructured{}
				err := decoder.Decode(&manifest)
				if err != nil {
					if errors.Is(err, io.EOF) {
						// Reach the end of the data, exit the loop
						break
					}
					return err
				}
				// In case it contains the UID and ResourceVersion, remove them
				manifest.SetUID("")
				manifest.SetResourceVersion("")

				fileName := strconv.Itoa(i) + "_" + manifest.GetName() + "_" + manifest.GetNamespace() + ".yaml"
				filePath := filepath.Join(toDir, ExtraManifestPath, fileName)
				err = utils.MarshalToYamlFile(&manifest, filePath)
				if err != nil {
					return err
				}
				h.Log.Info("Exported manifest to file", "path", filePath)
			}
		}
	}

	return nil
}

// ApplyExtraManifests applies the extra manifests from the preserved extra manifests directory
func (h *EMHandler) ApplyExtraManifests(ctx context.Context, fromDir string) error {
	manifestYamls, err := os.ReadDir(fromDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	h.Log.Info("Applying extra manifests")
	if len(manifestYamls) == 0 {
		h.Log.Info("No extra manifests found", "path", fromDir)
		return nil
	}

	c, mapper, err := common.NewDynamicClientAndRESTMapper()
	if err != nil {
		return err
	}

	for _, manifestYaml := range manifestYamls {
		manifestYamlPath := filepath.Join(fromDir, manifestYaml.Name())
		if manifestYaml.IsDir() {
			h.Log.Info("Unexpected directory found, skipping", "directory", manifestYamlPath)
			continue
		}

		manifest := &unstructured.Unstructured{}
		if err := utils.ReadYamlOrJSONFile(manifestYamlPath, manifest); err != nil {
			return err
		}
		// Mapping resource GVK to CVR
		mapping, err := mapper.RESTMapping(manifest.GroupVersionKind().GroupKind(), manifest.GroupVersionKind().Version)
		if err != nil {
			return err
		}

		h.Log.Info("Applying manifest from file", "path", manifestYamlPath)
		// Check if the resource exists
		existingManifest := &unstructured.Unstructured{}
		existingManifest.SetGroupVersionKind(manifest.GroupVersionKind())
		resource := c.Resource(mapping.Resource).Namespace(manifest.GetNamespace())
		existingManifest, err = resource.Get(ctx, manifest.GetName(), metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				return err
			}
			// Create if it doesn't exist
			if _, err := resource.Create(ctx, manifest, metav1.CreateOptions{}); err != nil {
				// Capture both invalid syntax and webhook validation errors
				if k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) {
					errMsg := fmt.Sprintf("Failed to create manifest %s %s: %s",
						manifest.GetKind(), manifest.GetName(), err.Error())
					h.Log.Error(nil, errMsg)
					return NewEMFailedError(errMsg)
				}
				return err
			}
			h.Log.Info("Created manifest", "manifest", manifest.GetName())
		} else {
			manifest.SetResourceVersion(existingManifest.GetResourceVersion())
			if _, err := resource.Update(ctx, manifest, metav1.UpdateOptions{}); err != nil {
				// Capture both invalid syntax and webhook validation errors
				if k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) {
					errMsg := fmt.Sprintf("Failed to update manifest %s %s: %s",
						manifest.GetKind(), manifest.GetName(), err.Error())
					h.Log.Error(nil, errMsg)
					return NewEMFailedError(errMsg)
				}
				return err
			}
			h.Log.Info("Updated manifest", "manifest", manifest.GetName())
		}
	}

	// Remove the extra manifests directory
	if err := os.RemoveAll(fromDir); err != nil {
		return err
	}
	h.Log.Info("Extra manifests path removed", "path", fromDir)
	return nil
}
