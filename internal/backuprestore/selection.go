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
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// This file holds the source-independent backup-selection model and the
// stage-aware validator shared by Prep and Upgrade pre-pivot.
//
// The validator deliberately never references BackupContentSpec: it operates on
// SelectionModel so the deprecated spec.oadpContent path (and any future source)
// can reuse every rule by supplying its own adapter. The BackupContent adapter
// lives in backupcontent_source.go.

// ResourceScope classifies a resource entry's scope. Modelling scope explicitly
// collapses a source's namespaced/cluster-scoped (and excluded) lists into a
// single typed list each, and lets the included/excluded scope-match rules confirm
// the declared scope against discovery with one shared check.
type ResourceScope int

const (
	// ScopeNamespaced marks an entry declared as namespace-scoped.
	ScopeNamespaced ResourceScope = iota
	// ScopeClusterScoped marks an entry declared as cluster-scoped.
	ScopeClusterScoped
)

// namespacesResource is the core v1 resource name for Namespace objects, used for
// the declared-namespace existence check and when capturing namespaces.
const namespacesResource = "namespaces"

// ValidationStage identifies which IBU stage is running the validation. It drives
// the stage-specific severity of the existence and non-empty-selection rules
// (missing declared namespaces, missing named objects, and a structurally empty
// selection are a Prep warning but an Upgrade terminal failure). Every other rule
// is terminal in both stages.
type ValidationStage int

const (
	// StagePrep is the read-only validation run during Prep. Missing selected
	// namespaces/objects and structurally-empty selections are warnings here.
	StagePrep ValidationStage = iota
	// StageUpgrade is the re-validation run from freshly read CRs at pre-pivot.
	// Every finding is terminal here.
	StageUpgrade
)

// Severity classifies a Finding. Terminal findings fail the stage; warnings are
// reported but let Prep continue.
type Severity int

const (
	// SeverityTerminal fails the stage (Prep or Upgrade pre-pivot).
	SeverityTerminal Severity = iota
	// SeverityWarning is reported but does not fail Prep.
	SeverityWarning
)

// ResourceEntry is one included or excluded resource selector in the
// source-independent model. Kind may be bare ("configmaps") or group-qualified
// ("deployments.apps"); Names optionally restricts the entry to specific objects.
type ResourceEntry struct {
	Kind  string
	Names []string
	Scope ResourceScope
}

// SelectionModel is the source-independent representation of a single backup
// source's selection. A source adapter (e.g. backupContentToSelectionModel)
// populates it; the validator consumes only this type.
type SelectionModel struct {
	// SourceName identifies the source for findings and logs (e.g. the
	// BackupContent CR name).
	SourceName string
	// Namespaces is the set of namespaces in scope for the namespaced entries.
	Namespaces []string
	// Included and Excluded carry the included and excluded resource entries,
	// each tagged with its declared scope.
	Included []ResourceEntry
	Excluded []ResourceEntry
	// LabelSelector applies only to unnamed included entries and never to the
	// excluded lists.
	LabelSelector *metav1.LabelSelector
}

// Finding is a single validation result. Source/Field/Cause identify the source,
// the offending field or reference, and the cause (per the error-reporting
// requirement that every entry be self-describing).
type Finding struct {
	Source   string
	Field    string
	Cause    string
	Severity Severity
}

// String renders a finding for a condition message or log line.
func (f Finding) String() string {
	var b strings.Builder
	if f.Source != "" {
		fmt.Fprintf(&b, "source %q", f.Source)
	}
	if f.Field != "" {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(f.Field)
	}
	if b.Len() > 0 {
		b.WriteString(": ")
	}
	b.WriteString(f.Cause)
	return b.String()
}

// terminalFinding builds a Terminal finding (fails both stages).
func terminalFinding(source, field, cause string) Finding {
	return Finding{Source: source, Field: field, Cause: cause, Severity: SeverityTerminal}
}

// stageFinding builds a finding whose severity depends on the stage: a Prep
// warning but an Upgrade terminal failure. Used by the stage-dependent rules:
// declared-namespace existence, named-object existence, and the non-empty-selection
// check.
func stageFinding(stage ValidationStage, source, field, cause string) Finding {
	sev := SeverityTerminal
	if stage == StagePrep {
		sev = SeverityWarning
	}
	return Finding{Source: source, Field: field, Cause: cause, Severity: sev}
}

// includedField/excludedField name the source field a scoped entry came from, for
// finding messages.
func includedField(scope ResourceScope) string {
	if scope == ScopeNamespaced {
		return "namespacedResources"
	}
	return "clusterScopedResources"
}

