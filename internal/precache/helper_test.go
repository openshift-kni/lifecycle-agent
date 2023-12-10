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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"github.com/stretchr/testify/assert"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/diff"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	os.Setenv(EnvLcaPrecacheImage, precacheWorkloadImage)
}

func generateImageList() ([]string, string) {
	imageList := []string{
		"precache-test-image1:latest",
		"precache-test-image2:latest",
		"precache-test-image3:latest",
	}
	imageListStr := "precache-test-image1:latest\nprecache-test-image2:latest\nprecache-test-image3:latest\n"
	return imageList, imageListStr
}

func TestRenderConfigMap(t *testing.T) {
	imageList, imageListStr := generateImageList()
	testCases := []struct {
		name               string
		inputConfigMapName string
		inputImageList     []string
		expectedConfigMap  *corev1.ConfigMap
	}{
		{
			name:               "Empty data list",
			inputConfigMapName: LcaPrecacheConfigMapName,
			inputImageList:     []string{},
			expectedConfigMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      LcaPrecacheConfigMapName,
					Namespace: common.LcaNamespace,
				},
				Data: map[string]string{
					PrecachingSpecFilename: "\n",
				},
			},
		},
		{
			name:               "Image data list",
			inputConfigMapName: LcaPrecacheConfigMapName,
			inputImageList:     imageList,
			expectedConfigMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      LcaPrecacheConfigMapName,
					Namespace: common.LcaNamespace,
				},
				Data: map[string]string{
					PrecachingSpecFilename: imageListStr,
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cm := renderConfigMap(tc.inputImageList)
			assert.NotNil(t, cm)

			// Validate ConfigMap
			assert.Equal(t, tc.expectedConfigMap.ObjectMeta.Name, cm.ObjectMeta.Name)
			assert.Equal(t, tc.expectedConfigMap.ObjectMeta.Namespace, cm.ObjectMeta.Namespace)
			assert.Equal(t, tc.expectedConfigMap.Data, cm.Data)
		})
	}
}

