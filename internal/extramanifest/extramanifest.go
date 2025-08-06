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
	"strings"

	"github.com/coreos/go-semver/semver"
	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/samber/lo"
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
	policyv1 "open-cluster-management.io/config-policy-controller/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;watch;create;

const (
	CmManifestPath              = "/opt/extra-manifests"  // manifests extracted from configmaps
	PolicyManifestPath          = "/opt/policy-manifests" // manifests extracted from policies
	ValidationWarningAnnotation = "extra-manifest.lca.openshift.io/validation-warning"
)

// unSupported resource type in extramanifests configmaps
var unSupportedResources = map[string][]string{
	"machineconfiguration.openshift.io/v1": {"MachineConfig"},
	"operators.coreos.com/v1alpha1":        {"ClusterServiceVersion", "InstallPlan", "Subscription"},
}

type EManifestHandler interface {
	ApplyExtraManifests(ctx context.Context, fromDir string) error
	ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []ibuv1.ConfigMapRef, toDir string) error
	ExtractAndExportManifestFromPoliciesToDir(ctx context.Context, policyLabels, objectLabels, validationAnns map[string]string, toDir string) error
	ValidateAndExtractManifestFromPolicies(ctx context.Context, policyLabels, objectLabels, validationAnns map[string]string) ([][]*unstructured.Unstructured, error)
	ValidateExtraManifestConfigmaps(ctx context.Context, extraManifestCMs []ibuv1.ConfigMapRef) (string, error)
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
	return e.ErrMessage
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

func IsEMFailedError(err error) bool {
	var emErr *EMStatusError
	if errors.As(err, &emErr) {
		return emErr.Reason == "Failed"
	}
	return false
}

func ApplyExtraManifest(ctx context.Context, dc dynamic.Interface, restMapper meta.RESTMapper, manifest *unstructured.Unstructured, isDryRun bool) error {
	// Mapping resource GVK to CVR
	mapping, err := restMapper.RESTMapping(manifest.GroupVersionKind().GroupKind(), manifest.GroupVersionKind().Version)
	if err != nil {
		return fmt.Errorf("failed to get RESTMapping: %w", err)
	}

	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		// cluster-scoped resource
		resource = dc.Resource(mapping.Resource)
	} else {
		// namespaced-scoped resource
		manifestNs := manifest.GetNamespace()
		if manifestNs == "" {
			manifestNs = "default"
		}
		resource = dc.Resource(mapping.Resource).Namespace(manifestNs)
	}

	// Check if the resource exists
	existingManifest := &unstructured.Unstructured{}
	existingManifest.SetGroupVersionKind(manifest.GroupVersionKind())
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
		applyType := common.ApplyTypeReplace
		if metadata, exists := manifest.Object["metadata"].(map[string]interface{}); exists {
			if annotations, exists := metadata["annotations"].(map[string]interface{}); exists {
				if at, exists := annotations[common.ApplyTypeAnnotation]; exists {
					applyType = at.(string)
					// Remove the annotation as it serves no purpose at runtime
					delete(annotations, common.ApplyTypeAnnotation)
				}
			}
		}

		opts := metav1.UpdateOptions{}
		if isDryRun {
			opts = metav1.UpdateOptions{
				DryRun: []string{metav1.DryRunAll},
			}
		}

		switch applyType {
		case common.ApplyTypeReplace:
			manifest.SetResourceVersion(existingManifest.GetResourceVersion())
			if _, err := resource.Update(ctx, manifest, opts); err != nil {
				return fmt.Errorf("failed to replace manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
			}

		case common.ApplyTypeMerge:
			mergedObj, err := mergeSpecs(manifest.Object, existingManifest.Object, string(policyv1.MustHave), false)
			if err != nil {
				return fmt.Errorf("failed to merge manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
			}

			merged := &unstructured.Unstructured{Object: mergedObj.(map[string]interface{})}
			merged.SetResourceVersion(existingManifest.GetResourceVersion())
			if _, err := resource.Update(ctx, merged, opts); err != nil {
				return fmt.Errorf("failed to update manifest %s called %s: %w", manifest.GetKind(), manifest.GetName(), err)
			}

		}

	}
	return nil
}

func fakeNamespaceForDryrun(ctx context.Context, c client.Client, log logr.Logger, nsName string) (bool, error) {
	ns := &corev1.Namespace{}
	ns.SetName(nsName)
	if err := c.Create(ctx, ns); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("failed to create namespace %s: %w", ns, err)
		}
		// Already exists
		return false, nil
	} else {
		log.Info(fmt.Sprintf("Created namespace %s for dryrun", ns.Name))
		return true, nil
	}
}

