package backuprestore

import (
	"fmt"
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

func TestResolveResourceGVR(t *testing.T) {
	// Simulated output of discovery.ServerPreferredResources(): one preferred
	// version per API group. "widgets" intentionally appears in two groups to
	// exercise the ambiguity path, and "deployments/status" exercises the
	// subresource-skipping path.
	apiResourceLists := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret"},
				{Name: "services", SingularName: "service", Namespaced: true, Kind: "Service", ShortNames: []string{"svc"}},
				{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace", ShortNames: []string{"ns"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment", ShortNames: []string{"deploy"}},
				{Name: "deployments/status", Namespaced: true, Kind: "Deployment"},
				{Name: "statefulsets", SingularName: "statefulset", Namespaced: true, Kind: "StatefulSet", ShortNames: []string{"sts"}},
			},
		},
		{
			GroupVersion: "route.openshift.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "routes", SingularName: "route", Namespaced: true, Kind: "Route"},
			},
		},
		{
			GroupVersion: "lvm.topolvm.io/v1alpha1",
			APIResources: []metav1.APIResource{
				{Name: "lvmclusters", SingularName: "lvmcluster", Namespaced: false, Kind: "LVMCluster"},
			},
		},
		{
			GroupVersion: "oadp.openshift.io/v1alpha1",
			APIResources: []metav1.APIResource{
				{
					Name:         "dataprotectionapplications",
					SingularName: "dataprotectionapplication",
					Namespaced:   true,
					Kind:         "DataProtectionApplication",
					ShortNames:   []string{"dpa"},
				},
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

	dpaGVR := schema.GroupVersionResource{Group: "oadp.openshift.io", Version: "v1alpha1", Resource: "dataprotectionapplications"}

	testcases := []struct {
		name            string
		resourceType    string
		expected        schema.GroupVersionResource
		expectNamespace bool
		expectFound     bool
		expectErr       bool
	}{
		{
			name:            "bare 'deployments' resolves to apps group",
			resourceType:    "deployments",
			expected:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "qualified 'deployments.apps' uses explicit group",
			resourceType:    "deployments.apps",
			expected:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "singular 'deployment' resolves to canonical plural",
			resourceType:    "deployment",
			expected:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "short name 'deploy' resolves to canonical plural",
			resourceType:    "deploy",
			expected:        schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "bare 'secrets' resolves to core group",
			resourceType:    "secrets",
			expected:        schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:         "cluster-scoped 'namespaces' reports non-namespaced",
			resourceType: "namespaces",
			expected:     schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"},
			expectFound:  true,
		},
		{
			name:            "qualified 'routes.route.openshift.io' uses explicit group",
			resourceType:    "routes.route.openshift.io",
			expected:        schema.GroupVersionResource{Group: "route.openshift.io", Version: "v1", Resource: "routes"},
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "bare CRD 'lvmclusters' discovers non-core group and preferred version",
			resourceType:    "lvmclusters",
			expected:        schema.GroupVersionResource{Group: "lvm.topolvm.io", Version: "v1alpha1", Resource: "lvmclusters"},
			expectFound:     true,
			expectNamespace: false,
		},
		{
			name:            "plural 'dataprotectionapplications' resolves to canonical GVR",
			resourceType:    "dataprotectionapplications",
			expected:        dpaGVR,
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "singular 'dataprotectionapplication' resolves to canonical plural",
			resourceType:    "dataprotectionapplication",
			expected:        dpaGVR,
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "Kind-like 'DataProtectionApplication' resolves case-insensitively",
			resourceType:    "DataProtectionApplication",
			expected:        dpaGVR,
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "short name 'dpa' resolves to canonical plural",
			resourceType:    "dpa",
			expected:        dpaGVR,
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:            "qualified short name 'dpa.oadp.openshift.io' resolves to canonical plural",
			resourceType:    "dpa.oadp.openshift.io",
			expected:        dpaGVR,
			expectNamespace: true,
			expectFound:     true,
		},
		{
			name:         "ambiguous bare name matching multiple groups returns error",
			resourceType: "widgets",
			expectErr:    true,
		},
		{
			name:            "qualified 'widgets.example.io' disambiguates by explicit group",
			resourceType:    "widgets.example.io",
			expected:        schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"},
			expectNamespace: true,
			expectFound:     true,
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
			result, namespaced, found, err := resolveResourceGVR(tc.resourceType, apiResourceLists)
			if tc.expectErr {
				assert.Error(t, err)
				assert.False(t, found)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.expectFound, found)
			if tc.expectFound {
				assert.Equal(t, tc.expected, result)
				assert.Equal(t, tc.expectNamespace, namespaced)
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

	t.Run("negative apply-wave does not panic and groups correctly", func(t *testing.T) {
		specs := []BackupSpec{
			{Name: "b", ApplyWave: "0"},
			{Name: "a", ApplyWave: "-1"},
			{Name: "c", ApplyWave: "-1"},
		}
		groups, err := SortBackupSpecsByApplyWave(specs)
		assert.NoError(t, err)
		assert.Equal(t, 2, len(groups))
		assert.Equal(t, 2, len(groups[0]))
		assert.Equal(t, "a", groups[0][0].Name)
		assert.Equal(t, "c", groups[0][1].Name)
		assert.Equal(t, 1, len(groups[1]))
		assert.Equal(t, "b", groups[1][0].Name)
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
	// The legacy spec.excludedResources field is ignored: only the scope-specific
	// excluded fields are parsed.
	assert.Equal(t, []string{"secrets"}, specs[0].ExcludedNamespaceScopedResources)
	assert.Equal(t, []string{"persistentvolumes"}, specs[0].ExcludedClusterScopedResources)
}

func TestExcludedGroupResources(t *testing.T) {
	apiResourceLists := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret"},
				{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap", ShortNames: []string{"cm"}},
				{Name: "persistentvolumes", SingularName: "persistentvolume", Namespaced: false, Kind: "PersistentVolume", ShortNames: []string{"pv"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", SingularName: "deployment", Namespaced: true, Kind: "Deployment", ShortNames: []string{"deploy"}},
			},
		},
	}

	t.Run("exclusions are normalized to canonical GroupResource", func(t *testing.T) {
		// "Deployment" (Kind), "pv" (short name) and "configmap" (singular) must
		// all normalize to the same canonical GroupResource as their inclusions.
		excluded := excludedGroupResources(apiResourceLists,
			[]string{"Deployment"}, []string{"pv", "configmap"})

		assert.True(t, excluded["deployments.apps"])
		assert.True(t, excluded["persistentvolumes"])
		assert.True(t, excluded["configmaps"])
		assert.False(t, excluded["secrets"])
	})

	t.Run("unresolvable exclusions are skipped", func(t *testing.T) {
		excluded := excludedGroupResources(apiResourceLists, []string{"doesnotexist"})
		assert.Empty(t, excluded)
	})
}

// backupRestoreCM renders a single configmap holding the given Backup and Restore
// YAML documents, for the pairing-validation tests.
func backupRestoreCM(name string, docs ...string) corev1.ConfigMap {
	data := make(map[string]string, len(docs))
	for i, d := range docs {
		data[fmt.Sprintf("doc%d.yaml", i)] = d
	}
	return corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "openshift-adp"},
		Data:       data,
	}
}

func backupDoc(name string) string {
	return fmt.Sprintf(`apiVersion: velero.io/v1
kind: Backup
metadata:
  name: %s
spec:
  includedNamespaces:
  - app-ns
`, name)
}

func restoreDoc(name, backupName string) string {
	return fmt.Sprintf(`apiVersion: velero.io/v1
kind: Restore
metadata:
  name: %s
spec:
  backupName: %s
`, name, backupName)
}

func TestValidateBackupRestorePairing(t *testing.T) {
	t.Run("valid 1:1 pairing passes", func(t *testing.T) {
		cms := []corev1.ConfigMap{
			backupRestoreCM("cm1", backupDoc("b1"), restoreDoc("r1", "b1")),
			backupRestoreCM("cm2", backupDoc("b2"), restoreDoc("r2", "b2")),
		}
		assert.NoError(t, ValidateBackupRestorePairing(cms))
	})

	t.Run("backup without a restore is rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{backupRestoreCM("cm1", backupDoc("b1"))}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no matching Restore")
	})

	t.Run("restore without a matching backup is rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{backupRestoreCM("cm1", restoreDoc("r1", "b1"))}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not defined in any referenced configmap")
	})

	t.Run("restore with empty backupName is rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{backupRestoreCM("cm1", backupDoc("b1"), restoreDoc("r1", ""))}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "empty spec.backupName")
	})

	t.Run("two restores for one backup are rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{backupRestoreCM("cm1",
			backupDoc("b1"), restoreDoc("r1", "b1"), restoreDoc("r2", "b1"))}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exactly one is required")
	})

	t.Run("duplicate backup name across configmaps is rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{
			backupRestoreCM("cm1", backupDoc("b1"), restoreDoc("r1", "b1")),
			backupRestoreCM("cm2", backupDoc("b1"), restoreDoc("r2", "b1")),
		}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate Backup name")
	})

	t.Run("duplicate restore name across configmaps is rejected", func(t *testing.T) {
		cms := []corev1.ConfigMap{
			backupRestoreCM("cm1", backupDoc("b1"), restoreDoc("r1", "b1")),
			backupRestoreCM("cm2", backupDoc("b2"), restoreDoc("r1", "b2")),
		}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate Restore name")
	})

	t.Run("cross-configmap pairing is allowed", func(t *testing.T) {
		// Backup in cm1, its matching Restore in cm2: the pairing may span
		// ConfigMaps (as was valid before this feature).
		cms := []corev1.ConfigMap{
			backupRestoreCM("cm1", backupDoc("b1")),
			backupRestoreCM("cm2", restoreDoc("r1", "b1")),
		}
		assert.NoError(t, ValidateBackupRestorePairing(cms))
	})

	t.Run("two cross-configmap restores for one backup are rejected", func(t *testing.T) {
		// The 1:1 rule still holds across ConfigMaps: a single Backup may not be
		// named by more than one Restore, even in different ConfigMaps.
		cms := []corev1.ConfigMap{
			backupRestoreCM("cm1", backupDoc("b1"), restoreDoc("r1", "b1")),
			backupRestoreCM("cm2", restoreDoc("r2", "b1")),
		}
		err := ValidateBackupRestorePairing(cms)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exactly one is required")
	})
}

