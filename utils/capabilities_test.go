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

package utils

import (
	"context"
	"strconv"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterclaimv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	ctrlutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
)

func capabilitiesScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, ibuv1.AddToScheme(s))
	require.NoError(t, clusterclaimv1alpha1.Install(s))
	return s
}

// noMatchReader is a client.Reader whose Get always reports that the requested
// kind has no REST mapping, i.e. the CRD is not installed. It simulates a spoke
// where the klusterlet (and therefore the ClusterClaim CRD) is absent.
type noMatchReader struct{}

func (noMatchReader) Get(_ context.Context, _ runtimeclient.ObjectKey, _ runtimeclient.Object, _ ...runtimeclient.GetOption) error {
	return &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: clusterclaimv1alpha1.GroupName, Kind: "ClusterClaim"},
		SearchedVersions: []string{"v1alpha1"},
	}
}

func (noMatchReader) List(_ context.Context, _ runtimeclient.ObjectList, _ ...runtimeclient.ListOption) error {
	return nil
}

func TestPublishSeedFormatVersion_SetsIBUStatus(t *testing.T) {
	s := capabilitiesScheme(t)
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: ctrlutils.IBUName},
		Spec:       ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Idle},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ibu).WithStatusSubresource(ibu).Build()
	log := logr.Discard()

	require.NoError(t, PublishSeedFormatVersion(context.Background(), c, &log))

	got := &ibuv1.ImageBasedUpgrade{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ctrlutils.IBUName}, got))
	assert.Equal(t, ibuv1.SeedFormatVersion, got.Status.SeedFormatVersion)
}

func TestPublishSeedFormatVersion_OverwritesRestoredStatus(t *testing.T) {
	s := capabilitiesScheme(t)
	// Simulate a post-pivot restore: the saved IBU carries the previous LCA's
	// (stale) seed format version in its status.
	stale := ibuv1.SeedFormatVersion + 99
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: ctrlutils.IBUName},
		Spec:       ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Idle},
		Status:     ibuv1.ImageBasedUpgradeStatus{SeedFormatVersion: stale},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ibu).WithStatusSubresource(ibu).Build()
	log := logr.Discard()

	require.NoError(t, PublishSeedFormatVersion(context.Background(), c, &log))

	got := &ibuv1.ImageBasedUpgrade{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ctrlutils.IBUName}, got))
	assert.Equal(t, ibuv1.SeedFormatVersion, got.Status.SeedFormatVersion)
}

func TestEnsureSeedFormatClaim_CreatesWhenMissing(t *testing.T) {
	s := capabilitiesScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	log := logr.Discard()

	require.NoError(t, EnsureSeedFormatClaim(context.Background(), c, c, &log))

	got := &clusterclaimv1alpha1.ClusterClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ibuv1.SeedFormatClusterClaimName}, got))
	assert.Equal(t, strconv.Itoa(ibuv1.SeedFormatVersion), got.Spec.Value)
}

func TestEnsureSeedFormatClaim_PatchesDriftedValue(t *testing.T) {
	s := capabilitiesScheme(t)
	existing := &clusterclaimv1alpha1.ClusterClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ibuv1.SeedFormatClusterClaimName},
		Spec:       clusterclaimv1alpha1.ClusterClaimSpec{Value: "999"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()
	log := logr.Discard()

	require.NoError(t, EnsureSeedFormatClaim(context.Background(), c, c, &log))

	got := &clusterclaimv1alpha1.ClusterClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: ibuv1.SeedFormatClusterClaimName}, got))
	assert.Equal(t, strconv.Itoa(ibuv1.SeedFormatVersion), got.Spec.Value)
}

// TestEnsureSeedFormatClaim_ToleratesMissingCRD is the regression guard for the
// crash-loop a watch-based controller would have caused: when the ClusterClaim
// CRD is absent, the helper must be a no-op and return no error.
func TestEnsureSeedFormatClaim_ToleratesMissingCRD(t *testing.T) {
	s := capabilitiesScheme(t)
	writer := fake.NewClientBuilder().WithScheme(s).Build()
	log := logr.Discard()

	err := EnsureSeedFormatClaim(context.Background(), noMatchReader{}, writer, &log)
	require.NoError(t, err)

	// Nothing should have been created.
	got := &clusterclaimv1alpha1.ClusterClaim{}
	getErr := writer.Get(context.Background(), types.NamespacedName{Name: ibuv1.SeedFormatClusterClaimName}, got)
	assert.True(t, apierrors.IsNotFound(getErr), "expected no ClusterClaim to be created, got err=%v", getErr)
}