func deleteFakedNamespacesForDryrun(ctx context.Context, c client.Client, log logr.Logger, nsSet map[string]bool) error {
	// Cleanup any faked namespaces at the end of validation
	for name := range nsSet {
		ns := &corev1.Namespace{}
		ns.SetName(name)
		if err := c.Delete(ctx, ns); err != nil {
			if !k8serrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete namespace %s: %w", ns.Name, err)
			}
		}
		log.Info(fmt.Sprintf("Deleted faked namespace %s for dryrun", ns.Name))
	}
	return nil
}

// ValidateExtraManifestConfigmaps validates IBU extramanifest configmaps and returns validation warning and error.
// It iterates over all manifests included in the configmaps and perform Kubernetes Dry-run.
// Errors such as Invalid or webhook BadRequest types from Dry-run apply are considered warnings. This also includes
// cases where CRDs are missing from the current stateroot and dependent namespace does not exist on the current
// stateroot but is also not found in the configmaps. Other validation failures are treated as errors, such as random
// chars, missing resource Kind, resource ApiVersion, resource name or the presence of disallowed resource types
func (h *EMHandler) ValidateExtraManifestConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef) (string, error) {
	var errs []string         // validation errors
	var warnings []string     // validation warnings
	var validationErr error   // returned validation err
	var validationWarn string // returned warning string

	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("the extraManifests configMap is not found, error: %v. Please create the configMap.", err)
			return validationWarn, NewEMFailedValidationError(errMsg)
		}
		return validationWarn, fmt.Errorf("failed to get configMaps to export extraManifest to dir: %w", err)
	}

	manifests, extractErrs := h.extractManifestFromConfigmaps(configmaps)
	if len(extractErrs) != 0 {
		errs = append(errs, extractErrs...)
	}

	sortedManifests, err := common.SortAndGroupByApplyWave[*unstructured.Unstructured](manifests)
	if err != nil {
		return validationWarn, fmt.Errorf("failed to sort and group manifests: %w", err)
	}

	// A namespace set saves all created namespaces for dryrun
	fakedNamespacesForDryrun := make(map[string]bool)
	// Cleanup any faked namespaces at the end of validation
	defer deleteFakedNamespacesForDryrun(ctx, h.Client, h.Log, fakedNamespacesForDryrun)

	nsManifestsSet := make(map[string]bool) // A namespace set collects appeared namespaces in configmaps
	for _, manifestGroup := range sortedManifests {
		for _, manifest := range manifestGroup {
			mKind := manifest.GetKind()
			mApiVersion := manifest.GetAPIVersion()
			mName := manifest.GetName()
			mNamespace := manifest.GetNamespace()

			// Verify resource
			if mName == "" {
				errs = append(errs, fmt.Sprintf("%s resource name is empty", mKind))
				continue
			}

			// Verify disallowed resource
			if kinds, exists := unSupportedResources[mApiVersion]; exists {
				if lo.Contains(kinds, mKind) {
					errs = append(errs, fmt.Sprintf("%s[%s] in extramanifests is not allowed", mKind, mName))
					continue
				}
			}

			// Add to namespace set when manifest is Namespace
			if mKind == "Namespace" {
				nsManifestsSet[mName] = true
				continue
			}

			// Check if manifest's namespace exists on cluster before running dryrun
			if mNamespace != "" && mNamespace != "default" {
				ns := &corev1.Namespace{}
				if err := h.Client.Get(ctx, types.NamespacedName{Name: mNamespace}, ns); err != nil {
					if !k8serrors.IsNotFound(err) {
						return validationWarn, fmt.Errorf("failed to query namespace %s: %w", ns, err)
					}

					// Manifest's namespace not found on cluster and not in configmap
					if _, exists := nsManifestsSet[mNamespace]; !exists {
						warnings = append(warnings, fmt.Sprintf("%s[%s] - namespace %s not found on cluster or not added in configmap in right order", mKind, mName, mNamespace))
						continue
					}

					// Manifest's namespace not found on cluster but in configmap, fake the namespace
					faked, err := fakeNamespaceForDryrun(ctx, h.Client, h.Log, mNamespace)
					if err != nil {
						return validationWarn, fmt.Errorf("failed to create namespace %s for dryrun: %w", ns, err)
					} else if faked {
						fakedNamespacesForDryrun[mNamespace] = true
					}
				}
			}

			// Apply manifest with dryRun
			if err := ApplyExtraManifest(ctx, h.DynamicClient, h.Client.RESTMapper(), manifest, true); err != nil {
				var groupDiscoveryErr *discovery.ErrGroupDiscoveryFailed
				switch {
				case errors.As(err, &groupDiscoveryErr), meta.IsNoMatchError(err): // Capture unknown CRD
					warnings = append(warnings, fmt.Sprintf("%s[%s] - CRD not deployed on cluster", mKind, mName))
				case k8serrors.IsInvalid(err), k8serrors.IsBadRequest(err): // Capture both invalid syntax and webhook validation errors
					warnings = append(warnings, fmt.Sprintf("%s[%s] - dryrun failed in namespace %s: %s", mKind, mName, mNamespace, err.Error()))
				default: // Capture unknown runtime error
					// nolint: staticcheck
					return validationWarn, fmt.Errorf("failed to validate manifest with dryrun: %w", err)
				}
			}
		}
	}

	if len(warnings) == 0 && len(errs) == 0 {
		h.Log.Info("ExtraManifests configmaps are validated", "configmaps", content)
		return "", nil
	}

	if len(warnings) > 0 {
		validationWarn = strings.Join(warnings, "; ")
		h.Log.Info(fmt.Sprintf("[WARNING] ExtraManifests configmaps validation: %s", validationWarn))
	}

	if len(errs) > 0 {
		validationErr = NewEMFailedValidationError(strings.Join(errs, "; "))
		h.Log.Error(validationErr, "[ERROR] ExtraManifests configmaps validation")
	}

	return validationWarn, validationErr
}

