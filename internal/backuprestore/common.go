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
	"bufio"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8syaml "sigs.k8s.io/yaml"
)

// +kubebuilder:rbac:groups="*",resources="*",verbs="*"

const (
	applyLabelAnn = "lca.openshift.io/apply-label"

	LocalBackupPath = "/var/lib/containers/lca-backups"

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
	IncludedClusterScopedResources   []string
	ExcludedResources                []string
	ExcludedNamespaceScopedResources []string
	ExcludedClusterScopedResources   []string
	LabelSelector                    *metav1.LabelSelector
}

// RestoreSpec holds the parsed fields from a Velero Restore CR spec in a configmap
type RestoreSpec struct {
	Name                   string
	Namespace              string
	BackupName             string
	ApplyWave              string
	RestorePVs             bool
	RestoreStatusResources []string
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

func (o *ObjMetadata) GroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    o.Group,
		Version:  o.Version,
		Resource: o.Resource,
	}
}

// veleroTypeMeta captures the identifying fields of a document so it can be
// filtered by apiVersion/kind before it is fully decoded.
type veleroTypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// veleroObjectMeta models only the metadata fields LCA reads from a Velero CR.
type veleroObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Annotations map[string]string `json:"annotations"`
}

// veleroBackup models the subset of a Velero Backup CR that LCA reads. It
// deliberately avoids importing the Velero API types; any unknown fields in the
// source document are ignored during decoding.
type veleroBackup struct {
	Metadata veleroObjectMeta `json:"metadata"`
	Spec     veleroBackupSpec `json:"spec"`
}

type veleroBackupSpec struct {
	IncludedNamespaces               []string              `json:"includedNamespaces"`
	IncludedNamespaceScopedResources []string              `json:"includedNamespaceScopedResources"`
	IncludedClusterScopedResources   []string              `json:"includedClusterScopedResources"`
	ExcludedResources                []string              `json:"excludedResources"`
	ExcludedNamespaceScopedResources []string              `json:"excludedNamespaceScopedResources"`
	ExcludedClusterScopedResources   []string              `json:"excludedClusterScopedResources"`
	LabelSelector                    *metav1.LabelSelector `json:"labelSelector"`
}

// veleroRestore models the subset of a Velero Restore CR that LCA reads.
type veleroRestore struct {
	Metadata veleroObjectMeta  `json:"metadata"`
	Spec     veleroRestoreSpec `json:"spec"`
}

type veleroRestoreSpec struct {
	BackupName    string               `json:"backupName"`
	RestorePVs    bool                 `json:"restorePVs"`
	RestoreStatus *veleroRestoreStatus `json:"restoreStatus"`
}

type veleroRestoreStatus struct {
	IncludedResources []string `json:"includedResources"`
}

// forEachVeleroDoc iterates over every YAML/JSON document in a configmap value,
// invoking handle with the raw bytes of each document whose apiVersion and kind
// match the given GVK. Non-matching documents are ignored; malformed documents
// produce a clear decode error.
func forEachVeleroDoc(value string, gvk schema.GroupVersionKind, handle func(raw []byte) error) error {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(value)))
	for {
		raw, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("failed to read configmap data: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var typeMeta veleroTypeMeta
		if err := k8syaml.Unmarshal(raw, &typeMeta); err != nil {
			return fmt.Errorf("failed to decode configmap data: %w", err)
		}
		if typeMeta.APIVersion != gvk.GroupVersion().String() || typeMeta.Kind != gvk.Kind {
			continue
		}

		if err := handle(raw); err != nil {
			return err
		}
	}
	return nil
}

