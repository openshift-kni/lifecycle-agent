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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func getJob(ctx context.Context, c client.Client) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      LcaPrecacheJobName,
		Namespace: common.LcaNamespace,
	}, job); err != nil {
		return nil, err //nolint:wrapcheck
	}

	return job, nil
}

func renderConfigMap(imageList []string) *corev1.ConfigMap {
	data := make(map[string]string)
	data[PrecachingSpecFilename] = strings.Join(imageList, "\n") + "\n"

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LcaPrecacheConfigMapName,
			Namespace: common.LcaNamespace,
		},
		Data: data,
	}

	return configMap
}

func validateJobConfig(ctx context.Context, c client.Client, imageList []string) error {
	job, err := getJob(ctx, c)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	}
	if job != nil {
		return errors.New("precaching job already exists, cannot create new job")
	}

	cm, err := common.GetConfigMap(ctx, c, v1alpha1.ConfigMapRef{
		Name:      LcaPrecacheConfigMapName,
		Namespace: common.LcaNamespace,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to get precache configMap: %w", err)
	}
	if cm != nil {
		return errors.New("precaching configmap already exists, cannot create new job")
	}

	if len(imageList) < 1 {
		return errors.New("no images specified for precaching")
	}

	return nil
}

func renderJob(config *Config, log logr.Logger, ibu *v1alpha1.ImageBasedUpgrade, scheme *runtime.Scheme) (*batchv1.Job, error) {

	var ValidIoNiceClasses = []int{IoNiceClassNone, IoNiceClassRealTime, IoNiceClassBestEffort, IoNiceClassIdle}

	workloadImg := os.Getenv(EnvLcaPrecacheImage)
	if workloadImg == "" {
		return nil, fmt.Errorf("missing %s environment variable", EnvLcaPrecacheImage)
	}

	var (
		backOffLimit    = BackoffLimit
		defaultMode     = DefaultMode
		privileged      = Privileged
		runAsUser       = RunAsUser
		hostDirPathType = HostDirPathType
	)

	// Process precaching config parameters, use default values if unspecified
	// Process number of concurrent pulls
	numConcurrentPulls := config.NumConcurrentPulls
	if numConcurrentPulls < 1 {
		log.Info("Precaching invalid configuration [numConcurrentPulls], using default", "spec", numConcurrentPulls,
			"default", DefaultMaxConcurrentPulls)
		numConcurrentPulls = DefaultMaxConcurrentPulls
	}

	// Process nice priority
	nicePriority := config.NicePriority
	if nicePriority < MinNicePriority || nicePriority > MaxNicePriority {
		log.Info("Precaching invalid configuration [nicePriority], using default", "spec", nicePriority,
			"default", DefaultNicePriority)
		nicePriority = DefaultNicePriority
	}

	// Process ionice class
	ioNiceClass := config.IoNiceClass
	isValid := false
	for _, validClass := range ValidIoNiceClasses {
		if ioNiceClass == validClass {
			isValid = true
			break
		}
	}
	if !isValid {
		log.Info("Precaching invalid configuration [ioNiceClass], using default", "spec", ioNiceClass,
			"default", DefaultIoNiceClass)
		ioNiceClass = DefaultIoNiceClass
	}

	// Process ionice priority
	ioNicePriority := config.IoNicePriority
	if ioNicePriority < MinIoNicePriority || ioNicePriority > MaxIoNicePriority {
		log.Info("Precaching invalid configuration [ioNicePriority], using default", "spec", ioNicePriority,
			"default", DefaultIoNicePriority)
		ioNicePriority = DefaultIoNicePriority
	}

	execPrecacheArgs := fmt.Sprintf("nice -n %d ionice -c %d -n %d precache",
		nicePriority, ioNiceClass, ioNicePriority)

	precacheEnvVars := append(config.EnvVars, []corev1.EnvVar{
		{
			Name:  EnvPrecacheSpecFile,
			Value: filepath.Join(PrecachingSpecFilepath, PrecachingSpecFilename),
		},
		{
			Name:  EnvMaxPullThreads,
			Value: strconv.Itoa(numConcurrentPulls),
		},
	}...)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LcaPrecacheJobName,
			Namespace: common.LcaNamespace,
			Annotations: map[string]string{
				"app.kubernetes.io/name": "lifecycle-agent-precache",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backOffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						common.WorkloadManagementAnnotationKey: common.WorkloadManagementAnnotationValue,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "workload",
							Image:           workloadImg,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"sh", "-c", "--"},
							Args:            []string{execPrecacheArgs},
							Env:             precacheEnvVars,
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
								RunAsUser:  &runAsUser,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "host",
									MountPath: common.Host,
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

	// set reference
	if err := ctrl.SetControllerReference(ibu, job, scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// set finalizer
	controllerutil.AddFinalizer(job, LcaPrecacheFinalizer)

	return job, nil
}

func generateDeleteOptions() *client.DeleteOptions {
	propagationPolicy := metav1.DeletePropagationBackground

	delOpt := client.DeleteOptions{
		PropagationPolicy: &propagationPolicy,
	}
	return &delOpt
}

// deleteConfigMap delete the precache configMap
func deleteConfigMap(ctx context.Context, c client.Client) error {
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LcaPrecacheConfigMapName,
			Namespace: common.LcaNamespace,
		},
	}

	if err := c.Delete(ctx, &cm, generateDeleteOptions()); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete configMaps: %w", err)
		}
	}

	return nil
}

// deleteJob delete the precache job
func deleteJob(ctx context.Context, c client.Client) error {
	if err := removePrecacheFinalizer(ctx, c); err != nil {
		return fmt.Errorf("failed to remove finalizer during cleanup: %w", err)
	}

	precache := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      LcaPrecacheJobName,
			Namespace: common.LcaNamespace,
		},
	}
	if err := c.Delete(ctx, &precache, generateDeleteOptions()); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to job: %w", err)
		}
	}

	return nil
}

// removePrecacheFinalizer remove the finalizer if present
func removePrecacheFinalizer(ctx context.Context, c client.Client) error {
	precache, err := getJob(ctx, c)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed get precache job to remove finalizer: %w", err)
	}

	if controllerutil.ContainsFinalizer(precache, LcaPrecacheFinalizer) {
		finalizersUpdated := controllerutil.RemoveFinalizer(precache, LcaPrecacheFinalizer)
		if finalizersUpdated {
			if err := c.Update(ctx, precache); err != nil {
				return fmt.Errorf("failed to remove finalizer during update: %w", err)
			}
		}
	}

	return nil
}
