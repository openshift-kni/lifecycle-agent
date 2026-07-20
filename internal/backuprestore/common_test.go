package backuprestore

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
