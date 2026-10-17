package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestExtractBackupSpecsFromConfigmaps(t *testing.T) {
	t.Run("empty configmaps returns empty specs", func(t *testing.T) {
		specs, err := ExtractBackupSpecsFromConfigmaps(nil)
		assert.NoError(t, err)
		assert.Empty(t, specs)
	})

	t.Run("parses backup spec fields including labelSelector", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"backup.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: my-backup
  namespace: openshift-adp
  annotations:
    lca.openshift.io/apply-label: rbac.authorization.k8s.io/v1/clusterroles/klusterlet
    lca.openshift.io/apply-wave: "3"
spec:
  includedNamespaces:
  - test-ns
  includedNamespaceScopedResources:
  - secrets
  includedClusterScopedResources:
  - clusterroles
  labelSelector:
    matchLabels:
      app: my-app
    matchExpressions:
    - key: tier
      operator: In
      values:
      - frontend
      - backend
`,
			},
		}
		specs, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "my-backup", specs[0].Name)
		assert.Equal(t, "openshift-adp", specs[0].Namespace)
		assert.Equal(t, "rbac.authorization.k8s.io/v1/clusterroles/klusterlet", specs[0].ApplyLabel)
		assert.Equal(t, "3", specs[0].ApplyWave)
		assert.Equal(t, []string{"test-ns"}, specs[0].IncludedNamespaces)
		assert.Equal(t, []string{"secrets"}, specs[0].IncludedNamespaceScopedResources)
		assert.Equal(t, []string{"clusterroles"}, specs[0].IncludedClusterScopedResources)
		assert.NotNil(t, specs[0].LabelSelector)
		assert.Equal(t, map[string]string{"app": "my-app"}, specs[0].LabelSelector.MatchLabels)
		assert.Len(t, specs[0].LabelSelector.MatchExpressions, 1)
		assert.Equal(t, "tier", specs[0].LabelSelector.MatchExpressions[0].Key)
		assert.Equal(t, metav1.LabelSelectorOpIn, specs[0].LabelSelector.MatchExpressions[0].Operator)
		assert.Equal(t, []string{"frontend", "backend"}, specs[0].LabelSelector.MatchExpressions[0].Values)
	})

	t.Run("defaults for missing optional fields", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"backup.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: simple-backup
spec:
  includedNamespaces:
  - test-ns
`,
			},
		}
		specs, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "simple-backup", specs[0].Name)
		assert.Empty(t, specs[0].Namespace)
		assert.Empty(t, specs[0].ApplyLabel)
		assert.Empty(t, specs[0].ApplyWave)
		assert.Nil(t, specs[0].LabelSelector)
		assert.Nil(t, specs[0].IncludedClusterScopedResources)
	})

	t.Run("non-Velero documents are ignored", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"mixed.yaml": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: some-configmap
data:
  key: value
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: not-a-backup
spec:
  backupName: my-backup
---
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: the-backup
`,
			},
		}
		specs, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "the-backup", specs[0].Name)
	})

	t.Run("malformed document returns a clear decode error", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"broken.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: broken
   this: is not valid yaml
`,
			},
		}
		_, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode")
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

