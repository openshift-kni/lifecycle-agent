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
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// These tests exercise ValidateSelectionModels against SelectionModels built
// directly (never through the BackupContent adapter), proving the follow-up
// ConfigMap/Velero source can reuse every shared rule by supplying its own
// adapter.

// newValidatorHandler builds a BRHandler backed by a fake dynamic client (for the
// live declared-namespace and named-object existence checks) and the given discovery interface.
func newValidatorHandler(disc discovery.DiscoveryInterface, objs ...runtime.Object) *BRHandler {
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), objs...)
	return &BRHandler{DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}
}

// ambiguousWidgetsDiscovery advertises "widgets" under two API groups so a bare
// "widgets" reference is ambiguous, plus the core v1 types used by the tests.
func ambiguousWidgetsDiscovery() *fakeDiscovery {
	return &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace"},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
		}},
		"a.example.com/v1": {GroupVersion: "a.example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Namespaced: true, Kind: "Widget"},
		}},
		"b.example.com/v1": {GroupVersion: "b.example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", SingularName: "widget", Namespaced: true, Kind: "Widget"},
		}},
	}}
}

// partialFailDiscovery reports a partial (per-group) discovery failure: the listed
// lists resolve, and failedGroups come back as an ErrGroupDiscoveryFailed so a bare
// name that does not resolve cannot be confirmed absent.
type partialFailDiscovery struct {
	discovery.DiscoveryInterface
	lists        []*metav1.APIResourceList
	failedGroups []string
}

func (f *partialFailDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	groups := map[schema.GroupVersion]error{}
	for _, g := range f.failedGroups {
		groups[schema.GroupVersion{Group: g, Version: "v1"}] = fmt.Errorf("discovery failed for %s", g)
	}
	return f.lists, &discovery.ErrGroupDiscoveryFailed{Groups: groups}
}

// fullFailDiscovery reports a full discovery failure (not a per-group one), which
// getServerPreferredResources surfaces as a transient error.
type fullFailDiscovery struct {
	discovery.DiscoveryInterface
}

func (f *fullFailDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return nil, fmt.Errorf("the server could not be reached")
}

func findingWith(findings []Finding, substr string) *Finding {
	for i := range findings {
		if strings.Contains(findings[i].String(), substr) {
			return &findings[i]
		}
	}
	return nil
}

func nsObj(name string) runtime.Object {
	return newUnstructuredWithLabels("Namespace", name, "", nil)
}

// bothStages runs fn for StagePrep and StageUpgrade.
func bothStages(t *testing.T, fn func(t *testing.T, stage ValidationStage)) {
	t.Helper()
	t.Run("prep", func(t *testing.T) { fn(t, StagePrep) })
	t.Run("upgrade", func(t *testing.T) { fn(t, StageUpgrade) })
}

func TestValidateSelectionModelsBC2UnknownKind(t *testing.T) {
	bothStages(t, func(t *testing.T, stage ValidationStage) {
		h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
		models := []SelectionModel{{
			SourceName: "bc",
			Namespaces: []string{"app-ns"},
			Included:   []ResourceEntry{{Kind: "widgets", Scope: ScopeNamespaced}},
		}}
		findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
		assert.NoError(t, err)
		f := findingWith(findings, "widgets")
		if assert.NotNil(t, f, "expected a finding for the unknown kind") {
			assert.Equal(t, SeverityTerminal, f.Severity)
			assert.Contains(t, f.Cause, "not found")
		}
	})
}

func TestValidateSelectionModelsBC2AmbiguousKind(t *testing.T) {
	bothStages(t, func(t *testing.T, stage ValidationStage) {
		h := newValidatorHandler(ambiguousWidgetsDiscovery(), nsObj("app-ns"))
		models := []SelectionModel{{
			SourceName: "bc",
			Namespaces: []string{"app-ns"},
			Included:   []ResourceEntry{{Kind: "widgets", Scope: ScopeNamespaced}},
		}}
		findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
		assert.NoError(t, err)
		f := findingWith(findings, "ambiguous")
		if assert.NotNil(t, f, "expected an ambiguity finding") {
			assert.Equal(t, SeverityTerminal, f.Severity)
			assert.Contains(t, f.Cause, "a.example.com")
			assert.Contains(t, f.Cause, "b.example.com")
			assert.Contains(t, f.Cause, "<resource>.<group>")
		}
	})
}

