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

package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const lcaNs = "openshift-lifecycle-agent"

var (
	testscheme = scheme.Scheme
)

func init() {
	testscheme.AddKnownTypes(ranv1alpha1.GroupVersion, &ranv1alpha1.ImageBasedUpgrade{})
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

func TestImageBasedUpgradeReconciler_Reconcile(t *testing.T) {
	testcases := []struct {
		name         string
		ibu          client.Object
		request      reconcile.Request
		validateFunc func(t *testing.T, result ctrl.Result, ibu *ranv1alpha1.ImageBasedUpgrade)
	}{
		{
			name: "idle IBU",
			ibu: &ranv1alpha1.ImageBasedUpgrade{
				ObjectMeta: v1.ObjectMeta{
					Name:      utils.IBUName,
					Namespace: lcaNs,
				},
				Spec: ranv1alpha1.ImageBasedUpgradeSpec{
					Stage: ranv1alpha1.Stages.Idle,
				},
			},
			request: reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      utils.IBUName,
					Namespace: lcaNs,
				},
			},
			validateFunc: func(t *testing.T, result ctrl.Result, ibu *ranv1alpha1.ImageBasedUpgrade) {
				idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
				assert.Equal(t, idleCondition.Status, metav1.ConditionTrue)
				if result != doNotRequeue() {
					t.Errorf("expect no requeue")
				}
			},
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: v1.ObjectMeta{
			Name: lcaNs,
		},
	}
	for _, tc := range testcases {
		t.TempDir()
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{ns, tc.ibu}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			r := &ImageBasedUpgradeReconciler{
				Client: fakeClient,
				Log:    logr.Discard(),
				Scheme: fakeClient.Scheme(),
			}
			result, err := r.Reconcile(context.TODO(), tc.request)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			ibu := &ranv1alpha1.ImageBasedUpgrade{}
			if err := fakeClient.Get(context.TODO(), types.NamespacedName{Name: utils.IBUName, Namespace: lcaNs}, ibu); err != nil {
				t.Errorf("unexcepted error: %v", err.Error())
			}
			tc.validateFunc(t, result, ibu)
		})
	}
}
