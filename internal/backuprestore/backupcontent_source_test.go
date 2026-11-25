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
	"strings"
	"testing"

	"github.com/go-logr/logr"
	backupcontentv1 "github.com/openshift-kni/lifecycle-agent/api/backupcontent/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestBackupContentToSelectionModel verifies the adapter maps a BackupContent CR
// into the source-independent SelectionModel: scope tagging, the excluded lists,
// and the labelSelector carried as-is.
func TestBackupContentToSelectionModel(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc1"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces: []string{"ns-a", "ns-b"},
			NamespacedResources: []backupcontentv1.ResourceSelector{
				{Kind: "configmaps"},
				{Kind: "secrets", Names: []string{"s1"}},
			},
			ClusterScopedResources: []backupcontentv1.ResourceSelector{
				{Kind: "persistentvolumes"},
			},
			ExcludedNamespacedResources: []backupcontentv1.ResourceSelector{
				{Kind: "secrets", Names: []string{"s-excl"}},
			},
			ExcludedClusterScopedResources: []backupcontentv1.ResourceSelector{
				{Kind: "clusterroles"},
			},
			LabelSelector: sel,
		},
	}

	m := backupContentToSelectionModel(bc)
	assert.Equal(t, "bc1", m.SourceName)
	assert.Equal(t, []string{"ns-a", "ns-b"}, m.Namespaces)
	assert.Same(t, sel, m.LabelSelector)

	assert.Equal(t, []ResourceEntry{
		{Kind: "configmaps", Scope: ScopeNamespaced},
		{Kind: "secrets", Names: []string{"s1"}, Scope: ScopeNamespaced},
		{Kind: "persistentvolumes", Scope: ScopeClusterScoped},
	}, m.Included)

	assert.Equal(t, []ResourceEntry{
		{Kind: "secrets", Names: []string{"s-excl"}, Scope: ScopeNamespaced},
		{Kind: "clusterroles", Scope: ScopeClusterScoped},
	}, m.Excluded)
}

// newBackupContentValidationHandler builds a BRHandler with a typed client holding
// the BackupContent CRs, a fake dynamic client holding the live objects, and the
// given discovery.
func newBackupContentValidationHandler(t *testing.T, bcs []*backupcontentv1.BackupContent, dynObjs ...runtime.Object) *BRHandler {
	typedObjs := make([]client.Object, 0, len(bcs))
	for _, bc := range bcs {
		typedObjs = append(typedObjs, bc)
	}
	c := newBackupContentTypedClient(t, typedObjs...)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme(), dynObjs...)
	return &BRHandler{Client: c, DynamicClient: dc, DiscoveryClient: backupContentDiscovery(), Log: logr.Discard()}
}

func TestValidateBackupContentDanglingReferences(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")
	h := newBackupContentValidationHandler(t, nil)

	warnings, err := h.ValidateBackupContent(context.Background(),
		[]ibuv1.BackupContentRef{{Name: "absent-a"}, {Name: "absent-b"}})
	assert.Empty(t, warnings)
	assert.Error(t, err)
	assert.True(t, IsBRFailedValidationError(err), "a missing reference is terminal in Prep")
	// One-pass: both missing references are reported.
	assert.Contains(t, err.Error(), "absent-a")
	assert.Contains(t, err.Error(), "absent-b")
}

func TestValidateBackupContentTerminalRule(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc1", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:          []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "widgets"}}, // unknown kind -> BC-2 terminal
		},
	}
	h := newBackupContentValidationHandler(t, []*backupcontentv1.BackupContent{bc}, nsObj("app-ns"))

	// A terminal finding fails Prep via the returned error; any incidental warnings
	// are irrelevant because the caller fails on err first.
	_, err := h.ValidateBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc1"}})
	assert.Error(t, err)
	assert.True(t, IsBRFailedValidationError(err))
	assert.Contains(t, err.Error(), "widgets")
}

func TestValidateBackupContentWarningOnly(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc1", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:          []string{"missing-ns"}, // BC-5 -> Prep warning
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
		},
	}
	// missing-ns is absent from the dynamic client.
	h := newBackupContentValidationHandler(t, []*backupcontentv1.BackupContent{bc})

	warnings, err := h.ValidateBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc1"}})
	assert.NoError(t, err, "a warning-only config must not fail Prep")
	if assert.Len(t, warnings, 1) {
		assert.Contains(t, warnings[0], "missing-ns")
	}
}

