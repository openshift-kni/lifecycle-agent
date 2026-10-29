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
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8syaml "sigs.k8s.io/yaml"
)

// The local restore path recreates arbitrary resources across all API groups, so
// broad group/resource coverage is required. The verbs are restricted to the
// standard CRUD set (dropping the sensitive wildcard verbs impersonate and
// deletecollection, which are unused) to follow least privilege. escalate and
// bind are granted only for RBAC roles/clusterroles, which restoring those
// objects legitimately requires.
// +kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;clusterroles,verbs=escalate;bind

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
	StartBackup(ctx context.Context, content []ibuv1.ConfigMapRef) (*BackupTracker, error)
	StartRestore(ctx context.Context, content []ibuv1.ConfigMapRef) (*RestoreTracker, error)
	CleanupBackups(ctx context.Context) error
	PatchPVsReclaimPolicy(ctx context.Context) error
	RestorePVsReclaimPolicy(ctx context.Context) error
}

// BRHandler handles the backup and restore
type BRHandler struct {
	client.Client
	DynamicClient dynamic.Interface
	// DiscoveryClient is used to discover the server's preferred API resources
	// at backup time, so that CRDs installed after the controller started (e.g.
	// lvmclusters) are visible and resources configured by name only can be
	// resolved to their preferred GVR.
	DiscoveryClient discovery.DiscoveryInterface
	Log             logr.Logger
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
	// ExistingResourcePolicy mirrors the Velero Restore CR field. LCA does not
	// implement it; a non-empty value is rejected by ValidateRestoreSpecs.
	ExistingResourcePolicy string
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
	// The legacy Velero spec.includedResources and spec.excludedResources fields
	// are intentionally not modeled here: LCA ignores them. Use the scope-specific
	// included/excluded fields below instead.
	IncludedNamespaces               []string              `json:"includedNamespaces"`
	IncludedNamespaceScopedResources []string              `json:"includedNamespaceScopedResources"`
	IncludedClusterScopedResources   []string              `json:"includedClusterScopedResources"`
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
	// ExistingResourcePolicy is the Velero field that selects how existing
	// resources are handled during restore. LCA does not implement it (it always
	// creates-or-updates), so a non-empty value is rejected during validation
	// rather than silently ignored. See ValidateRestoreSpecs.
	ExistingResourcePolicy string `json:"existingResourcePolicy"`
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

// ValidateBackupRestorePairing enforces a strict 1:1 pairing between Backup and
// Restore CRs across the referenced oadpContent ConfigMaps: every Backup must be
// named by exactly one Restore's spec.backupName and every Restore must name an
// existing Backup. The matching Backup and Restore may live in different
// referenced ConfigMaps (as was valid before this change). Orphan Backup or
// Restore documents are rejected, and Backup and Restore names must be unique
// across all referenced ConfigMaps. This runs at Prep and is re-checked in
// StartBackup in case the ConfigMaps changed after Prep.
func ValidateBackupRestorePairing(configmaps []corev1.ConfigMap) error {
	seenBackups := make(map[string]string)  // backup name -> configmap ref
	seenRestores := make(map[string]string) // restore name -> configmap ref

	type restoreRef struct {
		name       string
		cmRef      string
		backupName string
	}
	var allRestores []restoreRef

	// First pass: collect every Backup and Restore across all ConfigMaps so the
	// pairing can be validated globally rather than per-ConfigMap.
	for _, cm := range configmaps {
		cmRef := configmapRef(cm)
		single := []corev1.ConfigMap{cm}

		backups, err := ExtractBackupSpecsFromConfigmaps(single)
		if err != nil {
			return err
		}
		restores, err := ExtractRestoreSpecsFromConfigmaps(single)
		if err != nil {
			return err
		}

		for _, b := range backups {
			if b.Name == "" {
				return fmt.Errorf("configmap %s contains a Backup CR with no metadata.name", cmRef)
			}
			if prev, ok := seenBackups[b.Name]; ok {
				return fmt.Errorf("duplicate Backup name %q found in configmaps %s and %s", b.Name, prev, cmRef)
			}
			seenBackups[b.Name] = cmRef
		}

		for _, r := range restores {
			if r.Name == "" {
				return fmt.Errorf("configmap %s contains a Restore CR with no metadata.name", cmRef)
			}
			if prev, ok := seenRestores[r.Name]; ok {
				return fmt.Errorf("duplicate Restore name %q found in configmaps %s and %s", r.Name, prev, cmRef)
			}
			seenRestores[r.Name] = cmRef

			if r.BackupName == "" {
				return fmt.Errorf("restore %q in configmap %s has an empty spec.backupName", r.Name, cmRef)
			}
			allRestores = append(allRestores, restoreRef{name: r.Name, cmRef: cmRef, backupName: r.BackupName})
		}
	}

	// Every Restore must name an existing Backup. The Backup may be defined in a
	// different referenced ConfigMap than the Restore.
	restoreCountByBackup := make(map[string]int, len(seenBackups))
	for _, r := range allRestores {
		if _, ok := seenBackups[r.backupName]; !ok {
			return fmt.Errorf("restore %q in configmap %s references backup %q which is not defined in any referenced configmap", r.name, r.cmRef, r.backupName)
		}
		restoreCountByBackup[r.backupName]++
	}

	// Every Backup must be named by exactly one Restore.
	for name, cmRef := range seenBackups {
		switch restoreCountByBackup[name] {
		case 1:
			// exactly one matching Restore — the required pairing
		case 0:
			return fmt.Errorf("backup %q in configmap %s has no matching Restore CR", name, cmRef)
		default:
			return fmt.Errorf("backup %q in configmap %s is referenced by %d Restore CRs; exactly one is required", name, cmRef, restoreCountByBackup[name])
		}
	}
	return nil
}

// configmapRef renders a configmap reference as namespace/name for error messages.
func configmapRef(cm corev1.ConfigMap) string {
	if cm.Namespace != "" {
		return fmt.Sprintf("%s/%s", cm.Namespace, cm.Name)
	}
	return cm.Name
}

// ValidateBackupSpecs rejects Backup CR specs that use fields LCA cannot honor or
// whose resource selection would be empty or ineffective, so an unsupported or
// misconfigured backup fails loudly instead of silently backing up the wrong or an
// empty set of resources.
func ValidateBackupSpecs(specs []BackupSpec) error {
	for _, s := range specs {
		// includedNamespaceScopedResources only takes effect within included
		// namespaces (namespace-scoped resources are listed per included namespace).
		// Setting it without any included namespace selects nothing, so reject it as
		// an ineffective selection.
		if len(s.IncludedNamespaceScopedResources) > 0 && len(s.IncludedNamespaces) == 0 {
			return fmt.Errorf("backup %q sets spec.includedNamespaceScopedResources without spec.includedNamespaces: "+
				"namespace-scoped resources are only backed up within included namespaces", s.Name)
		}

		// When namespaces are included without the apply-label annotation, the
		// namespace-scoped resource types to back up must be listed explicitly: LCA
		// does not fall back to a built-in default list. (With the apply-label
		// annotation the backup scope is limited to the annotated objects, so the
		// resource list is not required.)
		if s.ApplyLabel == "" && len(s.IncludedNamespaces) > 0 && len(s.IncludedNamespaceScopedResources) == 0 {
			return fmt.Errorf("backup %q sets spec.includedNamespaces without spec.includedNamespaceScopedResources: "+
				"list the namespace-scoped resource types to back up", s.Name)
		}

		// A backup that selects nothing at all — no apply-label annotation, no
		// included namespace, and no included cluster-scoped resource — issues no
		// resource queries. Reject it instead of recording an empty backup as
		// successful. Cluster-only and apply-label-only backups remain valid.
		if s.ApplyLabel == "" && len(s.IncludedNamespaces) == 0 && len(s.IncludedClusterScopedResources) == 0 {
			return fmt.Errorf("backup %q selects no resources: set the %s annotation, spec.includedNamespaces "+
				"(with spec.includedNamespaceScopedResources), or spec.includedClusterScopedResources", s.Name, applyLabelAnn)
		}
	}
	return nil
}

// ValidateRestoreSpecs rejects Restore CR fields LCA does not implement so an
// unsupported configuration fails loudly instead of being silently ignored.
func ValidateRestoreSpecs(specs []RestoreSpec) error {
	for _, s := range specs {
		if s.ExistingResourcePolicy != "" {
			return fmt.Errorf("restore %q sets spec.existingResourcePolicy=%q which LCA does not implement; "+
				"LCA always creates or updates restored resources regardless of whether they already exist. "+
				"Remove the field", s.Name, s.ExistingResourcePolicy)
		}
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
		Name:                   r.Metadata.Name,
		Namespace:              r.Metadata.Namespace,
		ApplyWave:              r.Metadata.Annotations[common.ApplyWaveAnn],
		BackupName:             r.Spec.BackupName,
		RestorePVs:             r.Spec.RestorePVs,
		ExistingResourcePolicy: r.Spec.ExistingResourcePolicy,
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
		// The legacy Velero spec.includedResources and spec.excludedResources
		// fields are intentionally ignored; LCA uses only the scope-specific
		// included/excluded fields.
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

	// The first item always starts a new group; subsequent items start a new group
	// only when their wave differs from the preceding item. Comparing against the
	// previous item (rather than a sentinel) avoids an index-out-of-range panic when
	// the lowest wave happens to equal a sentinel value such as a negative apply-wave.
	var groups [][]BackupSpec
	for i, item := range items {
		if i == 0 || item.wave != items[i-1].wave {
			groups = append(groups, []BackupSpec{})
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