func excludedField(scope ResourceScope) string {
	if scope == ScopeNamespaced {
		return "excludedNamespacedResources"
	}
	return "excludedClusterScopedResources"
}

// resolveEntryGVR is the single shared implementation of the kind-resolution and
// scope rules: every included or excluded kind must resolve to exactly one API
// resource (a missing, ambiguous, or unconfirmable kind is an error), and the
// resolved scope must match the list the kind was declared under (namespaced vs
// cluster-scoped). Both the Prep/Upgrade validator and the pre-pivot capture call
// it, so the rule is never duplicated. A resolution or scope mismatch is a terminal
// configuration error; a partial discovery failure in an unrelated group does not
// fail a kind that still resolves (the semantics of resolveRequiredGVR).
func resolveEntryGVR(kind string, scope ResourceScope, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) (schema.GroupVersionResource, error) {
	gvr, namespaced, err := resolveRequiredGVR(kind, apiResourceLists, failedGroups)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	switch scope {
	case ScopeNamespaced:
		if !namespaced {
			return schema.GroupVersionResource{}, fmt.Errorf(
				"resource %q is declared as namespaced but is cluster-scoped per discovery", kind)
		}
	case ScopeClusterScoped:
		if namespaced {
			return schema.GroupVersionResource{}, fmt.Errorf(
				"resource %q is declared as cluster-scoped but is namespace-scoped per discovery", kind)
		}
	}
	return gvr, nil
}

// ValidateSelectionModels runs every shared selection rule against the given
// source-independent models in a single pass, appending a Finding per violation
// rather than returning on the first. Config errors are returned as Findings (so
// Prep can report them all and classify severity by stage); a returned error
// signals a transient discovery/API failure that the caller must treat as
// retryable (requeue), not as a configuration failure.
//
// Reference resolution (every name-only source reference must resolve to an
// existing CR) is source-specific and handled by the adapter before this runs.
func (h *BRHandler) ValidateSelectionModels(ctx context.Context, models []SelectionModel, stage ValidationStage) ([]Finding, error) {
	// Discover the server's preferred API resources once for the whole pass. A
	// full discovery failure is transient (retryable); a partial failure yields
	// failedGroups and is handled per-kind by resolveRequiredGVR.
	apiResourceLists, failedGroups, err := h.getServerPreferredResources(ctx)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for i := range models {
		modelFindings, transientErr := h.validateModel(ctx, &models[i], stage, apiResourceLists, failedGroups)
		findings = append(findings, modelFindings...)
		if transientErr != nil {
			return findings, transientErr
		}
	}
	return findings, nil
}

// resolvedInclude is an included entry that resolved to a GVR, carried from the
// include-resolution pass to the named-object-existence and non-empty-selection
// checks.
type resolvedInclude struct {
	entry ResourceEntry
	gvr   schema.GroupVersionResource
	gr    string
}

// validateModel runs the shared rules against a single model, returning its
// findings and a transient error (if a live GET/LIST failed for a reason other
// than NotFound). Each rule group is a small helper so the orchestration stays
// readable.
func (h *BRHandler) validateModel(ctx context.Context, m *SelectionModel, stage ValidationStage,
	apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) ([]Finding, error) {

	var findings []Finding

	findings = append(findings, validateLabelSelector(m)...) // label-selector validity
	findings = append(findings, validateNamespaceList(m)...) // namespace-list sanity

	nsFindings, err := h.validateNamespacesExist(ctx, m, stage) // declared-namespace existence
	findings = append(findings, nsFindings...)
	if err != nil {
		return findings, err
	}

	excluded, exclFindings := resolveExclusions(m, apiResourceLists, failedGroups) // excluded-kind resolution and scope
	findings = append(findings, exclFindings...)

	resolved, incFindings := resolveIncludes(m, apiResourceLists, failedGroups) // included-kind resolution and scope
	findings = append(findings, incFindings...)

	findings = append(findings, checkEmptySelection(m, stage, resolved, excluded)...) // non-empty selection

	namedFindings, err := h.validateNamedObjects(ctx, m, stage, resolved, excluded) // named-object existence
	findings = append(findings, namedFindings...)
	if err != nil {
		return findings, err
	}

	return findings, nil
}

// validateLabelSelector enforces label-selector validity: the labelSelector must be
// syntactically valid. It applies only to unnamed includes; a valid selector that
// matches zero objects is never a finding.
func validateLabelSelector(m *SelectionModel) []Finding {
	if m.LabelSelector == nil {
		return nil
	}
	if _, err := metav1.LabelSelectorAsSelector(m.LabelSelector); err != nil {
		return []Finding{terminalFinding(m.SourceName, "labelSelector",
			fmt.Sprintf("invalid label selector: %s", err.Error()))}
	}
	return nil
}

