/*
 * Copyright 2023 Red Hat, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this inputFilePath except in compliance with the License.
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

package precache

import (
	"context"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const precacheWorkloadImage string = "quay.io/openshift-kni/lifecycle-agent-operator-workload:test"

var (
	testScheme = scheme.Scheme
)

func init() {
	os.Setenv(EnvLcaPrecacheImage, precacheWorkloadImage)
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

func TestCreateJobAndConfigMap(t *testing.T) {
	imageList, imageListStr := generateImageList()
	testCases := []struct {
		name               string
		config             *Config
		inputConfigMapName string
		inputJobName       string
		expectedError      error
		expectedConfigMap  *corev1.ConfigMap
		expectedJob        *batchv1.Job
	}{
		{
			name: "No image list provided",
			config: &Config{
				ImageList: []string{},
			},
			inputConfigMapName: "",
			inputJobName:       "",
			expectedError:      assert.AnError,
			expectedConfigMap:  nil,
			expectedJob:        nil,
		},
		{
			name: "Success case",
			config: &Config{
				ImageList:          imageList,
				NumConcurrentPulls: 5,
				IoNicePriority:     1,
			},
			inputConfigMapName: "",
			inputJobName:       "",
			expectedError:      nil,
			expectedConfigMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      LcaPrecacheResourceName,
					Namespace: common.LcaNamespace,
				},
				Data: map[string]string{
					PrecachingSpecFilename: imageListStr,
				},
			},
			expectedJob: &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      LcaPrecacheResourceName,
					Namespace: common.LcaNamespace,
				},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "workload",
									Image: precacheWorkloadImage,
									Env: []corev1.EnvVar{
										{
											Name:  EnvMaxPullThreads,
											Value: "5",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}
			ibu := ibuv1.ImageBasedUpgrade{}
			sc := runtime.NewScheme()
			_ = ibuv1.AddToScheme(sc)

			Log := ctrl.Log.WithName("Precache")

			// Inject ConfigMap, Job
			if tc.inputConfigMapName != "" {
				cm := renderConfigMap(tc.config.ImageList)
				objs = append(objs, cm)
			}
			if tc.inputJobName != "" {
				job, err := renderJob(tc.config, Log, &ibu, sc)
				assert.NoError(t, err)
				objs = append(objs, job)
			}

			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			handler := &PHandler{
				Client: fakeClient,
				Log:    Log,
				Scheme: sc,
			}

			err = handler.CreateJobAndConfigMap(context.TODO(), tc.config, &ibu)
			if tc.expectedError != nil {
				assert.NotNil(t, err)
			} else {
				assert.NoError(t, err)

				actualConfigMap, err := common.GetConfigMap(context.TODO(), fakeClient, ibuv1.ConfigMapRef{
					Name:      LcaPrecacheResourceName,
					Namespace: common.LcaNamespace,
				})
				assert.NoError(t, err)
				assert.NotNil(t, actualConfigMap)

				actualJob, err := GetJob(context.TODO(), fakeClient)
				assert.NoError(t, err)
				assert.NotNil(t, actualJob)

				// Validate ConfigMap
				assert.Equal(t, tc.expectedConfigMap.ObjectMeta.Name, actualConfigMap.ObjectMeta.Name)
				assert.Equal(t, tc.expectedConfigMap.Data, actualConfigMap.Data)

				// Validate Job
				assert.Equal(t, actualJob.ObjectMeta.Name, tc.expectedJob.ObjectMeta.Name)
				// Check few specs
				assert.Equal(t, len(tc.expectedJob.Spec.Template.Spec.Containers), len(actualJob.Spec.Template.Spec.Containers))
				assert.Equal(t, tc.expectedJob.Spec.Template.Spec.Containers[0].Image, actualJob.Spec.Template.Spec.Containers[0].Image)
				for _, env := range tc.expectedJob.Spec.Template.Spec.Containers[0].Env {
					assert.Contains(t, actualJob.Spec.Template.Spec.Containers[0].Env, env)
				}

			}
		})
	}
}

func TestCleanup(t *testing.T) {
	imageList, _ := generateImageList()
	config := &Config{ImageList: imageList}
	testCases := []struct {
		name               string
		inputConfigMapName string
		inputJobName       string
		expectedError      error
	}{
		{
			name:               "Missing ConfigMap",
			inputConfigMapName: "",
			inputJobName:       LcaPrecacheResourceName,
			expectedError:      nil,
		},
		{
			name:               "Missing Job",
			inputConfigMapName: LcaPrecacheResourceName,
			inputJobName:       "",
			expectedError:      nil,
		},
		{
			name:               "Success case",
			inputConfigMapName: LcaPrecacheResourceName,
			inputJobName:       LcaPrecacheResourceName,
			expectedError:      nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}

			Log := ctrl.Log.WithName("Precache")

			ibu := ibuv1.ImageBasedUpgrade{}
			sc := runtime.NewScheme()
			_ = ibuv1.AddToScheme(sc)

			// Inject ConfigMap, Job
			if tc.inputConfigMapName != "" {
				cm := renderConfigMap(config.ImageList)
				objs = append(objs, cm)
			}
			if tc.inputJobName != "" {
				job, err := renderJob(config, Log, &ibu, sc)
				assert.NoError(t, err)
				objs = append(objs, job)
			}

			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			handler := &PHandler{
				Client: fakeClient,
				Log:    Log,
			}

			err = handler.Cleanup(context.TODO())
			if tc.expectedError != nil {
				assert.NotNil(t, err)
			} else {
				assert.NoError(t, err)

				actualConfigMap, err := common.GetConfigMap(context.TODO(), fakeClient, ibuv1.ConfigMapRef{
					Name:      LcaPrecacheResourceName,
					Namespace: common.LcaNamespace,
				})
				assert.Equal(t, true, k8serrors.IsNotFound(err))
				assert.Nil(t, actualConfigMap)

				_, err = GetJob(context.TODO(), fakeClient)
				assert.Equal(t, true, k8serrors.IsNotFound(err))
			}
		})
	}
}