func TestValidateJobConfig(t *testing.T) {
	imageList, _ := generateImageList()
	testCases := []struct {
		name               string
		imageList          []string
		inputConfigMapName string
		inputJobName       string
		expectedError      error
	}{
		{
			name:               "Empty image list",
			imageList:          []string{},
			inputConfigMapName: "",
			inputJobName:       "",
			expectedError:      assert.AnError,
		},
		{
			name:               "Existing precache configmap",
			imageList:          imageList,
			inputConfigMapName: LcaPrecacheConfigMapName,
			inputJobName:       "",
			expectedError:      assert.AnError,
		},
		{
			name:               "Existing precache job",
			imageList:          imageList,
			inputConfigMapName: "",
			inputJobName:       LcaPrecacheJobName,
			expectedError:      assert.AnError,
		},
		{
			name:               "Success case",
			imageList:          imageList,
			inputConfigMapName: "",
			inputJobName:       "",
			expectedError:      nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}

			// Inject ConfigMap, Job
			if tc.inputConfigMapName != "" {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: LcaPrecacheConfigMapName, Namespace: common.LcaNamespace}}
				objs = append(objs, cm)
			}
			if tc.inputJobName != "" {
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: LcaPrecacheJobName, Namespace: common.LcaNamespace}}
				objs = append(objs, job)
			}

			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			err = validateJobConfig(context.TODO(), fakeClient, tc.imageList)
			if tc.expectedError != nil {
				assert.NotNil(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func SortEnvVars(envVars []corev1.EnvVar) []corev1.EnvVar {
	// Define a sorting function
	sort.Slice(envVars, func(i, j int) bool {
		return envVars[i].Name < envVars[j].Name
	})

	return envVars
}

func getExpectedBaseJob() *batchv1.Job {
	var (
		backOffLimit    = BackoffLimit
		defaultMode     = DefaultMode
		privileged      = Privileged
		runAsUser       = RunAsUser
		hostDirPathType = HostDirPathType
	)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LcaPrecacheJobName,
			Namespace: common.LcaNamespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backOffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "workload",
							Image:           precacheWorkloadImage,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"sh", "-c", "--"},
							Env: []corev1.EnvVar{
								{
									Name:  EnvPrecacheSpecFile,
									Value: filepath.Join(PrecachingSpecFilepath, PrecachingSpecFilename),
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								RunAsUser:  &runAsUser,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "host",
									MountPath: utils.Host,
								},
								{
									Name:      "image-list-cm",
									MountPath: PrecachingSpecFilepath,
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(RequestResourceCPU),
									corev1.ResourceMemory: resource.MustParse(RequestResourceMemory),
								},
							},
						},
					},
					ServiceAccountName: LcaPrecacheServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						{
							// Mount root fs
							Name: "host",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/",
									Type: &hostDirPathType,
								}}},
						{
							// Mount precaching image list configmap
							Name: "image-list-cm",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: LcaPrecacheConfigMapName,
									},
									DefaultMode: &defaultMode,
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestRenderJob(t *testing.T) {
	testCases := []struct {
		name            string
		config          *Config
		expectedError   error
		expectedArgs    []string
		expectedEnvVars []corev1.EnvVar
	}{
		{
			name:          "Fully specified, valid precaching config",
			config:        NewConfig([]string{}, "NumConcurrentPulls", 1, "NicePriority", 1, "IoNiceClass", IoNiceClassRealTime, "IoNicePriority", 5),
			expectedError: nil,
			expectedArgs:  []string{fmt.Sprintf("nice -n 1 ionice -c %d -n 5 precache", IoNiceClassRealTime)},
			expectedEnvVars: []corev1.EnvVar{
				{
					Name:  EnvMaxPullThreads,
					Value: "1",
				},
			},
		},
		{
			name:          "Partially specified, with some invalid precaching config",
			config:        NewConfig([]string{}, "NumConcurrentPulls", 10, "NicePriority", 100, "IoNiceClass", IoNiceClassRealTime),
			expectedError: nil,
			expectedArgs: []string{fmt.Sprintf("nice -n %d ionice -c %d -n %d precache",
				DefaultNicePriority, IoNiceClassRealTime, DefaultIoNicePriority)},
			expectedEnvVars: []corev1.EnvVar{
				{
					Name:  EnvMaxPullThreads,
					Value: "10",
				},
			},
		},
		{
			name:          "Only image list provided in precaching config",
			config:        NewConfig([]string{}),
			expectedError: nil,
			expectedArgs: []string{fmt.Sprintf("nice -n %d ionice -c %d -n %d precache",
				DefaultNicePriority, DefaultIoNiceClass, DefaultIoNicePriority)},
			expectedEnvVars: []corev1.EnvVar{
				{
					Name:  EnvMaxPullThreads,
					Value: strconv.Itoa(DefaultMaxConcurrentPulls),
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			renderedJob, err := renderJob(tc.config, ctrl.Log.WithName("Precache"))
			if tc.expectedError != nil {
				assert.NotNil(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, renderedJob)

				expectedJob := getExpectedBaseJob()
				expectedJob.Spec.Template.Spec.Containers[0].Args = tc.expectedArgs
				for _, env := range tc.expectedEnvVars {
					expectedJob.Spec.Template.Spec.Containers[0].Env = append(expectedJob.Spec.Template.Spec.Containers[0].Env, env)
				}

				// Sort the env-vars of both expected and rendered job specs for comparisons
				expectedJob.Spec.Template.Spec.Containers[0].Env = SortEnvVars(expectedJob.Spec.Template.Spec.Containers[0].Env)
				renderedJob.Spec.Template.Spec.Containers[0].Env = SortEnvVars(renderedJob.Spec.Template.Spec.Containers[0].Env)

				// Compare the two Job specs for equivalence
				assert.True(t, reflect.DeepEqual(expectedJob.Spec, renderedJob.Spec), "Job specs are not equivalent")
				// Compare the two Job specs for equivalence
				if !reflect.DeepEqual(expectedJob.Spec, renderedJob.Spec) {
					diff := diff.ObjectDiff(expectedJob.Spec, renderedJob.Spec)
					t.Errorf("Job specs are not equivalent. Difference:\n%s", diff)
				}
			}
		})
	}
}