// validateNamespaceList enforces namespace-list sanity: a namespaced selection needs
// at least one namespace to search, and an empty namespace entry is never valid.
func validateNamespaceList(m *SelectionModel) []Finding {
	var findings []Finding
	for _, ns := range m.Namespaces {
		if ns == "" {
			findings = append(findings, terminalFinding(m.SourceName, namespacesResource,
				"contains an empty namespace entry"))
		}
	}
	hasNamespacedInclude := false
	for _, e := range m.Included {
		if e.Scope == ScopeNamespaced {
			hasNamespacedInclude = true
			break
		}
	}
	if hasNamespacedInclude && len(m.Namespaces) == 0 {
		findings = append(findings, terminalFinding(m.SourceName, namespacesResource,
			"a namespaced selection requires at least one namespace, but namespaces is empty"))
	}
	return findings
}

// validateNamespacesExist enforces declared-namespace existence: every declared
// namespace must exist. Missing is a Prep warning and an Upgrade terminal failure;
// a non-NotFound error is transient (retryable).
func (h *BRHandler) validateNamespacesExist(ctx context.Context, m *SelectionModel, stage ValidationStage) ([]Finding, error) {
	var findings []Finding
	nsGVR := schema.GroupVersionResource{Version: "v1", Resource: namespacesResource}
	for _, ns := range m.Namespaces {
		if ns == "" {
			continue // already reported by the namespace-list sanity check
		}
		if _, err := h.getResourceWithRetry(ctx, h.DynamicClient.Resource(nsGVR), ns); err != nil {
			if k8serrors.IsNotFound(err) {
				findings = append(findings, stageFinding(stage, m.SourceName, namespacesResource,
					fmt.Sprintf("declared namespace %q does not exist", ns)))
				continue
			}
			return findings, fmt.Errorf("failed to check namespace %q for source %q: %w", ns, m.SourceName, err)
		}
	}
	return findings, nil
}

// resolveExclusions enforces excluded-kind resolution and scope: resolve the
// excluded kinds (and their scope) and build the exclusion set from the ones that
// resolve. An unresolvable, ambiguous, or wrongly-scoped exclusion is terminal and
// must not be silently skipped (fail closed so an intended exclusion is never
// silently dropped).
func resolveExclusions(m *SelectionModel, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) (exclusionSet, []Finding) {
	var findings []Finding
	excluded := exclusionSet{kinds: map[string]bool{}, names: map[string]map[string]bool{}}
	for _, e := range m.Excluded {
		gvr, err := resolveEntryGVR(e.Kind, e.Scope, apiResourceLists, failedGroups)
		if err != nil {
			findings = append(findings, terminalFinding(m.SourceName, excludedField(e.Scope),
				fmt.Sprintf("cannot apply exclusion for %q: %s", e.Kind, err.Error())))
			continue
		}
		gr := gvr.GroupResource().String()
		if len(e.Names) == 0 {
			excluded.kinds[gr] = true
			continue
		}
		if excluded.names[gr] == nil {
			excluded.names[gr] = map[string]bool{}
		}
		for _, name := range e.Names {
			excluded.names[gr][name] = true
		}
	}
	return excluded, findings
}

// resolveIncludes enforces included-kind resolution and scope: resolve every
// included kind and confirm its scope, returning the resolved entries (for the
// named-object-existence and non-empty-selection checks) and the findings for the
// ones that fail to resolve.
func resolveIncludes(m *SelectionModel, apiResourceLists []*metav1.APIResourceList, failedGroups map[string]bool) ([]resolvedInclude, []Finding) {
	var resolved []resolvedInclude
	var findings []Finding
	for _, e := range m.Included {
		gvr, err := resolveEntryGVR(e.Kind, e.Scope, apiResourceLists, failedGroups)
		if err != nil {
			findings = append(findings, terminalFinding(m.SourceName, includedField(e.Scope), err.Error()))
			continue
		}
		resolved = append(resolved, resolvedInclude{entry: e, gvr: gvr, gr: gvr.GroupResource().String()})
	}
	return resolved, findings
}