func TestResolveResourceGVR(t *testing.T) {
	// Simulated output of discovery.ServerPreferredResources(): one preferred
	// version per API group. "widgets" intentionally appears in two groups to
	// exercise the ambiguity path, and "deployments/status" exercises the
	// subresource-skipping path.
	apiResourceLists := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "secrets", Namespaced: true, Kind: "Secret"},
				{Name: "services", Namespaced: true, Kind: "Service"},
				{Name: "namespaces", Namespaced: false, Kind: "Namespace"},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment"},
				{Name: "deployments/status", Namespaced: true, Kind: "Deployment"},
				{Name: "statefulsets", Namespaced: true, Kind: "StatefulSet"},
			},
		},
		{
			GroupVersion: "route.openshift.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "routes", Namespaced: true, Kind: "Route"},
			},
		},
		{
			GroupVersion: "lvm.topolvm.io/v1alpha1",
			APIResources: []metav1.APIResource{
				{Name: "lvmclusters", Namespaced: false, Kind: "LVMCluster"},
			},
		},
		{
			GroupVersion: "example.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Namespaced: true, Kind: "Widget"},
			},
		},
		{
			GroupVersion: "other.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Namespaced: true, Kind: "Widget"},
			},
		},
	}

	testcases := []struct {
		name         string
		resourceType string
		expected     schema.GroupVersionResource
		expectFound  bool
		expectErr    bool
	}{
		{
			name:         "bare 'deployments' resolves to apps group",
			resourceType: "deployments",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectFound:  true,
		},
		{
			name:         "qualified 'deployments.apps' uses explicit group",
			resourceType: "deployments.apps",
			expected:     schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectFound:  true,
		},
		{
			name:         "bare 'secrets' resolves to core group",
			resourceType: "secrets",
			expected:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			expectFound:  true,
		},
		{
			name:         "qualified 'routes.route.openshift.io' uses explicit group",
			resourceType: "routes.route.openshift.io",
			expected:     schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"},
			expectFound:  true,
		},
		{
			name:         "bare CRD 'lvmclusters' discovers non-core group and preferred version",
			resourceType: "lvmclusters",
			expected:     schema.GroupVersionResource{Group: "lvm.topolvm.io", Version: "v1alpha1", Resource: "lvmclusters"},
			expectFound:  true,
		},
		{
			name:         "ambiguous bare name matching multiple groups returns error",
			resourceType: "widgets",
			expectErr:    true,
		},
		{
			name:         "qualified 'widgets.example.io' disambiguates by explicit group",
			resourceType: "widgets.example.io",
			expected:     schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"},
			expectFound:  true,
		},
		{
			name:         "unknown resource is not found and skippable",
			resourceType: "doesnotexist",
			expectFound:  false,
		},
		{
			name:         "qualified name with unknown group is not found and skippable",
			resourceType: "deployments.unknown.io",
			expectFound:  false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result, found, err := resolveResourceGVR(tc.resourceType, apiResourceLists)
			if tc.expectErr {
				assert.Error(t, err)
				assert.False(t, found)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.expectFound, found)
			if tc.expectFound {
				assert.Equal(t, tc.expected, result)
			}
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

	t.Run("non-Velero documents are ignored", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"mixed.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: not-a-restore
---
apiVersion: v1
kind: Secret
metadata:
  name: some-secret
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: the-restore
spec:
  backupName: the-backup
`,
			},
		}
		specs, err := ExtractRestoreSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.NoError(t, err)
		assert.Len(t, specs, 1)
		assert.Equal(t, "the-restore", specs[0].Name)
		assert.Equal(t, "the-backup", specs[0].BackupName)
	})

	t.Run("malformed document returns a clear decode error", func(t *testing.T) {
		cm := corev1.ConfigMap{
			Data: map[string]string{
				"broken.yaml": `
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: broken
spec:
  restorePVs: "not-a-bool-but-a-mapping"
  nested:
     - bad
    indent: here
`,
			},
		}
		_, err := ExtractRestoreSpecsFromConfigmaps([]corev1.ConfigMap{cm})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to decode")
	})
}

func TestExtractBackupSpecsExcludedResources(t *testing.T) {
	cm := corev1.ConfigMap{
		Data: map[string]string{
			"backup.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: my-backup
spec:
  includedNamespaces:
  - test-ns
  includedNamespaceScopedResources:
  - secrets
  - deployments
  includedClusterScopedResources:
  - clusterroles
  - persistentvolumes
  excludedResources:
  - events
  excludedNamespaceScopedResources:
  - secrets
  excludedClusterScopedResources:
  - persistentvolumes
`,
		},
	}
	specs, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
	assert.NoError(t, err)
	assert.Len(t, specs, 1)
	assert.Equal(t, []string{"events"}, specs[0].ExcludedResources)
	assert.Equal(t, []string{"secrets"}, specs[0].ExcludedNamespaceScopedResources)
	assert.Equal(t, []string{"persistentvolumes"}, specs[0].ExcludedClusterScopedResources)
}

