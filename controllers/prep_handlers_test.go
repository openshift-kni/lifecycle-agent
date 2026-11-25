package controllers

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	mock_backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore/mocks"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
			client := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(version).Build()
			r := &ImageBasedUpgradeReconciler{
				Client:          client,
				NoncachedClient: client,
				Log:             logr.Logger{},
			}

			err := r.validateSeedOcpVersion(tt.args.seedOcpVersion)
			tt.wantErr(t, err, fmt.Sprintf("validateSeedOcpVersion(%v)", tt.args.seedOcpVersion))
			if err != nil {
				assert.Equal(t, tt.wantErrMsg, err.Error())
			}
		})
	}
}

// newBackupContentValidateReconciler builds a reconciler whose client knows the
// cluster version (so validateSeedOcpVersion passes) and the IBU (so the warning
// annotation patch can be applied), wired to a mock BackuperRestorer.
func newBackupContentValidateReconciler(t *testing.T, ibu *ibuv1.ImageBasedUpgrade, mockBR *mock_backuprestore.MockBackuperRestorer) *ImageBasedUpgradeReconciler {
	s := scheme.Scheme
	assert.NoError(t, configv1.AddToScheme(s))
	assert.NoError(t, ibuv1.AddToScheme(s))
	version := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{Name: "version"},
		Status:     configv1.ClusterVersionStatus{Desired: configv1.Release{Version: "4.14.8"}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(version, ibu).Build()
	return &ImageBasedUpgradeReconciler{
		Client:          c,
		NoncachedClient: c,
		Log:             logr.Discard(),
		BackupRestore:   mockBR,
	}
}

func newBackupContentIBU() *ibuv1.ImageBasedUpgrade {
	return &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade"},
		Spec: ibuv1.ImageBasedUpgradeSpec{
			SeedImageRef:  ibuv1.SeedImageRef{Version: "4.14.9"},
			BackupContent: []ibuv1.BackupContentRef{{Name: "bc1"}},
		},
	}
}

// TestValidateIBUSpecBackupContentTerminal verifies a terminal BackupContent
// finding fails Prep (surfaced as a BRFailedValidationError).
func TestValidateIBUSpecBackupContentTerminal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl)
	ibu := newBackupContentIBU()
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	mockBR.EXPECT().
		ValidateBackupContent(gomock.Any(), ibu.Spec.BackupContent).
		Return(nil, backuprestore.NewBRFailedValidationError("Backup", "widgets not found"))

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.Error(t, err)
	assert.True(t, backuprestore.IsBRFailedValidationError(err), "terminal finding must be a validation error")

	var retryable *retryableValidationError
	assert.False(t, errors.As(err, &retryable), "a terminal finding must not be retryable")
}

// TestValidateIBUSpecBackupContentWarning verifies a warning-only BackupContent
// result completes Prep and records the warning annotation on the IBU.
func TestValidateIBUSpecBackupContentWarning(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl)
	ibu := newBackupContentIBU()
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	mockBR.EXPECT().
		ValidateBackupContent(gomock.Any(), ibu.Spec.BackupContent).
		Return([]string{`source "bc1" namespaces: declared namespace "missing-ns" does not exist`}, nil)

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.NoError(t, err)
	assert.Contains(t, ibu.GetAnnotations(), backuprestore.BackupContentValidationWarningAnnotation)
	assert.Contains(t, ibu.GetAnnotations()[backuprestore.BackupContentValidationWarningAnnotation], "missing-ns")
}

// TestValidateIBUSpecBackupContentClearsStaleWarning verifies that when a later
// validation within the same Prep cycle (e.g. a retry after container storage
// cleanup failed) reports no warnings, a stale warning annotation from an earlier
// attempt is removed so PrepCompleted does not re-surface a resolved warning.
func TestValidateIBUSpecBackupContentClearsStaleWarning(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl)
	ibu := newBackupContentIBU()
	ibu.SetAnnotations(map[string]string{
		backuprestore.BackupContentValidationWarningAnnotation: "stale warning from a previous attempt",
		"unrelated.lca.openshift.io/keep":                      "value",
	})
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	mockBR.EXPECT().
		ValidateBackupContent(gomock.Any(), ibu.Spec.BackupContent).
		Return(nil, nil)

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.NoError(t, err)
	assert.NotContains(t, ibu.GetAnnotations(), backuprestore.BackupContentValidationWarningAnnotation,
		"a now-clean validation must drop the stale warning annotation")
	assert.Contains(t, ibu.GetAnnotations(), "unrelated.lca.openshift.io/keep",
		"unrelated annotations must be preserved")
}

