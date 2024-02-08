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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ExtraManifestPath  = "/opt/extra-manifests"
	PolicyManifestPath = "/opt/policy-manifests"
)

type EManifestHandler interface {
	ApplyExtraManifests(ctx context.Context, fromDir string) error
	ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef, toDir string) error
	ExtractAndExportManifestFromPoliciesToDir(ctx context.Context, policyLabels, objectLabels map[string]string, toDir string) error
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
		return fmt.Errorf("failed to get configMaps to export extraManifest to dir: %w", err)
	}

	// Create the directory for the extra manifests
	exMDirPath := filepath.Join(toDir, ExtraManifestPath)
	if err := os.MkdirAll(exMDirPath, 0o700); err != nil {
		return fmt.Errorf("failed to create directory for extra manifests in %s: %w", exMDirPath, err)
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
					return fmt.Errorf("failed to decode manifest: %w", err)
				}
				// In case it contains the UID and ResourceVersion, remove them
				manifest.SetUID("")
				manifest.SetResourceVersion("")

				fileName := strconv.Itoa(i) + "_" + manifest.GetName() + "_" + manifest.GetNamespace() + ".yaml"
				filePath := filepath.Join(toDir, ExtraManifestPath, fileName)
				err = utils.MarshalToYamlFile(&manifest, filePath)
				if err != nil {
					return fmt.Errorf("failed to marshal manifest %s to yaml: %w", manifest.GetName(), err)
				}
				h.Log.Info("Exported manifest to file", "path", filePath)
			}
		}
	}

	return nil
}

// ExtractAndExportManifestFromPoliciesToDir extracts CR specs from policies. It matches policies and/or CRs by labels.
func (h *EMHandler) ExtractAndExportManifestFromPoliciesToDir(ctx context.Context, policyLabels, objectLabels map[string]string, toDir string) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: "policies.policy.open-cluster-management.io"}, crd); err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.Info("Skipping extraction from policies as the policy CRD is not found. This is expected if the cluster is not managed by ACM")
			return nil
		} else {
			return fmt.Errorf("error while check policy CRD: %w", err)
		}
	}

	// Create the directory for the extra manifests
	manifestsDir := filepath.Join(toDir, PolicyManifestPath)
	if err := os.MkdirAll(manifestsDir, 0o700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", manifestsDir, err)
	}

	policies, err := h.GetPolicies(ctx, policyLabels)
	if err != nil {
		return fmt.Errorf("failed to get policies: %w", err)
	}

	for i, policy := range policies {
		objects, err := getConfigurationObjects(policy, objectLabels)
		if err != nil {
			return fmt.Errorf("failed to extract manifests from policies: %w", err)
		}
		for _, object := range objects {
			manifestFilePath := filepath.Join(manifestsDir, fmt.Sprintf("%d_%s_%s.yaml", i, object.GetName(), object.GetNamespace()))
			if err := utils.MarshalToYamlFile(&object, manifestFilePath); err != nil {
				return fmt.Errorf("failed to save manifests to file %s: %w", manifestFilePath, err)
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
		return fmt.Errorf("failed to read extraManifest from dir %s: %w", fromDir, err)
	}

	h.Log.Info("Applying extra manifests")
	if len(manifestYamls) == 0 {
		h.Log.Info("No extra manifests found", "path", fromDir)
		return nil
	}

	c, mapper, err := common.NewDynamicClientAndRESTMapper()
	if err != nil {
		return fmt.Errorf("failed to get NewDynamicClientAndRESTMapper for extraManifests: %w", err)
	}

	for _, manifestYaml := range manifestYamls {
		manifestYamlPath := filepath.Join(fromDir, manifestYaml.Name())
		if manifestYaml.IsDir() {
			h.Log.Info("Unexpected directory found, skipping", "directory", manifestYamlPath)
			continue
		}

		manifest := &unstructured.Unstructured{}
		if err := utils.ReadYamlOrJSONFile(manifestYamlPath, manifest); err != nil {
			return fmt.Errorf("failed to read manifest: %w", err)
		}
		// Mapping resource GVK to CVR
		mapping, err := mapper.RESTMapping(manifest.GroupVersionKind().GroupKind(), manifest.GroupVersionKind().Version)
		if err != nil {
			return fmt.Errorf("failed to get RESTMapping for %+v-%s: %w", manifest.GroupVersionKind().GroupKind(), manifest.GroupVersionKind().Version, err)
		}

		h.Log.Info("Applying manifest from file", "path", manifestYamlPath)
		// Check if the resource exists
		existingManifest := &unstructured.Unstructured{}
		existingManifest.SetGroupVersionKind(manifest.GroupVersionKind())
		resource := c.Resource(mapping.Resource).Namespace(manifest.GetNamespace())
		existingManifest, err = resource.Get(ctx, manifest.GetName(), metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				return fmt.Errorf("failed to get extramanifest called %s: %w", manifest.GetName(), err)
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
				return fmt.Errorf("failed to create extramanifest called %s: %w", manifest.GetName(), err)
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
				return fmt.Errorf("failed to update manifest %s: %w", manifest.GetName(), err)
			}
			h.Log.Info("Updated manifest", "manifest", manifest.GetName())
		}
	}

	// Remove the extra manifests directory
	if err := os.RemoveAll(fromDir); err != nil {
		return fmt.Errorf("failed to remove manifests from %s: %w", fromDir, err)
	}
	h.Log.Info("Extra manifests path removed", "path", fromDir)
	return nil
}
