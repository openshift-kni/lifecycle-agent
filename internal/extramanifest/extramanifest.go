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
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
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
	ValidateExtraManifestConfigmaps(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef) error
}

// EMHandler handles the extra manifests
type EMHandler struct {
	Client        client.Client
	DynamicClient dynamic.Interface
	Log           logr.Logger
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

func NewEMFailedValidationError(msg string) *EMStatusError {
	return &EMStatusError{
		Reason:     "FailedValidation",
		ErrMessage: msg,
	}
}

func NewEMUnknownCRDError(msg string) *EMStatusError {
	return &EMStatusError{
		Reason:     "UnknownCRD",
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

func IsEMFailedValidationError(err error) bool {
	var emErr *EMStatusError
	if errors.As(err, &emErr) {
		return emErr.Reason == "FailedValidation"
	}
	return false
}

func IsEMUnknownCRDError(err error) bool {
	var emErr *EMStatusError
	if errors.As(err, &emErr) {
		return emErr.Reason == "UnknownCRD"
	}
	return false
}

func ApplyExtraManifest(ctx context.Context, dc dynamic.Interface, restMapper meta.RESTMapper, manifest *unstructured.Unstructured, isDryRun bool) error {
	// Mapping resource GVK to CVR
	mapping, err := restMapper.RESTMapping(manifest.GroupVersionKind().GroupKind(), manifest.GroupVersionKind().Version)
	if err != nil {
		return fmt.Errorf("failed to get RESTMapping: %w", err)
	}

	// Check if the resource exists
	existingManifest := &unstructured.Unstructured{}
	existingManifest.SetGroupVersionKind(manifest.GroupVersionKind())
	resource := dc.Resource(mapping.Resource).Namespace(manifest.GetNamespace())
	existingManifest, err = resource.Get(ctx, manifest.GetName(), metav1.GetOptions{})
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
		}

		opts := metav1.CreateOptions{}
		if isDryRun {
			opts = metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			}
		}

		manifest.SetResourceVersion("")
		if _, err := resource.Create(ctx, manifest, opts); err != nil {
			return fmt.Errorf("failed to create manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
		}
	} else {
		opts := metav1.UpdateOptions{}
		if isDryRun {
			opts = metav1.UpdateOptions{
				DryRun: []string{metav1.DryRunAll},
			}
		}

		manifest.SetResourceVersion(existingManifest.GetResourceVersion())
		if _, err := resource.Update(ctx, manifest, opts); err != nil {
			return fmt.Errorf("failed to update manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
		}
	}
	return nil
}

func (h *EMHandler) ValidateExtraManifestConfigmaps(ctx context.Context, content []lcav1alpha1.ConfigMapRef) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.Error(err, "The extraManifests configMap is not found")
			errMsg := fmt.Sprintf("the extraManifests configMap is not found, error: %v. Please create the configMap.", err)
			return NewEMFailedValidationError(errMsg)
		}
		return fmt.Errorf("failed to get configMaps to export extraManifest to dir: %w", err)
	}

	manifests, err := h.extractManifestFromConfigmaps(configmaps)
	if err != nil {
		return fmt.Errorf("failed to extract manifests from configMap: %w", err)
	}

	// MachineConfig can trigger reboot and this will result in not meeting KPI downtime requirement
	// so raise validation error if there is a MachineConfig in extramanifests
	for _, manifest := range manifests {
		if manifest.GetKind() == "MachineConfig" && manifest.GetAPIVersion() == "machineconfiguration.openshift.io/v1" {
			return NewEMFailedValidationError("Using MachingConfigs in extramanifests is not allowed")
		}
	}

	// Apply manifest with dryrun
	dryrun := true
	for _, manifest := range manifests {
		err = ApplyExtraManifest(ctx, h.DynamicClient, h.Client.RESTMapper(), manifest, dryrun)
		if err != nil {
			h.Log.Error(err, "Failed to apply manifest with dryrun")
			errMsg := fmt.Sprintf("failed to apply manifest with dryrun: %v", err.Error())

			// Capture both invalid syntax and webhook validation errors
			if k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) {
				return NewEMFailedValidationError(errMsg)
			}

			// Capture unknown CRD issue
			var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
			if errors.As(err, &groupDiscoveryErr) || meta.IsNoMatchError(err) {
				return NewEMUnknownCRDError(errMsg)
			}

			return fmt.Errorf(errMsg)
		}
	}

	h.Log.Info("ExtraManifests configmaps are validated", "configmaps", content)
	return nil
}

func (h *EMHandler) extractManifestFromConfigmaps(configmaps []corev1.ConfigMap) ([]*unstructured.Unstructured, error) {
	var manifests []*unstructured.Unstructured

	for _, cm := range configmaps {
		for _, value := range cm.Data {
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(value), 4096)
			for {
				resource := unstructured.Unstructured{}
				err := decoder.Decode(&resource)
				if err != nil {
					if errors.Is(err, io.EOF) {
						// Reach the end of the data, exit the loop
						break
					}
					h.Log.Error(err, "Failed to decode yaml in the configMap", "configmap", cm.Name)
					errMsg := fmt.Sprintf("failed to decode yaml in the configMap %s: %v", cm.Name, err.Error())
					return nil, NewEMFailedValidationError(errMsg)
				}
				// In case it contains the UID and ResourceVersion, remove them
				resource.SetUID("")
				resource.SetResourceVersion("")

				manifests = append(manifests, &resource)
			}
		}
	}
	return manifests, nil
}