// TestValidateIBUSpecBackupContentNotConfiguredClearsStaleWarning verifies that
// when BackupContent is no longer configured, a stale warning annotation is removed
// without running validation.
func TestValidateIBUSpecBackupContentNotConfiguredClearsStaleWarning(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl) // no validation call expected
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name: "upgrade",
			Annotations: map[string]string{
				backuprestore.BackupContentValidationWarningAnnotation: "stale warning",
			},
		},
		Spec: ibuv1.ImageBasedUpgradeSpec{SeedImageRef: ibuv1.SeedImageRef{Version: "4.14.9"}},
	}
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.NoError(t, err)
	assert.NotContains(t, ibu.GetAnnotations(), backuprestore.BackupContentValidationWarningAnnotation,
		"removing BackupContent must drop the stale warning annotation")
}

// TestValidateIBUSpecBackupContentTransient verifies a transient failure is wrapped
// as retryable so Prep requeues instead of failing terminally.
func TestValidateIBUSpecBackupContentTransient(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl)
	ibu := newBackupContentIBU()
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	mockBR.EXPECT().
		ValidateBackupContent(gomock.Any(), ibu.Spec.BackupContent).
		Return(nil, fmt.Errorf("the server could not be reached"))

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.Error(t, err)
	assert.False(t, backuprestore.IsBRFailedValidationError(err))
	var retryable *retryableValidationError
	assert.True(t, errors.As(err, &retryable), "a transient failure must be retryable")
}

// TestValidateIBUSpecNoBackupSource verifies that with neither backup source
// configured, no BackupContent validation runs and Prep validation passes.
func TestValidateIBUSpecNoBackupSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockBR := mock_backuprestore.NewMockBackuperRestorer(ctrl) // no calls expected
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrade"},
		Spec:       ibuv1.ImageBasedUpgradeSpec{SeedImageRef: ibuv1.SeedImageRef{Version: "4.14.9"}},
	}
	r := newBackupContentValidateReconciler(t, ibu, mockBR)

	err := r.validateIBUSpec(context.Background(), ibu)
	assert.NoError(t, err)
}

// TestPrepSuccessMessage verifies the PrepCompleted message names the validated
// BackupContent CRs, reports when no backup is configured, and surfaces warnings.
func TestPrepSuccessMessage(t *testing.T) {
	assert.NoError(t, ibuv1.AddToScheme(scheme.Scheme))

	t.Run("names validated BackupContent", func(t *testing.T) {
		ibu := newBackupContentIBU()
		_, err := prepSuccessDoNotRequeue(logr.Discard(), ibu)
		assert.NoError(t, err)
		msg := prepCompletedMessage(t, ibu)
		assert.Contains(t, msg, "validated BackupContent: bc1")
	})

	t.Run("no application backup configured", func(t *testing.T) {
		ibu := &ibuv1.ImageBasedUpgrade{ObjectMeta: metav1.ObjectMeta{Name: "upgrade"}}
		_, err := prepSuccessDoNotRequeue(logr.Discard(), ibu)
		assert.NoError(t, err)
		msg := prepCompletedMessage(t, ibu)
		assert.Contains(t, msg, "no application backup configured")
	})

	t.Run("surfaces BackupContent warning annotation", func(t *testing.T) {
		ibu := newBackupContentIBU()
		ibu.SetAnnotations(map[string]string{backuprestore.BackupContentValidationWarningAnnotation: "x"})
		_, err := prepSuccessDoNotRequeue(logr.Discard(), ibu)
		assert.NoError(t, err)
		msg := prepCompletedMessage(t, ibu)
		assert.Contains(t, msg, "BackupContent validation warning")
	})
}

// prepCompletedMessage returns the message of the PrepCompleted condition.
func prepCompletedMessage(t *testing.T, ibu *ibuv1.ImageBasedUpgrade) string {
	t.Helper()
	for _, c := range ibu.Status.Conditions {
		if c.Type == "PrepCompleted" {
			return c.Message
		}
	}
	return ""
}
