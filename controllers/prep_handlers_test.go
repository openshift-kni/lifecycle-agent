package controllers

import (
	"fmt"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

func TestImageBasedUpgradeReconciler_validateSeedOcpVersion(t *testing.T) {
	// fake client setup
	s := scheme.Scheme
	s.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterVersion{})
	version := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Status: configv1.ClusterVersionStatus{
			Desired: configv1.Release{
				Version: "4.14.8",
			},
		},
	}

	type args struct {
		seedOcpVersion string
	}
	tests := []struct {
		name       string
		args       args
		wantErr    assert.ErrorAssertionFunc
		wantErrMsg string
	}{
		{
			name:    "when seed OCP is greater than target cluster",
			args:    args{seedOcpVersion: "4.14.9"},
			wantErr: assert.NoError,
		},
		{
			name:       "when seed OCP is equal to target cluster, version also contains metadata",
			args:       args{seedOcpVersion: "4.14.8+metadata"},
			wantErr:    assert.Error,
			wantErrMsg: "seed OCP version (4.14.8+metadata) must be higher than current OCP version (4.14.8)",
		},
		{
			name:       "when seed OCP is less to target cluster, version is also pre release",
			args:       args{seedOcpVersion: "4.14.7-rc.1"},
			wantErr:    assert.Error,
			wantErrMsg: "seed OCP version (4.14.7-rc.1) must be higher than current OCP version (4.14.8)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			r := &ImageBasedUpgradeReconciler{
				Client: fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(version).Build(),
				Log:    logr.Logger{},
			}

			err := r.validateSeedOcpVersion(tt.args.seedOcpVersion)
			tt.wantErr(t, err, fmt.Sprintf("validateSeedOcpVersion(%v)", tt.args.seedOcpVersion))
			if err != nil {
				assert.Equal(t, tt.wantErrMsg, err.Error())
			}
		})
	}
}
