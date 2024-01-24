package postpivot

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/google/uuid"
	clusterconfig_api "github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	openshift_api_v1 "github.com/openshift/api/config/v1"

	"github.com/sirupsen/logrus"
)

func TestSetNewClusterId(t *testing.T) {
	testcases := []struct {
		name          string
		expectedError bool
		config        clusterconfig_api.SeedReconfiguration
	}{
		{
			name:   "cluster id set",
			config: clusterconfig_api.SeedReconfiguration{ClusterID: uuid.New().String()},
		},
		{
			name:   "cluster id is empty",
			config: clusterconfig_api.SeedReconfiguration{},
		},
	}
	ctx := context.Background()
	seedClusterID := uuid.New().String()
	clusterVersion := &openshift_api_v1.ClusterVersion{
		ObjectMeta: v1.ObjectMeta{Name: "version"},
		Spec:       openshift_api_v1.ClusterVersionSpec{ClusterID: openshift_api_v1.ClusterID(seedClusterID)}}
	var schemes = runtime.NewScheme()
	utilruntime.Must(openshift_api_v1.Install(schemes))
	fakeClient := fake.NewClientBuilder().WithScheme(schemes).WithObjects([]client.Object{clusterVersion}...).Build()
	for _, tc := range testcases {
		log := logrus.Logger{}
		ctrl := gomock.NewController(t)
		pp := NewPostPivot(scheme.Scheme, &log, ops.NewOps(&log, ops.NewMockExecute(ctrl)),
			common.ImageRegistryAuthFile, common.OptOpenshift, common.KubeconfigFile)
		t.Run(tc.name, func(t *testing.T) {
			pp.setNewClusterID(ctx, fakeClient, &tc.config)
			clusterVersion = &openshift_api_v1.ClusterVersion{}
			if err := fakeClient.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
				t.Errorf(err.Error())
			}
			if tc.config.ClusterID != "" {
				if string(clusterVersion.Spec.ClusterID) != tc.config.ClusterID {
					t.Errorf("Cluster id mismatch, expected %s got %s", tc.config.ClusterID, clusterVersion.Spec.ClusterID)
				}
			}
			if string(clusterVersion.Spec.ClusterID) == seedClusterID {
				t.Errorf("Cluster id is the same as the seed cluster id: %s", clusterVersion.Spec.ClusterID)
			}

		})
	}
}