// ExportExtraManifestToDir extracts the extra manifests from configmaps
// and writes them to the given directory
func (h *EMHandler) ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []lcav1alpha1.ConfigMapRef, toDir string) error {
	if extraManifestCMs == nil {
		h.Log.Info("No extra manifests configmap is provided")
		return nil
	}

	configmaps, err := common.GetConfigMaps(ctx, h.Client, extraManifestCMs)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.Error(err, "The extraManifests configMap is not found")
			errMsg := fmt.Sprintf("the extraManifests configMap is not found, error: %v. Please create the configMap.", err)
			return NewEMFailedError(errMsg)
		}

		return fmt.Errorf("failed to get configMaps to export extraManifest to dir: %w", err)
	}

	manifests, err := h.extractManifestFromConfigmaps(configmaps)
	if err != nil {
		return fmt.Errorf("failed to extract manifests from configMap: %w", err)
	}

	sortedManifests, err := common.SortAndGroupByApplyWave[*unstructured.Unstructured](manifests)
	if err != nil {
		return fmt.Errorf("failed to sort and group manifests: %w", err)
	}

	// Create the directory for the extra manifests
	exMDirPath := filepath.Join(toDir, ExtraManifestPath)
	if err := os.MkdirAll(exMDirPath, 0o700); err != nil {
		return fmt.Errorf("failed to create directory for extra manifests in %s: %w", exMDirPath, err)
	}
	for i, manifestGroup := range sortedManifests {
		group := filepath.Join(toDir, ExtraManifestPath, "group"+strconv.Itoa(i+1))
		if err := os.MkdirAll(group, 0o700); err != nil {
			return fmt.Errorf("failed make dir in %s: %w", group, err)
		}

		for j, manifest := range manifestGroup {
			fileName := fmt.Sprintf("%d_%s_%s_%s.yaml", j+1, manifest.GetKind(), manifest.GetName(), manifest.GetNamespace())
			filePath := filepath.Join(group, fileName)
			if err := utils.MarshalToYamlFile(manifest, filePath); err != nil {
				return fmt.Errorf("failed to marshal manifest %s to yaml: %w", manifest.GetName(), err)
			}
			h.Log.Info("Exported manifest to file", "path", filePath)
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
			gvk := object.GroupVersionKind()
			manifestFilePath := filepath.Join(manifestsDir, fmt.Sprintf(
				"%d_%s_%s_%s.yaml", i, gvk.Kind, object.GetName(), object.GetNamespace()))
			if err := utils.MarshalToYamlFile(&object, manifestFilePath); err != nil { //nolint:gosec
				return fmt.Errorf("failed to save manifests to file %s: %w", manifestFilePath, err)
			}
			h.Log.Info("Extracted from policy and exported manifest to file", "policy", policy.GetName(), "path", manifestFilePath)
		}
	}

	return nil
}

// ApplyExtraManifests applies the extra manifests from the preserved extra manifests directory
func (h *EMHandler) ApplyExtraManifests(ctx context.Context, fromDir string) error {
	manifests, err := utils.LoadGroupedManifestsFromPath(fromDir, &h.Log)
	if err != nil {
		return fmt.Errorf("failed to read extra manifests from path: %w", err)
	}
	if manifests == nil || len(manifests) == 0 {
		h.Log.Info("No extra manifests to apply")
		return nil
	}

	for _, group := range manifests {
		for _, manifest := range group {
			h.Log.Info("Applying manifest", "kind", manifest.GetKind(), "name", manifest.GetName())
			err = ApplyExtraManifest(ctx, h.DynamicClient, h.Client.RESTMapper(), manifest, false)
			if err != nil {
				h.Log.Error(err, "Failed to apply manifest")
				errMsg := fmt.Sprintf("failed to apply manifest: %v", err.Error())

				// Capture both invalid syntax and webhook validation errors
				if k8serrors.IsInvalid(err) || k8serrors.IsBadRequest(err) {
					return NewEMFailedError(errMsg)
				}

				// Capture unknown CRD issue
				var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
				if errors.As(err, &groupDiscoveryErr) || meta.IsNoMatchError(err) {
					return NewEMFailedError(errMsg)
				}
				return fmt.Errorf(errMsg)
			}

			h.Log.Info("Applied manifest", "name", manifest.GetName(), "namespace", manifest.GetNamespace())
		}
	}

	// Remove the extra manifests directory
	if err := os.RemoveAll(fromDir); err != nil {
		return fmt.Errorf("failed to remove manifests from %s: %w", fromDir, err)
	}
	h.Log.Info("Extra manifests path removed", "path", fromDir)
	return nil
}