func (h *EMHandler) extractManifestFromConfigmaps(configmaps []corev1.ConfigMap) ([]*unstructured.Unstructured, []string) {
	var manifests []*unstructured.Unstructured
	var errs []string

	for _, cm := range configmaps {
		for key, value := range cm.Data {
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(value), 4096)
			for {
				resource := unstructured.Unstructured{}
				err := decoder.Decode(&resource)
				if err != nil {
					if errors.Is(err, io.EOF) {
						// Reach the end of the data, exit the loop
						break
					}
					errs = append(errs, fmt.Sprintf("failed to decode %s in the configMap %s: %v", key, cm.Name, err.Error()))
					continue
				}
				// In case it contains the UID and ResourceVersion, remove them
				resource.SetUID("")
				resource.SetResourceVersion("")

				manifests = append(manifests, &resource)
			}
		}
	}
	return manifests, errs
}

// ExportExtraManifestToDir extracts the extra manifests from configmaps
// and writes them to the given directory
func (h *EMHandler) ExportExtraManifestToDir(ctx context.Context, extraManifestCMs []ibuv1.ConfigMapRef, toDir string) error {
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

	manifests, errs := h.extractManifestFromConfigmaps(configmaps)
	if errs != nil {
		err := NewEMFailedError(strings.Join(errs, "; "))
		return fmt.Errorf("failed to extract manifests from configMap: %w", err)
	}

	sortedManifests, err := common.SortAndGroupByApplyWave[*unstructured.Unstructured](manifests)
	if err != nil {
		return fmt.Errorf("failed to sort and group manifests: %w", err)
	}

	// Create the directory for the extra manifests
	exMDirPath := filepath.Join(toDir, CmManifestPath)
	if err := os.MkdirAll(exMDirPath, 0o700); err != nil {
		return fmt.Errorf("failed to create directory for extra manifests in %s: %w", exMDirPath, err)
	}
	for i, manifestGroup := range sortedManifests {
		group := filepath.Join(toDir, CmManifestPath, "group"+strconv.Itoa(i+1))
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

// ExtractAndExportManifestFromPoliciesToDir extracts CR specs from policies and writes to a given directory. It matches policies and/or CRs by labels.
func (h *EMHandler) ExtractAndExportManifestFromPoliciesToDir(ctx context.Context, policyLabels, objectLabels, validationAnns map[string]string, toDir string) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: "policies.policy.open-cluster-management.io"}, crd); err != nil {
		if k8serrors.IsNotFound(err) {
			h.Log.Info("Skipping extraction from policies as the policy CRD is not found. This is expected if the cluster is not managed by ACM")
			return nil
		} else {
			return fmt.Errorf("error while check policy CRD: %w", err)
		}
	}

	sortedObjects, err := h.ValidateAndExtractManifestFromPolicies(ctx, policyLabels, objectLabels, validationAnns)
	if err != nil {
		return fmt.Errorf("failed to validate and extract manifests from policies: %w", err)
	}

	for i, objects := range sortedObjects {
		group := filepath.Join(toDir, PolicyManifestPath, "group"+strconv.Itoa(i+1))
		if err := os.MkdirAll(group, 0o700); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", group, err)
		}

		for j, object := range objects {
			gvk := object.GroupVersionKind()
			manifestFilePath := filepath.Join(group, fmt.Sprintf(
				"%d_%s_%s_%s.yaml", j+1, gvk.Kind, object.GetName(), object.GetNamespace()))
			if err := utils.MarshalToYamlFile(&object, manifestFilePath); err != nil { //nolint:gosec
				return fmt.Errorf("failed to save manifests to file %s: %w", manifestFilePath, err)
			}
			h.Log.Info("Extracted from policy and exported manifest to file", "path", manifestFilePath)
		}
	}

	return nil
}