func TestValidateBackupContentTransientDiscovery(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")
	withFastTransientBackoff(t)
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "bc1", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:          []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
		},
	}
	c := newBackupContentTypedClient(t, bc)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme())
	h := &BRHandler{Client: c, DynamicClient: dc, DiscoveryClient: &fullFailDiscovery{}, Log: logr.Discard()}

	warnings, err := h.ValidateBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc1"}})
	assert.Empty(t, warnings)
	assert.Error(t, err)
	assert.False(t, IsBRFailedValidationError(err), "a transient discovery failure must not be terminal")
}

// TestValidateBackupContentTerminalFindingSurvivesTransientError verifies that a
// terminal finding collected before a transient API failure still fails Prep
// terminally: a missing reference (BC-1) is reported even though discovery then
// fails transiently, so Prep does not requeue forever on a known-bad config.
func TestValidateBackupContentTerminalFindingSurvivesTransientError(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")
	withFastTransientBackoff(t)
	bc := &backupcontentv1.BackupContent{
		ObjectMeta: metav1.ObjectMeta{Name: "present", Namespace: "lca-ns"},
		Spec: backupcontentv1.BackupContentSpec{
			Namespaces:          []string{"app-ns"},
			NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
		},
	}
	c := newBackupContentTypedClient(t, bc)
	dc := dynamicfake.NewSimpleDynamicClient(backupContentDynScheme())
	// Discovery fails transiently, so the shared validator returns a transient error.
	h := &BRHandler{Client: c, DynamicClient: dc, DiscoveryClient: &fullFailDiscovery{}, Log: logr.Discard()}

	// "absent" is a missing reference (terminal BC-1 finding); "present" resolves.
	_, err := h.ValidateBackupContent(context.Background(),
		[]ibuv1.BackupContentRef{{Name: "present"}, {Name: "absent"}})
	assert.Error(t, err)
	assert.True(t, IsBRFailedValidationError(err),
		"a known terminal finding must fail Prep terminally even when a later lookup fails transiently")
	assert.Contains(t, err.Error(), "absent")
}

// TestBackupContentIntroducedAfterPrep covers the same invalid BackupContent both
// when it is present during Prep (a warning, Prep continues) and when the same
// invalidity is introduced after a successful Prep and caught at Upgrade pre-pivot
// (terminal). It also asserts the pre-pivot failure is a BRFailedValidationError,
// so it takes the terminal (no plain-error requeue) path.
func TestBackupContentIntroducedAfterPrep(t *testing.T) {
	t.Setenv(common.OperatorNamespaceEnvVar, "lca-ns")

	t.Run("missing selected namespace", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bc1", Namespace: "lca-ns"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces:          []string{"app-ns"},
				NamespacedResources: []backupcontentv1.ResourceSelector{{Kind: "configmaps"}},
			},
		}
		// app-ns is absent -> BC-5.
		h := newBackupContentValidationHandler(t, []*backupcontentv1.BackupContent{bc})

		// Prep: a warning, Prep completes.
		warnings, err := h.ValidateBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc1"}})
		assert.NoError(t, err)
		assert.NotEmpty(t, warnings)

		// Upgrade pre-pivot: the same condition is now terminal.
		_, err = h.startBackupFromBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc1"}}, &BackupTracker{})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err), "a missing namespace is terminal at pre-pivot")
		assert.Contains(t, err.Error(), "app-ns")
	})

	t.Run("missing named object", func(t *testing.T) {
		bc := &backupcontentv1.BackupContent{
			ObjectMeta: metav1.ObjectMeta{Name: "bc2", Namespace: "lca-ns"},
			Spec: backupcontentv1.BackupContentSpec{
				Namespaces: []string{"app-ns"},
				NamespacedResources: []backupcontentv1.ResourceSelector{
					{Kind: "configmaps", Names: []string{"cm-missing"}},
				},
			},
		}
		// app-ns exists but cm-missing does not.
		h := newBackupContentValidationHandler(t, []*backupcontentv1.BackupContent{bc}, nsObj("app-ns"))

		warnings, err := h.ValidateBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc2"}})
		assert.NoError(t, err)
		assert.NotEmpty(t, warnings)

		_, err = h.startBackupFromBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "bc2"}}, &BackupTracker{})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err))
		assert.Contains(t, err.Error(), "cm-missing")
	})

	t.Run("referenced BackupContent deleted after Prep", func(t *testing.T) {
		// No BackupContent CRs exist: the reference is dangling at pre-pivot.
		h := newBackupContentValidationHandler(t, nil)
		_, err := h.startBackupFromBackupContent(context.Background(), []ibuv1.BackupContentRef{{Name: "gone"}}, &BackupTracker{})
		assert.Error(t, err)
		assert.True(t, IsBRFailedValidationError(err), "a deleted reference is terminal at pre-pivot")
	})
}