func TestIsExcluded(t *testing.T) {
	t.Run("matches bare resource name", func(t *testing.T) {
		assert.True(t, isExcluded("deployments.apps", []string{"deployments"}))
	})

	t.Run("matches full resource type", func(t *testing.T) {
		assert.True(t, isExcluded("deployments.apps", []string{"deployments.apps"}))
	})

	t.Run("case insensitive", func(t *testing.T) {
		assert.True(t, isExcluded("secrets", []string{"Secrets"}))
	})

	t.Run("no match returns false", func(t *testing.T) {
		assert.False(t, isExcluded("secrets", []string{"configmaps"}))
	})

	t.Run("checks multiple lists", func(t *testing.T) {
		assert.True(t, isExcluded("persistentvolumes",
			[]string{"events"}, []string{"persistentvolumes"}))
	})

	t.Run("empty lists returns false", func(t *testing.T) {
		assert.False(t, isExcluded("secrets"))
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

func TestResolveWave(t *testing.T) {
	t.Run("restore wave takes priority", func(t *testing.T) {
		backup := BackupSpec{Name: "b1", ApplyWave: "5"}
		restore := &RestoreSpec{Name: "r1", ApplyWave: "2"}
		assert.Equal(t, 2, resolveWave(backup, restore))
	})

	t.Run("falls back to backup wave", func(t *testing.T) {
		backup := BackupSpec{Name: "b1", ApplyWave: "3"}
		assert.Equal(t, 3, resolveWave(backup, nil))
	})

	t.Run("falls back to backup wave when restore has no wave", func(t *testing.T) {
		backup := BackupSpec{Name: "b1", ApplyWave: "7"}
		restore := &RestoreSpec{Name: "r1"}
		assert.Equal(t, 7, resolveWave(backup, restore))
	})

	t.Run("defaults to 999", func(t *testing.T) {
		backup := BackupSpec{Name: "b1"}
		assert.Equal(t, 999, resolveWave(backup, nil))
	})
}

func TestParseWavePrefix(t *testing.T) {
	t.Run("parses wave prefix", func(t *testing.T) {
		wave, name := parseWavePrefix("001_acm-klusterlet")
		assert.Equal(t, 1, wave)
		assert.Equal(t, "acm-klusterlet", name)
	})

	t.Run("parses three-digit wave", func(t *testing.T) {
		wave, name := parseWavePrefix("999_test-app")
		assert.Equal(t, 999, wave)
		assert.Equal(t, "test-app", name)
	})

	t.Run("no prefix defaults to 999", func(t *testing.T) {
		wave, name := parseWavePrefix("my-backup")
		assert.Equal(t, 999, wave)
		assert.Equal(t, "my-backup", name)
	})
}

func TestResolveStatusResources(t *testing.T) {
	t.Run("nil restore returns nil", func(t *testing.T) {
		assert.Nil(t, resolveStatusResources(nil))
	})

	t.Run("empty restore resources returns nil", func(t *testing.T) {
		restore := &RestoreSpec{Name: "r1"}
		assert.Nil(t, resolveStatusResources(restore))
	})

	t.Run("returns lowercased resource set", func(t *testing.T) {
		restore := &RestoreSpec{
			Name:                   "r1",
			RestoreStatusResources: []string{"LogicalVolumes", "deployments"},
		}
		result := resolveStatusResources(restore)
		assert.True(t, result["logicalvolumes"])
		assert.True(t, result["deployments"])
		assert.False(t, result["services"])
	})
}

func TestShouldPreserveStatus(t *testing.T) {
	t.Run("nil map returns false", func(t *testing.T) {
		resource := &unstructured.Unstructured{}
		resource.SetKind("Deployment")
		assert.False(t, shouldPreserveStatus(resource, nil))
	})

	t.Run("matching kind returns true", func(t *testing.T) {
		resource := &unstructured.Unstructured{}
		resource.SetKind("Deployment")
		statusResources := map[string]bool{"deployments": true}
		assert.True(t, shouldPreserveStatus(resource, statusResources))
	})

	t.Run("non-matching kind returns false", func(t *testing.T) {
		resource := &unstructured.Unstructured{}
		resource.SetKind("Service")
		statusResources := map[string]bool{"deployments": true}
		assert.False(t, shouldPreserveStatus(resource, statusResources))
	})
}