// checkEmptySelection enforces the non-empty-selection rule: the selection must not
// be structurally empty after exclusions (every included kind excluded, or no
// effective include configured). A valid selector that matches zero live objects is
// NOT a finding here, so this is a purely structural check.
func checkEmptySelection(m *SelectionModel, stage ValidationStage, resolved []resolvedInclude, excluded exclusionSet) []Finding {
	for _, r := range resolved {
		if excluded.excludesKind(r.gr) {
			continue
		}
		if len(r.entry.Names) == 0 {
			return nil // an unnamed, non-excluded include is a non-empty selection
		}
		for _, name := range r.entry.Names {
			if !excluded.excludesName(r.gr, name) {
				return nil
			}
		}
	}
	return []Finding{stageFinding(stage, m.SourceName, "",
		"selection is structurally empty: no included resource remains after exclusions")}
}

// validateNamedObjects enforces named-object existence: every explicitly included,
// non-excluded object name must exist in at least one declared scope (a declared
// namespace, or cluster scope for a cluster-scoped resource). Missing is a Prep
// warning and an Upgrade terminal failure; a non-NotFound error is transient.
func (h *BRHandler) validateNamedObjects(ctx context.Context, m *SelectionModel, stage ValidationStage,
	resolved []resolvedInclude, excluded exclusionSet) ([]Finding, error) {

	var findings []Finding
	for _, r := range resolved {
		if len(r.entry.Names) == 0 || excluded.excludesKind(r.gr) {
			continue
		}
		scopes := []string{""}
		if r.entry.Scope == ScopeNamespaced {
			scopes = nonEmptyNamespaces(m.Namespaces)
			if len(scopes) == 0 {
				continue // no namespace to search: already reported by the namespace-list sanity check
			}
		}
		for _, name := range r.entry.Names {
			if name == "" {
				// An empty object name is a configuration error: the dynamic client
				// rejects it locally ("name is required"), which is not a NotFound and
				// would otherwise be misclassified as a transient API failure and
				// retried forever. Report it terminally instead.
				findings = append(findings, terminalFinding(m.SourceName, includedField(r.entry.Scope),
					fmt.Sprintf("%s contains an empty object name", r.entry.Kind)))
				continue
			}
			if excluded.excludesName(r.gr, name) {
				continue
			}
			found, err := h.namedObjectExists(ctx, r.gvr, scopes, name)
			if err != nil {
				return findings, fmt.Errorf("failed to check %s %q for source %q: %w", r.entry.Kind, name, m.SourceName, err)
			}
			if !found {
				findings = append(findings, stageFinding(stage, m.SourceName, includedField(r.entry.Scope),
					fmt.Sprintf("named %s %q does not exist in any declared scope", r.entry.Kind, name)))
			}
		}
	}
	return findings, nil
}

// namedObjectExists reports whether a named object exists in any of the given
// scopes. A NotFound in a scope is not an error; any other error is transient and
// returned to the caller.
func (h *BRHandler) namedObjectExists(ctx context.Context, gvr schema.GroupVersionResource, scopes []string, name string) (bool, error) {
	for _, scope := range scopes {
		if _, err := h.getResourceWithRetry(ctx, h.resourceInterface(gvr, scope), name); err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// nonEmptyNamespaces returns the namespaces with empty entries dropped (empty
// entries are reported separately by the namespace-list sanity check).
func nonEmptyNamespaces(namespaces []string) []string {
	out := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// splitFindings partitions findings into terminal and warning sets, preserving
// order within each.
func splitFindings(findings []Finding) (terminal, warnings []Finding) {
	for _, f := range findings {
		if f.Severity == SeverityTerminal {
			terminal = append(terminal, f)
		} else {
			warnings = append(warnings, f)
		}
	}
	return terminal, warnings
}

// findingStrings renders findings to their string form.
func findingStrings(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.String())
	}
	return out
}

// maxFindingsMessageLen bounds the aggregated finding message carried in a
// condition. The full list is always logged; if the joined message would exceed
// this, it is summarised with an omitted count so the condition stays within the
// condition message limit.
const maxFindingsMessageLen = 1024

// findingsMessage joins findings into a single message suitable for a condition.
// If the joined message would exceed maxFindingsMessageLen it is truncated to the
// findings that fit, with a trailing summary of how many were omitted; callers
// log the full list separately.
func findingsMessage(findings []Finding) string {
	msgs := findingStrings(findings)
	full := strings.Join(msgs, "; ")
	if len(full) <= maxFindingsMessageLen {
		return full
	}
	var b strings.Builder
	included := 0
	for _, m := range msgs {
		// Reserve room for the trailing summary.
		if b.Len()+len(m)+2 > maxFindingsMessageLen-64 {
			break
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(m)
		included++
	}
	fmt.Fprintf(&b, " (and %d more; see controller log for the full list)", len(msgs)-included)
	return b.String()
}