func (h *EMHandler) ValidateAndExtractManifestFromPolicies(ctx context.Context, policyLabels, objectLabels, validationAnns map[string]string) ([][]*unstructured.Unstructured, error) {
	var manifestCount int
	var sortedObjects = [][]*unstructured.Unstructured{}

	sortedPolicies, err := h.GetPolicies(ctx, policyLabels)
	if err != nil {
		return sortedObjects, fmt.Errorf("failed to get policies: %w", err)
	}

	for _, policy := range sortedPolicies {
		objects, err := getConfigurationObjects(&h.Log, policy, objectLabels)
		if err != nil {
			return sortedObjects, fmt.Errorf("failed to extract manifests from policies: %w", err)
		}

		if len(objects) > 0 {
			manifestCount += len(objects)
			sortedObjects = append(sortedObjects, objects)
		}
	}

	// check the labeled manifests count
	if expectedManifestCount, exists := validationAnns[TargetOcpVersionManifestCountAnnotation]; exists {
		if expectedManifestCount != strconv.Itoa(manifestCount) {
			errMsg := fmt.Sprintf("The labeled manifests count found in policies %s does not match the expected manifests count %s", strconv.Itoa(manifestCount), expectedManifestCount)
			h.Log.Error(nil, errMsg)
			return sortedObjects, NewEMFailedError(errMsg)
		}
	}
	return sortedObjects, nil
}

// ApplyExtraManifests applies the extra manifests from the preserved extra manifests directory
func (h *EMHandler) ApplyExtraManifests(ctx context.Context, fromDir string) error {
	manifests, err := utils.LoadGroupedManifestsFromPath(fromDir, &h.Log)
	if err != nil {
		return fmt.Errorf("failed to read extra manifests from path: %w", err)
	}
	if manifests == nil || len(manifests) == 0 {
		h.Log.Info(fmt.Sprintf("No extra manifests to apply in path %s", fromDir))
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
				// nolint: staticcheck
				return fmt.Errorf("failed to apply manifest: %w", err)
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

// GetMatchingTargetOcpVersionLabelVersions generates a list of matching versions that
// be used for extracting manifests from the policy
// The matching versions include: [full release version, Major.Minor.Patch, Major.Minor]
func GetMatchingTargetOcpVersionLabelVersions(ocpVersion string) ([]string, error) {
	var validVersions []string

	semVersion, err := semver.NewVersion(ocpVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to parse target ocp version %s: %w", ocpVersion, err)
	}

	validVersions = append(validVersions,
		fmt.Sprintf("%d.%d.%d", semVersion.Major, semVersion.Minor, semVersion.Patch),
		fmt.Sprintf("%d.%d", semVersion.Major, semVersion.Minor),
	)
	if !lo.Contains(validVersions, ocpVersion) {
		// full release version
		validVersions = append(validVersions, ocpVersion)
	}
	return validVersions, nil
}

// AddAnnotationEMWarningValidation add warning in annotation for when CRD is unknown
func AddAnnotationEMWarningValidation(c client.Client, log logr.Logger, ibu *ibuv1.ImageBasedUpgrade, annotationValue string) error {
	patch := client.MergeFrom(ibu.DeepCopy()) // a merge patch will preserve other fields modified at runtime.

	ann := ibu.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[ValidationWarningAnnotation] = annotationValue
	ibu.SetAnnotations(ann)

	if err := c.Patch(context.Background(), ibu, patch); err != nil {
		return fmt.Errorf("failed to update annotations: %w", err)
	}

	log.Info("Successfully added annotation", "annotation", ValidationWarningAnnotation)
	return nil
}

// RemoveAnnotationEMWarningValidation remove the warning if present
func RemoveAnnotationEMWarningValidation(c client.Client, log logr.Logger, ibu *ibuv1.ImageBasedUpgrade) error {
	if _, exists := ibu.GetAnnotations()[ValidationWarningAnnotation]; exists {
		patch := client.MergeFrom(ibu.DeepCopy()) // a merge patch will preserve other fields modified at runtime.

		ann := ibu.GetAnnotations()
		delete(ann, ValidationWarningAnnotation)
		ibu.SetAnnotations(ann)

		if err := c.Patch(context.Background(), ibu, patch); err != nil {
			return fmt.Errorf("failed to update annotations: %w", err)
		}
		log.Info("Successfully removed annotation", "annotations", ValidationWarningAnnotation)
	}

	return nil
}
