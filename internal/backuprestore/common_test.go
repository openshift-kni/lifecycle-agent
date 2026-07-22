package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExtractBackupSpecsFromConfigmaps(t *testing.T) {
	t.Run("empty configmaps returns empty specs", func(t *testing.T) {
		specs, err := ExtractBackupSpecsFromConfigmaps(nil)
		assert.NoError(t, err)
		assert.Empty(t, specs)
	})
}

func TestParseResourceType(t *testing.T) {
	testcases := []struct {
		name         string
		resourceType string
		expected     schema.GroupVersionResource
	}{
		{
			name:         "bare resource defaults to core API group",
			resourceType: "secrets",
			expected:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		},
		{
			name:         "qualified resource with single-segment group",
			resourceType: "deployments.apps",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		},
		{
			name:         "qualified resource with multi-segment group",
			resourceType: "routes.route.openshift.io",
			expected:     schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := parseResourceType(tc.resourceType)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestResolveResourceType(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "", Version: "v1"},
		{Group: "apps", Version: "v1"},
		{Group: "route.openshift.io", Version: "v1"},
		{Group: "example.io", Version: "v1alpha1"},
	})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "route.openshift.io", Version: "v1", Kind: "Route"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1alpha1", Kind: "Widget"}, meta.RESTScopeNamespace)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRESTMapper(mapper).Build()
	handler := &BRHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("test"),
	}

	testcases := []struct {
		name         string
		resourceType string
		expected     schema.GroupVersionResource
	}{
		{
			name:         "bare 'deployments' resolves to apps group via REST mapper",
			resourceType: "deployments",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		},
		{
			name:         "qualified 'deployments.apps' uses explicit group",
			resourceType: "deployments.apps",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		},
		{
			name:         "bare 'secrets' resolves to core group",
			resourceType: "secrets",
			expected:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
		},
		{
			name:         "bare 'services' resolves to core group",
			resourceType: "services",
			expected:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"},
		},
		{
			name:         "bare 'statefulsets' resolves to apps group",
			resourceType: "statefulsets",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
		},
		{
			name:         "qualified 'routes.route.openshift.io' uses explicit group",
			resourceType: "routes.route.openshift.io",
			expected:     schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"},
		},
		{
			name:         "qualified non-v1 CRD discovers correct API version via REST mapper",
			resourceType: "widgets.example.io",
			expected:     schema.GroupVersionResource{Group: "example.io", Version: "v1alpha1", Resource: "widgets"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result := handler.resolveResourceType(tc.resourceType)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestSortBackupSpecsByApplyWave(t *testing.T) {
	t.Run("empty specs returns nil", func(t *testing.T) {
		groups, err := SortBackupSpecsByApplyWave(nil)
		assert.NoError(t, err)
		assert.Nil(t, groups)
	})

	t.Run("specs are grouped and sorted by wave", func(t *testing.T) {
		specs := []BackupSpec{
			{Name: "b", ApplyWave: "2"},
			{Name: "a", ApplyWave: "1"},
			{Name: "c", ApplyWave: "1"},
			{Name: "d", ApplyWave: "2"},
		}
		groups, err := SortBackupSpecsByApplyWave(specs)
		assert.NoError(t, err)
		assert.Equal(t, 2, len(groups))
		assert.Equal(t, 2, len(groups[0]))
		assert.Equal(t, "a", groups[0][0].Name)
		assert.Equal(t, "c", groups[0][1].Name)
		assert.Equal(t, 2, len(groups[1]))
		assert.Equal(t, "b", groups[1][0].Name)
		assert.Equal(t, "d", groups[1][1].Name)
	})
}

func TestExtractRestoreSpecsFromConfigmaps(t *testing.T) {
	t.Run("empty configmaps returns empty specs", func(t *testing.T) {
		specs, err := ExtractRestoreSpecsFromConfigmaps(nil)
		assert.NoError(t, err)
		assert.Empty(t, specs)
	})

	t.Run("parses restore spec fields", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"restore.yaml": `
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: my-restore
  namespace: openshift-adp
  annotations:
    lca.openshift.io/apply-wave: "2"
spec:
  backupName: my-backup
  restorePVs: true
  restoreStatus:
    includedResources:
      - logicalvolumes
      - deployments
`,
			},
		}
		specs, err := ExtractRestoreSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "my-restore", specs[0].Name)
		assert.Equal(t, "openshift-adp", specs[0].Namespace)
		assert.Equal(t, "my-backup", specs[0].BackupName)
		assert.Equal(t, "2", specs[0].ApplyWave)
		assert.True(t, specs[0].RestorePVs)
		assert.Equal(t, []string{"logicalvolumes", "deployments"}, specs[0].RestoreStatusResources)
	})

	t.Run("defaults for missing fields", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"restore.yaml": `
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: simple-restore
spec:
  backupName: simple-backup
`,
			},
		}
		specs, err := ExtractRestoreSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "simple-backup", specs[0].BackupName)
		assert.False(t, specs[0].RestorePVs)
		assert.Empty(t, specs[0].RestoreStatusResources)
		assert.Empty(t, specs[0].ApplyWave)
	})
}

func TestValidateBackupRestoreMapping(t *testing.T) {
	t.Run("valid mapping passes", func(t *testing.T) {
		restores := []RestoreSpec{
			{Name: "r1", BackupName: "b1"},
			{Name: "r2", BackupName: "b2"},
		}
		assert.NoError(t, ValidateBackupRestoreMapping(restores))
	})

	t.Run("duplicate backup references fail", func(t *testing.T) {
		restores := []RestoreSpec{
			{Name: "r1", BackupName: "b1"},
			{Name: "r2", BackupName: "b1"},
		}
		err := ValidateBackupRestoreMapping(restores)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "multiple Restore CRs")
	})

	t.Run("empty backup name is ignored", func(t *testing.T) {
		restores := []RestoreSpec{
			{Name: "r1", BackupName: ""},
			{Name: "r2", BackupName: ""},
		}
		assert.NoError(t, ValidateBackupRestoreMapping(restores))
	})
}

func TestFindRestoreForBackup(t *testing.T) {
	restores := []RestoreSpec{
		{Name: "r1", BackupName: "b1"},
		{Name: "r2", BackupName: "b2"},
	}

	t.Run("finds matching restore", func(t *testing.T) {
		r := FindRestoreForBackup("b1", restores)
		assert.NotNil(t, r)
		assert.Equal(t, "r1", r.Name)
	})

	t.Run("returns nil for no match", func(t *testing.T) {
		r := FindRestoreForBackup("b3", restores)
		assert.Nil(t, r)
	})
}