func TestValidateRestoreSpecs(t *testing.T) {
	t.Run("restore without existingResourcePolicy passes", func(t *testing.T) {
		specs := []RestoreSpec{{Name: "r1", BackupName: "b1"}}
		assert.NoError(t, ValidateRestoreSpecs(specs))
	})

	t.Run("existingResourcePolicy is rejected", func(t *testing.T) {
		specs := []RestoreSpec{{Name: "r1", BackupName: "b1", ExistingResourcePolicy: "update"}}
		err := ValidateRestoreSpecs(specs)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "existingResourcePolicy")
	})
}

func TestExtractRestoreSpecsParsesExistingResourcePolicy(t *testing.T) {
	cm := corev1.ConfigMap{
		Data: map[string]string{
			"restore.yaml": `
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: my-restore
spec:
  backupName: my-backup
  existingResourcePolicy: update
`,
		},
	}
	specs, err := ExtractRestoreSpecsFromConfigmaps([]corev1.ConfigMap{cm})
	assert.NoError(t, err)
	assert.Len(t, specs, 1)
	assert.Equal(t, "update", specs[0].ExistingResourcePolicy)
	assert.Error(t, ValidateRestoreSpecs(specs))
}

func TestValidateBackupSpecs(t *testing.T) {
	t.Run("specs without legacy field pass", func(t *testing.T) {
		specs := []BackupSpec{
			{Name: "b1", IncludedNamespaces: []string{"app-ns"}, IncludedNamespaceScopedResources: []string{"configmaps"}},
			{Name: "b2", IncludedClusterScopedResources: []string{"clusterroles"}},
			{Name: "b3", ApplyLabel: "v1/configmaps/my-cm"},
		}
		assert.NoError(t, ValidateBackupSpecs(specs))
	})

	t.Run("namespace-scoped resources without included namespaces is rejected", func(t *testing.T) {
		// Only namespace-scoped resource types but no included namespaces: nothing
		// would be queried.
		specs := []BackupSpec{
			{Name: "b1", IncludedNamespaceScopedResources: []string{"configmaps"}},
		}
		err := ValidateBackupSpecs(specs)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "without spec.includedNamespaces")
	})

	t.Run("included namespaces without namespace-scoped resources is rejected", func(t *testing.T) {
		// Namespaces are included without the apply-label annotation, so the
		// namespace-scoped resource types must be listed explicitly.
		specs := []BackupSpec{
			{Name: "b1", IncludedNamespaces: []string{"app-ns"}},
		}
		err := ValidateBackupSpecs(specs)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "without spec.includedNamespaceScopedResources")
	})

	t.Run("apply-label with included namespaces but no namespace-scoped resources passes", func(t *testing.T) {
		// With the apply-label annotation the scope is limited to the annotated
		// objects, so the namespace-scoped resource list is not required.
		specs := []BackupSpec{
			{Name: "b1", ApplyLabel: "v1/configmaps/app-ns/my-cm", IncludedNamespaces: []string{"app-ns"}},
		}
		assert.NoError(t, ValidateBackupSpecs(specs))
	})

	t.Run("empty backup is rejected", func(t *testing.T) {
		err := ValidateBackupSpecs([]BackupSpec{{Name: "b1"}})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "selects no resources")
	})
}

// TestExtractBackupSpecsIgnoresLegacyIncludedResources verifies that the legacy
// Velero spec.includedResources field is ignored during parsing: it does not
// contribute to the resource selection, so a Backup whose only resource field is
// includedResources is treated as selecting nothing and is rejected by validation.
func TestExtractBackupSpecsIgnoresLegacyIncludedResources(t *testing.T) {
	cm := corev1.ConfigMap{
		Data: map[string]string{
			"backup.yaml": `
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: legacy-backup
spec:
  includedResources:
    - deployments
    - configmaps
`,
		},
	}
	specs, err := ExtractBackupSpecsFromConfigmaps([]corev1.ConfigMap{cm})
	assert.NoError(t, err)
	assert.Len(t, specs, 1)
	// includedResources is ignored, so the spec selects no resources and fails
	// validation for that reason.
	err = ValidateBackupSpecs(specs)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "selects no resources")
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
