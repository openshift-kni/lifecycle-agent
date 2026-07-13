/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/lcenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backuprestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=delete

const (
	applyLabelAnn = "lca.openshift.io/apply-label"
	backupLabel   = "lca.openshift.io/backup"

	LocalBackupPath = "/opt/lca-backups"

	topolvmValue                   = "topolvm.io"
	topolvmAnnotation              = "pv.kubernetes.io/provisioned-by"
	updatedReclaimPolicyAnnotation = "lca.openshift.io/updated-reclaim-policy"
)

var (
	hostPath = common.Host
)

// BackuperRestorer interface also used for mocks
type BackuperRestorer interface {
	ValidateBackupConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef) error
	StartBackup(ctx context.Context, content []ibuv1.ConfigMapRef, targetDir string) (*BackupTracker, error)
	StartRestore(ctx context.Context) (*RestoreTracker, error)
	CleanupBackups(ctx context.Context) error
	PatchPVsReclaimPolicy(ctx context.Context) error
	RestorePVsReclaimPolicy(ctx context.Context) error
}

// BRHandler handles the backup and restore
type BRHandler struct {
	client.Client
	DynamicClient dynamic.Interface
	Log           logr.Logger
}

// BRStatusError type
type BRStatusError struct {
	Type       string
	Reason     string
	ErrMessage string
}

type ObjMetadata struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
}

// BackupSpec holds the parsed fields from a Velero Backup CR spec in a configmap
type BackupSpec struct {
	Name                             string
	Namespace                        string
	ApplyLabel                       string
	ApplyWave                        string
	IncludedNamespaces               []string
	IncludedNamespaceScopedResources []string
	ExcludedResources                []string
	IncludedClusterScopedResources   []string
	LabelSelector                    *metav1.LabelSelector
}

type BackupTracker struct {
	SucceededBackups []string
	FailedBackups    []string
}

type RestoreTracker struct {
	SucceededRestores []string
	FailedRestores    []string
}

func (e *BRStatusError) Error() string {
	return e.ErrMessage
}

func NewBRFailedError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "Failed",
		ErrMessage: msg,
	}
}

func NewBRFailedValidationError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "FailedValidation",
		ErrMessage: msg,
	}
}

func IsBRFailedError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" {
			return brErr.Reason == "Failed"
		}
	}
	return false
}

func IsBRFailedValidationError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" {
			return brErr.Reason == "FailedValidation"
		}
	}
	return false
}

func CreateOrUpdateSecret(ctx context.Context, secret *corev1.Secret, c client.Client) error {
	existingSecret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      secret.Name,
		Namespace: secret.Namespace,
	}, existingSecret)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get secret: %w", err)
		}
		if err := c.Create(ctx, secret); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create secret: %w", err)
			}
		}
	} else {
		secret.SetResourceVersion(existingSecret.GetResourceVersion())
		if err := c.Update(ctx, secret); err != nil {
			return fmt.Errorf("failed to update secret: %w", err)
		}
	}
	return nil
}

// patchObj patches a Kubernetes resource using the dynamic client.
func patchObj(ctx context.Context, dynClient dynamic.Interface, obj *ObjMetadata,
	isDryRun bool, payload []byte, patchType types.PatchType) error {
	var err error
	gvr := obj.GroupVersionResource()
	resourceClient := dynClient.Resource(gvr)

	patchOptions := metav1.PatchOptions{}
	if isDryRun {
		patchOptions = metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	}

	if obj.Namespace != "" {
		_, err = resourceClient.Namespace(obj.Namespace).Patch(
			ctx, obj.Name, patchType, payload, patchOptions,
		)
	} else {
		_, err = resourceClient.Patch(ctx, obj.Name, patchType, payload, patchOptions)
	}
	if err != nil {
		return fmt.Errorf("failed to patch object: %w", err)
	}

	return nil
}

func (o *ObjMetadata) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    o.Group,
		Version:  o.Version,
		Resource: o.Resource,
	}
}