// ExtractBackupSpecsFromConfigmaps parses Velero Backup CR specs from configmaps
// into internal BackupSpec structs without depending on the Velero Go types.
func ExtractBackupSpecsFromConfigmaps(configmaps []corev1.ConfigMap) ([]BackupSpec, error) {
	var specs []BackupSpec
	for _, cm := range configmaps {
		for _, value := range cm.Data {
			err := forEachVeleroDoc(value, common.BackupGvk, func(raw []byte) error {
				var backup veleroBackup
				if err := k8syaml.Unmarshal(raw, &backup); err != nil {
					return fmt.Errorf("failed to decode Velero Backup CR: %w", err)
				}
				specs = append(specs, backup.toBackupSpec())
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return specs, nil
}

// ExtractRestoreSpecsFromConfigmaps parses Velero Restore CR specs from configmaps
func ExtractRestoreSpecsFromConfigmaps(configmaps []corev1.ConfigMap) ([]RestoreSpec, error) {
	var specs []RestoreSpec
	for _, cm := range configmaps {
		for _, value := range cm.Data {
			err := forEachVeleroDoc(value, common.RestoreGvk, func(raw []byte) error {
				var restore veleroRestore
				if err := k8syaml.Unmarshal(raw, &restore); err != nil {
					return fmt.Errorf("failed to decode Velero Restore CR: %w", err)
				}
				specs = append(specs, restore.toRestoreSpec())
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return specs, nil
}

// ValidateBackupRestoreMapping validates that each Backup CR is referenced by at most one Restore CR.
func ValidateBackupRestoreMapping(restoreSpecs []RestoreSpec) error {
	seen := make(map[string]string)
	for _, rs := range restoreSpecs {
		if rs.BackupName == "" {
			continue
		}
		if existing, ok := seen[rs.BackupName]; ok {
			return fmt.Errorf("backup %q is referenced by multiple Restore CRs: %q and %q", rs.BackupName, existing, rs.Name)
		}
		seen[rs.BackupName] = rs.Name
	}
	return nil
}

// FindRestoreForBackup returns the RestoreSpec matching a backup name, or nil if none
func FindRestoreForBackup(backupName string, restoreSpecs []RestoreSpec) *RestoreSpec {
	for i := range restoreSpecs {
		if restoreSpecs[i].BackupName == backupName {
			return &restoreSpecs[i]
		}
	}
	return nil
}

// toRestoreSpec converts the decoded Velero Restore document into the internal
// RestoreSpec, preserving the previous parsing semantics and defaults.
func (r *veleroRestore) toRestoreSpec() RestoreSpec {
	spec := RestoreSpec{
		Name:       r.Metadata.Name,
		Namespace:  r.Metadata.Namespace,
		ApplyWave:  r.Metadata.Annotations[common.ApplyWaveAnn],
		BackupName: r.Spec.BackupName,
		RestorePVs: r.Spec.RestorePVs,
	}
	if r.Spec.RestoreStatus != nil {
		spec.RestoreStatusResources = r.Spec.RestoreStatus.IncludedResources
	}
	return spec
}

// toBackupSpec converts the decoded Velero Backup document into the internal
// BackupSpec, preserving the previous parsing semantics and defaults.
func (b *veleroBackup) toBackupSpec() BackupSpec {
	return BackupSpec{
		Name:                             b.Metadata.Name,
		Namespace:                        b.Metadata.Namespace,
		ApplyLabel:                       b.Metadata.Annotations[applyLabelAnn],
		ApplyWave:                        b.Metadata.Annotations[common.ApplyWaveAnn],
		IncludedNamespaces:               b.Spec.IncludedNamespaces,
		IncludedNamespaceScopedResources: b.Spec.IncludedNamespaceScopedResources,
		IncludedClusterScopedResources:   b.Spec.IncludedClusterScopedResources,
		// excludedResources is the legacy Velero filter that applies to both
		// namespace- and cluster-scoped resources; the *Scoped variants match the
		// newer Velero resource-policy API and apply to their respective scope.
		ExcludedResources:                b.Spec.ExcludedResources,
		ExcludedNamespaceScopedResources: b.Spec.ExcludedNamespaceScopedResources,
		ExcludedClusterScopedResources:   b.Spec.ExcludedClusterScopedResources,
		LabelSelector:                    b.Spec.LabelSelector,
	}
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