func TestValidateSelectionModelsBC2DiscoveryIncomplete(t *testing.T) {
	bothStages(t, func(t *testing.T, stage ValidationStage) {
		disc := &partialFailDiscovery{
			lists: []*metav1.APIResourceList{
				{GroupVersion: "v1", APIResources: []metav1.APIResource{
					{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
				}},
			},
			failedGroups: []string{"apps"},
		}
		h := newValidatorHandler(disc, nsObj("app-ns"))
		models := []SelectionModel{{
			SourceName: "bc",
			Namespaces: []string{"app-ns"},
			// A bare name that does not resolve while discovery is incomplete cannot
			// be confirmed absent -> terminal, discovery-specific.
			Included: []ResourceEntry{{Kind: "deployments", Scope: ScopeNamespaced}},
		}}
		findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
		assert.NoError(t, err)
		f := findingWith(findings, "deployments")
		if assert.NotNil(t, f, "expected a terminal discovery-incomplete finding") {
			assert.Equal(t, SeverityTerminal, f.Severity)
			assert.Contains(t, f.Cause, "incomplete")
		}
	})
}

func TestValidateSelectionModelsBC3IncludedScopeMismatch(t *testing.T) {
	bothStages(t, func(t *testing.T, stage ValidationStage) {
		h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
		models := []SelectionModel{{
			SourceName: "bc",
			// configmaps are namespaced, but declared as cluster-scoped.
			Included: []ResourceEntry{{Kind: "configmaps", Scope: ScopeClusterScoped}},
		}}
		findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
		assert.NoError(t, err)
		f := findingWith(findings, "namespace-scoped per discovery")
		if assert.NotNil(t, f, "expected a scope-mismatch finding") {
			assert.Equal(t, SeverityTerminal, f.Severity)
		}
	})
}

func TestValidateSelectionModelsBC4Namespaces(t *testing.T) {
	t.Run("empty namespaces list with a namespaced selection", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery())
			models := []SelectionModel{{
				SourceName: "bc",
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "at least one namespace")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})

	t.Run("empty namespace entry", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns", ""},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "empty namespace entry")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})
}

func TestValidateSelectionModelsBC5MissingNamespace(t *testing.T) {
	cases := []struct {
		stage    ValidationStage
		severity Severity
	}{
		{StagePrep, SeverityWarning},
		{StageUpgrade, SeverityTerminal},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("stage-%d", tc.stage), func(t *testing.T) {
			// app-ns exists; missing-ns does not.
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns", "missing-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, tc.stage)
			assert.NoError(t, err)
			f := findingWith(findings, `declared namespace "missing-ns" does not exist`)
			if assert.NotNil(t, f, "expected a missing-namespace finding") {
				assert.Equal(t, tc.severity, f.Severity)
			}
		})
	}
}

func TestValidateSelectionModelsBC6LabelSelector(t *testing.T) {
	t.Run("invalid selector is terminal in both stages", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				LabelSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{Key: "x", Operator: "BadOperator"},
					},
				},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "invalid label selector")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})

	t.Run("valid selector matching zero objects is valid", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName:    "bc",
				Namespaces:    []string{"app-ns"},
				Included:      []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"nomatch": "true"}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			assert.Empty(t, findings, "a valid zero-match selector is not a finding")
		})
	})
}

func TestValidateSelectionModelsBC7UnresolvableExclusion(t *testing.T) {
	t.Run("misspelled exclusion kind", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				Excluded:   []ResourceEntry{{Kind: "configmapz", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "cannot apply exclusion")
			if assert.NotNil(t, f, "an unresolvable exclusion must not be silently skipped") {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})

	t.Run("ambiguous exclusion kind", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(ambiguousWidgetsDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				Excluded:   []ResourceEntry{{Kind: "widgets", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "ambiguous")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})
}

func TestValidateSelectionModelsBC8ExclusionScope(t *testing.T) {
	t.Run("persistentvolumes in excludedNamespacedResources", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				Excluded:   []ResourceEntry{{Kind: "persistentvolumes", Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "cluster-scoped per discovery")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})

	t.Run("configmaps in excludedClusterScopedResources", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			h := newValidatorHandler(backupContentDiscovery())
			models := []SelectionModel{{
				SourceName: "bc",
				Included:   []ResourceEntry{{Kind: "persistentvolumes", Scope: ScopeClusterScoped}},
				Excluded:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeClusterScoped}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			f := findingWith(findings, "namespace-scoped per discovery")
			if assert.NotNil(t, f) {
				assert.Equal(t, SeverityTerminal, f.Severity)
			}
		})
	})
}

func TestValidateSelectionModelsBC9MissingNamedObject(t *testing.T) {
	cases := []struct {
		stage    ValidationStage
		severity Severity
	}{
		{StagePrep, SeverityWarning},
		{StageUpgrade, SeverityTerminal},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("stage-%d", tc.stage), func(t *testing.T) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Names: []string{"cm-missing"}, Scope: ScopeNamespaced}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, tc.stage)
			assert.NoError(t, err)
			f := findingWith(findings, `named configmaps "cm-missing" does not exist`)
			if assert.NotNil(t, f, "expected a missing-named-object finding") {
				assert.Equal(t, tc.severity, f.Severity)
			}
		})
	}
}