// ExtractBackupSpecsFromConfigmaps parses Velero Backup CR specs from configmaps
// into internal BackupSpec structs without depending on the Velero Go types.
func ExtractBackupSpecsFromConfigmaps(configmaps []corev1.ConfigMap) ([]BackupSpec, error) {
	var specs []BackupSpec
	for _, cm := range configmaps {
		for _, value := range cm.Data {
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(value), 4096)
			for {
				resource := unstructured.Unstructured{}
				if err := decoder.Decode(&resource); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return nil, fmt.Errorf("failed to decode configmap data: %w", err)
				}

				if resource.GroupVersionKind() != common.BackupGvk {
					continue
				}

				spec, err := backupSpecFromUnstructured(&resource)
				if err != nil {
					return nil, err
				}
				specs = append(specs, spec)
			}
		}
	}
	return specs, nil
}

func backupSpecFromUnstructured(u *unstructured.Unstructured) (BackupSpec, error) {
	spec := BackupSpec{
		Name:      u.GetName(),
		Namespace: u.GetNamespace(),
	}

	if ann := u.GetAnnotations(); ann != nil {
		spec.ApplyLabel = ann[applyLabelAnn]
		spec.ApplyWave = ann[common.ApplyWaveAnn]
	}

	specMap, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		return spec, nil
	}

	spec.IncludedNamespaces = getStringSlice(specMap, "includedNamespaces")
	spec.IncludedNamespaceScopedResources = getStringSlice(specMap, "includedNamespaceScopedResources")
	spec.ExcludedResources = getStringSlice(specMap, "excludedResources")
	spec.IncludedClusterScopedResources = getStringSlice(specMap, "includedClusterScopedResources")

	if lsMap, ok := specMap["labelSelector"].(map[string]interface{}); ok {
		selector := &metav1.LabelSelector{}
		if ml, ok := lsMap["matchLabels"].(map[string]interface{}); ok {
			selector.MatchLabels = make(map[string]string)
			for k, v := range ml {
				selector.MatchLabels[k] = fmt.Sprintf("%v", v)
			}
		}
		spec.LabelSelector = selector
	}

	return spec, nil
}

func getStringSlice(m map[string]interface{}, key string) []string {
	val, ok := m[key]
	if !ok {
		return nil
	}
	arr, ok := val.([]interface{})
	if !ok {
		return nil
	}
	var result []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// SortBackupSpecsByApplyWave groups and sorts backup specs by their apply-wave annotation
func SortBackupSpecsByApplyWave(specs []BackupSpec) ([][]BackupSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	type waveSpec struct {
		wave int
		spec BackupSpec
	}

	var items []waveSpec
	for _, s := range specs {
		wave := 0
		if s.ApplyWave != "" {
			var err error
			wave, err = strconv.Atoi(s.ApplyWave)
			if err != nil {
				return nil, fmt.Errorf("invalid apply-wave value %q: %w", s.ApplyWave, err)
			}
		}
		items = append(items, waveSpec{wave: wave, spec: s})
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].wave != items[j].wave {
			return items[i].wave < items[j].wave
		}
		return items[i].spec.Name < items[j].spec.Name
	})

	var groups [][]BackupSpec
	prevWave := -1
	for _, item := range items {
		if item.wave != prevWave {
			groups = append(groups, []BackupSpec{})
			prevWave = item.wave
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], item.spec)
	}

	return groups, nil
}

func getObjsFromApplyLabel(applyLabel string) ([]ObjMetadata, error) {
	if applyLabel == "" {
		return nil, nil
	}

	var result []ObjMetadata
	objStrings := common.RemoveDuplicates[string](strings.Split(applyLabel, ","))
	for _, objString := range objStrings {
		parts := strings.Split(objString, "/")
		if len(parts) < 3 || len(parts) > 5 {
			return result, fmt.Errorf("invalid apply-label value: %s", objString)
		}
		var obj ObjMetadata
		switch len(parts) {
		case 3:
			obj = ObjMetadata{Version: parts[0], Resource: parts[1], Name: parts[2]}
		case 4:
			if parts[0] == "v1" {
				obj = ObjMetadata{
					Version: parts[0], Resource: parts[1],
					Namespace: parts[2], Name: parts[3],
				}
			} else {
				obj = ObjMetadata{
					Group: parts[0], Version: parts[1],
					Resource: parts[2], Name: parts[3],
				}
			}
		case 5:
			obj = ObjMetadata{
				Group: parts[0], Version: parts[1],
				Resource: parts[2], Namespace: parts[3], Name: parts[4],
			}
		default:
			return result, fmt.Errorf("invalid apply-label value: %s", objString)
		}
		result = append(result, obj)
	}
	return result, nil
}
