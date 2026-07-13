/*
Copyright 2023.

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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	testscheme = scheme.Scheme
)

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

func TestGetObjsFromApplyLabel(t *testing.T) {
	testcases := []struct {
		name       string
		annotation string
		expected   []ObjMetadata
	}{
		{
			name:       "one name spaced resource",
			annotation: "apps/v1/deployments/default/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "deployments",
					Namespace: "default",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "one cluster resource, one namespaced",
			annotation: "apps/v1/clusterroles/klusterlet,apps/v1/deployment/ns/klusterlet-deploy",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "deployment",
					Namespace: "ns",
					Name:      "klusterlet-deploy",
				},
			},
		},
		{
			name:       "two cluster resources",
			annotation: "apps/v1/clusterroles/klusterlet,apps/v1/clusterroles/klusterlet2",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet2",
				},
			},
		},
		{
			name:       "one cluster resources",
			annotation: "apps/v1/clusterroles/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "object without group and namespace",
			annotation: "v1/namespace/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "",
					Version:   "v1",
					Resource:  "namespace",
					Namespace: "",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "object without group",
			annotation: "v1/secrets/default/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "",
					Version:   "v1",
					Resource:  "secrets",
					Namespace: "default",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "empty",
			annotation: "",
			expected:   []ObjMetadata(nil),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := getObjsFromApplyLabel(tc.annotation)
			assert.Equal(t, tc.expected, result)
			assert.NoError(t, err)
		})
	}
}

func TestPatchPVsReclaimPolicy(t *testing.T) {
	testcases := []struct {
		name          string
		pvs           []client.Object
		expectedPatch map[string]corev1.PersistentVolumeReclaimPolicy
	}{
		{
			name: "No PVs",
			pvs:  []client.Object{},
		},
		{
			name: "PV with topolvm annotation and Delete policy gets patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv1",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv1": corev1.PersistentVolumeReclaimRetain,
			},
		},
		{
			name: "PV without topolvm annotation is not patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pv2",
						Annotations: map[string]string{},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv2": corev1.PersistentVolumeReclaimDelete,
			},
		},
		{
			name: "PV with topolvm annotation but Retain policy is not patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv3",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv3": corev1.PersistentVolumeReclaimRetain,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(tc.pvs...)
			if err != nil {
				t.Fatalf("error creating fake client: %v", err)
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			err = handler.PatchPVsReclaimPolicy(context.Background())
			assert.NoError(t, err)

			// Verify results
			pvList := &corev1.PersistentVolumeList{}
			err = fakeClient.List(context.Background(), pvList)
			assert.NoError(t, err)

			for _, pv := range pvList.Items {
				if expected, ok := tc.expectedPatch[pv.Name]; ok {
					assert.Equal(t, expected, pv.Spec.PersistentVolumeReclaimPolicy, "PV %s", pv.Name)
				}
			}
		})
	}
}

func TestRestorePVsReclaimPolicy(t *testing.T) {
	testcases := []struct {
		name          string
		pvs           []client.Object
		expectedPatch map[string]corev1.PersistentVolumeReclaimPolicy
	}{
		{
			name: "No PVs",
			pvs:  []client.Object{},
		},
		{
			name: "PV with updated-reclaim-policy annotation gets restored",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv1",
						Annotations: map[string]string{
							updatedReclaimPolicyAnnotation: "true",
							topolvmAnnotation:              topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv1": corev1.PersistentVolumeReclaimDelete,
			},
		},
		{
			name: "PV without updated-reclaim-policy annotation is not changed",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv2",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv2": corev1.PersistentVolumeReclaimRetain,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(tc.pvs...)
			if err != nil {
				t.Fatalf("error creating fake client: %v", err)
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			err = handler.RestorePVsReclaimPolicy(context.Background())
			assert.NoError(t, err)

			// Verify results
			pvList := &corev1.PersistentVolumeList{}
			err = fakeClient.List(context.Background(), pvList)
			assert.NoError(t, err)

			for _, pv := range pvList.Items {
				if expected, ok := tc.expectedPatch[pv.Name]; ok {
					assert.Equal(t, expected, pv.Spec.PersistentVolumeReclaimPolicy, "PV %s", pv.Name)
				}
			}
		})
	}
}