// TestBoundWarningAnnotationValue verifies the persisted warning annotation stays
// within the remaining annotation budget: a short list is kept verbatim, an
// oversized list is truncated to whole warnings with an omitted count, and the
// space already used by unrelated annotations shrinks the budget.
func TestBoundWarningAnnotationValue(t *testing.T) {
	t.Run("short list kept verbatim", func(t *testing.T) {
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		warnings := []string{"warning one", "warning two"}
		got, ok := BoundWarningAnnotationValue(ibu, warnings)
		assert.True(t, ok)
		assert.Equal(t, "warning one; warning two", got)
	})

	t.Run("oversized list truncated with omitted count", func(t *testing.T) {
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		long := strings.Repeat("x", 1024)
		warnings := make([]string, 0, 1000)
		for i := 0; i < 1000; i++ {
			warnings = append(warnings, long)
		}
		got, ok := BoundWarningAnnotationValue(ibu, warnings)
		assert.True(t, ok)
		assert.LessOrEqual(t, len(got), maxTotalAnnotationBytes)
		assert.Contains(t, got, "more; see controller log for the full list")
	})

	t.Run("existing annotations shrink the budget", func(t *testing.T) {
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		ibu.SetAnnotations(map[string]string{"other": strings.Repeat("y", maxTotalAnnotationBytes-4096)})
		long := strings.Repeat("z", 512)
		warnings := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			warnings = append(warnings, long)
		}
		got, ok := BoundWarningAnnotationValue(ibu, warnings)
		assert.True(t, ok)
		used := len("other") + (maxTotalAnnotationBytes - 4096)
		assert.LessOrEqual(t, len(got)+used+len(BackupContentValidationWarningAnnotation), maxTotalAnnotationBytes,
			"persisted warning plus existing annotations must fit the total annotation limit")
	})

	t.Run("existing annotations leave no room for the omitted summary", func(t *testing.T) {
		// Existing annotations consume almost the entire budget, so even the
		// "(and N more...)" summary does not fit. The result must still stay within
		// the total annotation limit rather than overflowing it and failing the patch.
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		otherKey := "other"
		// Leave a budget of 40 bytes — smaller than the omitted-count summary itself.
		otherVal := strings.Repeat("y", maxTotalAnnotationBytes-len(otherKey)-len(BackupContentValidationWarningAnnotation)-warningAnnotationReserve-40)
		ibu.SetAnnotations(map[string]string{otherKey: otherVal})
		long := strings.Repeat("z", 512)
		warnings := make([]string, 0, 100)
		for i := 0; i < 100; i++ {
			warnings = append(warnings, long)
		}
		got, ok := BoundWarningAnnotationValue(ibu, warnings)
		assert.True(t, ok)
		used := len(otherKey) + len(otherVal)
		assert.LessOrEqual(t, len(got)+used+len(BackupContentValidationWarningAnnotation), maxTotalAnnotationBytes,
			"persisted warning plus existing annotations must fit the total annotation limit even with no room for the summary")
	})

	t.Run("existing annotations leave no room for even the warning key", func(t *testing.T) {
		// The IBU's other annotations fill the limit so completely that not even the
		// warning key fits. BoundWarningAnnotationValue must report ok=false so the
		// caller skips the patch instead of overflowing the total annotation limit and
		// turning a warning-only Prep into a terminal failure.
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		otherKey := "other"
		// Consume the entire limit: nothing is left for the warning key or its value.
		otherVal := strings.Repeat("y", maxTotalAnnotationBytes-len(otherKey))
		ibu.SetAnnotations(map[string]string{otherKey: otherVal})
		warnings := []string{"warning one", "warning two"}
		got, ok := BoundWarningAnnotationValue(ibu, warnings)
		assert.False(t, ok, "annotation must not be persisted when there is no room for even the key")
		assert.Empty(t, got)
	})
}
