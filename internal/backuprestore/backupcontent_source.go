/*
Copyright 2026.

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

	"github.com/go-logr/logr"
	backupcontentv1 "github.com/openshift-kni/lifecycle-agent/api/backupcontent/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// This file is the BackupContent adapter onto the source-independent selection
// model and validator (selection.go). It is the only part of the validation layer
// that references BackupContentSpec, so the follow-up ConfigMap/Velero source can
// reuse every rule by supplying its own adapter.

// BackupContentValidationWarningAnnotation records the non-terminal BackupContent
// validation findings from Prep on the IBU, mirroring
// extramanifest.ValidationWarningAnnotation. prepSuccessDoNotRequeue surfaces it
// in the PrepCompleted message so a warning is visible without failing Prep.
const BackupContentValidationWarningAnnotation = "backup-content.lca.openshift.io/validation-warning"

// backupContentToSelectionModel maps a BackupContent CR into the source-independent
// SelectionModel: the namespaced/cluster-scoped included and excluded lists
// collapse into tagged ResourceEntry slices, and the top-level labelSelector is
// carried as-is (the validator applies it only to unnamed includes).
func backupContentToSelectionModel(bc *backupcontentv1.BackupContent) SelectionModel {
	spec := bc.Spec
	m := SelectionModel{
		SourceName:    bc.Name,
		Namespaces:    spec.Namespaces,
		LabelSelector: spec.LabelSelector,
	}
	for _, e := range spec.NamespacedResources {
		m.Included = append(m.Included, ResourceEntry{Kind: e.Kind, Names: e.Names, Scope: ScopeNamespaced})
	}
	for _, e := range spec.ClusterScopedResources {
		m.Included = append(m.Included, ResourceEntry{Kind: e.Kind, Names: e.Names, Scope: ScopeClusterScoped})
	}
	for _, e := range spec.ExcludedNamespacedResources {
		m.Excluded = append(m.Excluded, ResourceEntry{Kind: e.Kind, Names: e.Names, Scope: ScopeNamespaced})
	}
	for _, e := range spec.ExcludedClusterScopedResources {
		m.Excluded = append(m.Excluded, ResourceEntry{Kind: e.Kind, Names: e.Names, Scope: ScopeClusterScoped})
	}
	return m
}

// resolveBackupContentModels resolves the name-only BackupContent references in
// LCA's install namespace and maps each resolved CR to a SelectionModel (BC-1).
// A reference that does not resolve is collected as a terminal finding rather than
// aborting, so Prep can report every missing reference in one pass; a non-NotFound
// read error is returned as a transient (retryable) error.
func (h *BRHandler) resolveBackupContentModels(ctx context.Context, refs []ibuv1.BackupContentRef) ([]SelectionModel, []Finding, error) {
	namespace := common.OperatorNamespace()
	models := make([]SelectionModel, 0, len(refs))
	var findings []Finding
	for _, ref := range refs {
		bc := &backupcontentv1.BackupContent{}
		if err := h.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, bc); err != nil {
			if k8serrors.IsNotFound(err) {
				findings = append(findings, terminalFinding(ref.Name, "backupContent",
					fmt.Sprintf("BackupContent %q not found in namespace %q", ref.Name, namespace)))
				continue
			}
			// Return the findings collected so far alongside the error: a missing
			// reference already found is a terminal finding the caller must still
			// report even though this read failed transiently.
			return models, findings, fmt.Errorf("failed to get BackupContent %q in namespace %q: %w", ref.Name, namespace, err)
		}
		models = append(models, backupContentToSelectionModel(bc))
	}
	return models, findings, nil
}

// ValidateBackupContent validates the configured spec.backupContent during Prep
// (read-only). It resolves the referenced CRs (BC-1), runs every shared rule
// against their selection models in one pass, and splits the result: terminal
// findings become a single aggregated BRFailedValidationError (so Prep fails
// terminally, no requeue), warning findings are returned as strings (recorded on
// the IBU and surfaced in PrepCompleted), and a transient discovery/API failure
// is returned as a plain error so Prep requeues instead of failing.
func (h *BRHandler) ValidateBackupContent(ctx context.Context, backupContent []ibuv1.BackupContentRef) ([]string, error) {
	// Collect findings from both passes before acting on any transient error: a
	// terminal configuration finding (e.g. a missing reference) must fail Prep
	// terminally even if a later lookup failed transiently, since requeuing would
	// never make a known-bad configuration valid.
	models, bc1Findings, resolveErr := h.resolveBackupContentModels(ctx, backupContent)

	ruleFindings, ruleErr := h.ValidateSelectionModels(ctx, models, StagePrep)

	all := make([]Finding, 0, len(bc1Findings)+len(ruleFindings))
	all = append(all, bc1Findings...)
	all = append(all, ruleFindings...)
	terminal, warnings := splitFindings(all)

	if len(terminal) > 0 {
		h.Log.Info("BackupContent validation failed", "terminalFindings", findingStrings(terminal))
		return findingStrings(warnings), NewBRFailedValidationError("Backup", findingsMessage(terminal))
	}

	// No terminal finding is known. If a lookup failed transiently, propagate it so
	// Prep requeues rather than reporting an incomplete validation as successful.
	if resolveErr != nil {
		return nil, resolveErr
	}
	if ruleErr != nil {
		return nil, ruleErr
	}

	for _, w := range warnings {
		h.Log.Info("WARNING: BackupContent validation", "finding", w.String())
	}
	h.Log.Info("BackupContent validated", "backupContent", backupContent, "warnings", len(warnings))
	return findingStrings(warnings), nil
}

// AddAnnotationBackupContentWarningValidation records the Prep BackupContent
// validation warnings on the IBU via a merge patch, mirroring
// extramanifest.AddAnnotationEMWarningValidation so other runtime changes are
// preserved.
func AddAnnotationBackupContentWarningValidation(c client.Client, log logr.Logger, ibu *ibuv1.ImageBasedUpgrade, annotationValue string) error {
	patch := client.MergeFrom(ibu.DeepCopy())

	ann := ibu.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[BackupContentValidationWarningAnnotation] = annotationValue
	ibu.SetAnnotations(ann)

	if err := c.Patch(context.Background(), ibu, patch); err != nil {
		return fmt.Errorf("failed to update annotations: %w", err)
	}

	log.Info("Successfully added annotation", "annotation", BackupContentValidationWarningAnnotation)
	return nil
}

// RemoveAnnotationBackupContentWarningValidation removes the BackupContent warning
// annotation if present, mirroring extramanifest.RemoveAnnotationEMWarningValidation.
// It is called on the transition back to Idle so a stale warning from a prior Prep
// is not re-surfaced after the underlying misconfiguration has been fixed.
func RemoveAnnotationBackupContentWarningValidation(c client.Client, log logr.Logger, ibu *ibuv1.ImageBasedUpgrade) error {
	if _, exists := ibu.GetAnnotations()[BackupContentValidationWarningAnnotation]; exists {
		patch := client.MergeFrom(ibu.DeepCopy()) // a merge patch will preserve other fields modified at runtime.

		ann := ibu.GetAnnotations()
		delete(ann, BackupContentValidationWarningAnnotation)
		ibu.SetAnnotations(ann)

		if err := c.Patch(context.Background(), ibu, patch); err != nil {
			return fmt.Errorf("failed to update annotations: %w", err)
		}
		log.Info("Successfully removed annotation", "annotation", BackupContentValidationWarningAnnotation)
	}
	return nil
}