// TestValidateSelectionModelsEmptyNamedObject verifies that an empty object name in
// an included entry is reported as a terminal configuration finding in both stages,
// rather than reaching the dynamic client (which rejects it locally with "name is
// required") where it would be misclassified as a transient failure and retried
// forever.
func TestValidateSelectionModelsEmptyNamedObject(t *testing.T) {
	bothStages(t, func(t *testing.T, stage ValidationStage) {
		h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
		models := []SelectionModel{{
			SourceName: "bc",
			Namespaces: []string{"app-ns"},
			Included:   []ResourceEntry{{Kind: "configmaps", Names: []string{""}, Scope: ScopeNamespaced}},
		}}
		findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
		assert.NoError(t, err, "an empty name is a config finding, never a transient error")
		f := findingWith(findings, "empty object name")
		if assert.NotNil(t, f, "expected an empty-name finding") {
			assert.Equal(t, SeverityTerminal, f.Severity)
		}
	})
}

func TestValidateSelectionModelsBC10StructurallyEmpty(t *testing.T) {
	cases := []struct {
		stage    ValidationStage
		severity Severity
	}{
		{StagePrep, SeverityWarning},
		{StageUpgrade, SeverityTerminal},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("every included kind excluded stage-%d", tc.stage), func(t *testing.T) {
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
			models := []SelectionModel{{
				SourceName: "bc",
				Namespaces: []string{"app-ns"},
				Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				Excluded:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}}, // kind-only exclusion
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, tc.stage)
			assert.NoError(t, err)
			f := findingWith(findings, "structurally empty")
			if assert.NotNil(t, f) {
				assert.Equal(t, tc.severity, f.Severity)
			}
		})
	}

	t.Run("a valid selector matching zero objects is not an empty-selection finding", func(t *testing.T) {
		bothStages(t, func(t *testing.T, stage ValidationStage) {
			cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
			h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"), cm)
			models := []SelectionModel{{
				SourceName:    "bc",
				Namespaces:    []string{"app-ns"},
				Included:      []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"nomatch": "true"}},
			}}
			findings, err := h.ValidateSelectionModels(context.Background(), models, stage)
			assert.NoError(t, err)
			assert.Nil(t, findingWith(findings, "structurally empty"),
				"an unnamed, non-excluded include is a non-empty selection regardless of live matches")
		})
	})
}

// TestValidateSelectionModelsOnePass verifies that two models, each with a
// distinct violation, produce both findings in a single call (one-pass
// aggregation) rather than returning on the first.
func TestValidateSelectionModelsOnePass(t *testing.T) {
	h := newValidatorHandler(backupContentDiscovery(), nsObj("app-ns"))
	models := []SelectionModel{
		{
			SourceName: "bc-a",
			Namespaces: []string{"app-ns"},
			Included:   []ResourceEntry{{Kind: "widgets", Scope: ScopeNamespaced}}, // unknown kind
		},
		{
			SourceName: "bc-b",
			Namespaces: []string{"app-ns"},
			Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
			Excluded:   []ResourceEntry{{Kind: "persistentvolumes", Scope: ScopeNamespaced}}, // wrong scope
		},
	}
	findings, err := h.ValidateSelectionModels(context.Background(), models, StageUpgrade)
	assert.NoError(t, err)
	assert.NotNil(t, findingWith(findings, "widgets"), "expected the first model's finding")
	assert.NotNil(t, findingWith(findings, "cluster-scoped per discovery"), "expected the second model's finding")
}

// TestValidateSelectionModelsTransientDiscovery verifies that a full discovery
// failure is returned as a (retryable) error, not as a config finding.
func TestValidateSelectionModelsTransientDiscovery(t *testing.T) {
	withFastTransientBackoff(t)
	h := newValidatorHandler(&fullFailDiscovery{})
	models := []SelectionModel{{
		SourceName: "bc",
		Namespaces: []string{"app-ns"},
		Included:   []ResourceEntry{{Kind: "configmaps", Scope: ScopeNamespaced}},
	}}
	findings, err := h.ValidateSelectionModels(context.Background(), models, StagePrep)
	assert.Error(t, err)
	assert.Nil(t, findings)
}

// TestValidateSelectionModelsTransientLiveAccess verifies that a non-NotFound
// error from a live existence check (named-object existence) is returned as a retryable error.
func TestValidateSelectionModelsTransientLiveAccess(t *testing.T) {
	withFastTransientBackoff(t)
	cm := newUnstructuredWithLabels("ConfigMap", "cm", "app-ns", nil)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), nsObj("app-ns"), cm)
	dc.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewServiceUnavailable("api down")
	})
	h := &BRHandler{DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}
	models := []SelectionModel{{
		SourceName: "bc",
		Namespaces: []string{"app-ns"},
		Included:   []ResourceEntry{{Kind: "configmaps", Names: []string{"cm"}, Scope: ScopeNamespaced}},
	}}
	_, err := h.ValidateSelectionModels(context.Background(), models, StagePrep)
	assert.Error(t, err)
	assert.False(t, IsBRFailedValidationError(err), "a transient error must not be a terminal validation error")
}
