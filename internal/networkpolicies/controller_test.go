/*
 * Copyright 2026 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package networkpolicies

import (
	"context"
	"testing"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testPodName = "lifecycle-agent-controller-manager-0"

// testOwnerRef is the owning deployment reference carried by the controller-manager pod.
func testOwnerRef() metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := true

	return metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "Deployment",
		Name:               "lifecycle-agent-controller-manager",
		UID:                "uid-1234",
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

// copiedOwnerRef is testOwnerRef as getOwnerReference returns it, with BlockOwnerDeletion cleared.
func copiedOwnerRef() metav1.OwnerReference {
	ref := testOwnerRef()
	ref.BlockOwnerDeletion = nil

	return ref
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

// newBrokenSchemeClient returns a client whose scheme cannot represent NetworkPolicy, so every
// create and list through it fails with a non-AlreadyExists error.
func newBrokenSchemeClient() client.Client {
	return fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
}

// invalidClientConfig makes client.New fail while parsing the CA, before it dials or discovers.
func invalidClientConfig() *rest.Config {
	return &rest.Config{
		Host:            "https://127.0.0.1:1",
		TLSClientConfig: rest.TLSClientConfig{CAData: []byte("not a certificate")},
	}
}

func Test_parsePolicy(t *testing.T) {
	tcp := corev1.ProtocolTCP
	metricsPort := intstr.FromInt(9443)

	tests := []struct {
		name     string
		template string
		args     []any
		want     *networkingv1.NetworkPolicy
		wantErr  string
	}{
		{
			name:     "controller template",
			template: controllerTmpl,
			args:     []any{common.DefaultOperatorNamespace, "9443"},
			want: &networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "controller-manager-metrics",
					Namespace: common.DefaultOperatorNamespace,
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"control-plane": "controller-manager"},
					},
					Ingress: []networkingv1.NetworkPolicyIngressRule{{
						Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &metricsPort}},
					}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				},
			},
		},
		{
			name:     "job template (stateroot setup)",
			template: jobTmpl,
			args:     []any{prep.StaterootSetupJobName, common.DefaultOperatorNamespace},
			want: &networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      prep.StaterootSetupJobName,
					Namespace: common.DefaultOperatorNamespace,
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"job-name": prep.StaterootSetupJobName},
					},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				},
			},
		},
		{
			name:     "job template (precache)",
			template: jobTmpl,
			args:     []any{precache.LcaPrecacheResourceName, common.DefaultOperatorNamespace},
			want: &networkingv1.NetworkPolicy{
				TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      precache.LcaPrecacheResourceName,
					Namespace: common.DefaultOperatorNamespace,
				},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"job-name": precache.LcaPrecacheResourceName},
					},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				},
			},
		},
		{
			name:     "invalid YAML fails loudly",
			template: "%s",
			args:     []any{"this: [is not valid"},
			wantErr:  "failed to unmarshal NetworkPolicy YAML",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePolicy(tt.template, tt.args...)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_parsePort(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
	}{
		{name: "host and port", address: "0.0.0.0:8080", want: "8080"},
		{name: "wildcard host", address: ":9443", want: "9443"},
		{name: "IPv6 host", address: "[::]:8443", want: "8443"},
		{name: "hostname", address: "localhost:6443", want: "6443"},
		{name: "malformed falls back to metrics port", address: "definitely-not-an-address", want: "6443"},
		{name: "empty falls back to metrics port", address: "", want: "6443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parsePort(tt.address))
		})
	}
}

func Test_InstallPolicies_clientError(t *testing.T) {
	msg, err := (&Policy{Namespace: common.DefaultOperatorNamespace, MetricAddr: ":9443"}).InstallPolicies(invalidClientConfig())
	require.Error(t, err)
	assert.Empty(t, msg)
	assert.Contains(t, err.Error(), "failed to create direct client")
}

func Test_Check_clientError(t *testing.T) {
	assert.Contains(t, Check(invalidClientConfig(), common.DefaultOperatorNamespace), "failed to create k8s client")
}

func Test_installPolicies(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("HOSTNAME", "")

	t.Run("creates the expected policies", func(t *testing.T) {
		c := newFakeClient(t)

		got, err := installPolicies(c, common.DefaultOperatorNamespace, ":9443")
		require.NoError(t, err)
		assert.Equal(t, "NetworkPolicies Installed: controller-manager-metrics, "+
			prep.StaterootSetupJobName+", "+precache.LcaPrecacheResourceName, got)

		list := &networkingv1.NetworkPolicyList{}
		require.NoError(t, c.List(context.Background(), list, client.InNamespace(common.DefaultOperatorNamespace)))
		require.Len(t, list.Items, 3)
	})

	t.Run("tolerates already existing policies", func(t *testing.T) {
		c := newFakeClient(t)

		_, err := installPolicies(c, common.DefaultOperatorNamespace, ":9443")
		require.NoError(t, err)

		// The second install re-creates the same objects; AlreadyExists must not fail the install.
		_, err = installPolicies(c, common.DefaultOperatorNamespace, ":9443")
		require.NoError(t, err)
	})

	t.Run("attaches the pod owner reference to every policy", func(t *testing.T) {
		t.Setenv("POD_NAME", testPodName)
		c := newFakeClient(t, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            testPodName,
				Namespace:       common.DefaultOperatorNamespace,
				OwnerReferences: []metav1.OwnerReference{testOwnerRef()},
			},
		})

		_, err := installPolicies(c, common.DefaultOperatorNamespace, ":9443")
		require.NoError(t, err)

		list := &networkingv1.NetworkPolicyList{}
		require.NoError(t, c.List(context.Background(), list, client.InNamespace(common.DefaultOperatorNamespace)))
		require.Len(t, list.Items, 3)
		for _, item := range list.Items {
			assert.Equal(t, []metav1.OwnerReference{copiedOwnerRef()}, item.OwnerReferences, "policy %s", item.Name)
		}
	})

	t.Run("fails when the client cannot create policies", func(t *testing.T) {
		_, err := installPolicies(newBrokenSchemeClient(), common.DefaultOperatorNamespace, ":9443")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create NetworkPolicy")
	})
}

func Test_checkPolicies(t *testing.T) {
	t.Run("no policies", func(t *testing.T) {
		assert.Empty(t, checkPolicies(newFakeClient(t), common.DefaultOperatorNamespace))
	})

	t.Run("reports only policies in the given namespace", func(t *testing.T) {
		c := newFakeClient(t,
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy-a", Namespace: common.DefaultOperatorNamespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy-b", Namespace: common.DefaultOperatorNamespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "policy-c", Namespace: "other-ns"}},
		)

		assert.Equal(t,
			"NetworkPolicies [policy-a policy-b] detected in namespace ["+common.DefaultOperatorNamespace+
				"] - these may interfere with traffic flow",
			checkPolicies(c, common.DefaultOperatorNamespace))
	})

	t.Run("list failure is reported", func(t *testing.T) {
		assert.Contains(t, checkPolicies(newBrokenSchemeClient(), common.DefaultOperatorNamespace),
			"failed to list network policies")
	})
}

func Test_getOwnerReference(t *testing.T) {
	tests := []struct {
		name     string
		podName  string
		hostname string
		objects  []client.Object
		want     []metav1.OwnerReference
	}{
		{
			name:     "pod carries an owner reference",
			podName:  testPodName,
			hostname: "",
			objects: []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            testPodName,
					Namespace:       common.DefaultOperatorNamespace,
					OwnerReferences: []metav1.OwnerReference{testOwnerRef()},
				},
			}},
			want: []metav1.OwnerReference{copiedOwnerRef()},
		},
		{
			name:     "POD_NAME unset falls back to HOSTNAME",
			podName:  "",
			hostname: testPodName,
			objects: []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            testPodName,
					Namespace:       common.DefaultOperatorNamespace,
					OwnerReferences: []metav1.OwnerReference{testOwnerRef()},
				},
			}},
			want: []metav1.OwnerReference{copiedOwnerRef()},
		},
		{
			name:     "no pod name in environment",
			podName:  "",
			hostname: "",
			want:     []metav1.OwnerReference{},
		},
		{
			name:    "pod not found",
			podName: testPodName,
			want:    []metav1.OwnerReference{},
		},
		{
			name:    "pod without owner references",
			podName: testPodName,
			objects: []client.Object{&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: common.DefaultOperatorNamespace},
			}},
			want: []metav1.OwnerReference{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_NAME", tt.podName)
			t.Setenv("HOSTNAME", tt.hostname)

			assert.Equal(t, tt.want, getOwnerReference(newFakeClient(t, tt.objects...), common.DefaultOperatorNamespace))
		})
	}
}
