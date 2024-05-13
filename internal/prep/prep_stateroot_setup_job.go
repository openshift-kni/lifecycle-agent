package prep

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	v1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	JobName      = "lca-prep-stateroot-setup"
	jobFinalizer = "lca.openshift.io/stateroot-setup-finalizer"
)

func GetStaterootSetupJob(ctx context.Context, c client.Client, log logr.Logger) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: JobName, Namespace: common.LcaNamespace}, job); err != nil {
		return job, err //nolint:wrapcheck
	}

	log.Info("Got stateroot setup job", "namespace", job.Namespace, "name", job.Name)

	return job, nil
}

func LaunchStaterootSetupJob(ctx context.Context, c client.Client, ibu *v1.ImageBasedUpgrade, scheme *runtime.Scheme, log logr.Logger) (*batchv1.Job, error) {
	job, err := constructJobForStaterootSetup(ctx, c, ibu, scheme, log)
	if err != nil {
		return nil, fmt.Errorf("failed to render job: %w", err)
	}

	if err := c.Create(ctx, job); err != nil {
		return nil, err //nolint:wrapcheck
	}

	log.Info("Successfully created job", "job", job.Name)
	return job, nil
}

func constructJobForStaterootSetup(ctx context.Context, c client.Client, ibu *v1.ImageBasedUpgrade, scheme *runtime.Scheme, log logr.Logger) (*batchv1.Job, error) {
	log.Info("Getting lca deployment to configure stateroot setup job")
	lcaDeployment := appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: common.LcaNamespace, Name: "lifecycle-agent-controller-manager"}, &lcaDeployment); err != nil {
		return nil, fmt.Errorf("failed to get lifecycle-agent-controller-manager deployment: %w", err)
	}

	log.Info("Selecting 'manager' from LCA deployment", "deployment", lcaDeployment.Name)
	manager, ok := getManagerContainer(lcaDeployment)
	if !ok {
		return nil, fmt.Errorf("no 'manager' container found in deployment")
	}

	var backoffLimit int32 = 0
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName,
			Namespace: common.LcaNamespace,
			Annotations: map[string]string{
				"app.kubernetes.io/name": JobName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						common.WorkloadManagementAnnotationKey: common.WorkloadManagementAnnotationValue,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            JobName,
							Image:           manager.Image,
							ImagePullPolicy: manager.ImagePullPolicy,
							Command:         []string{"lca-cli", "ibu-stateroot-setup"},
							Env:             manager.Env,
							EnvFrom:         manager.EnvFrom,
							SecurityContext: manager.SecurityContext, // this is needed for podman
							VolumeMounts:    manager.VolumeMounts,
							Resources:       manager.Resources,
						},
					},
					HostPID:            lcaDeployment.Spec.Template.Spec.HostPID, // this is needed for rpmostree
					ServiceAccountName: lcaDeployment.Spec.Template.Spec.ServiceAccountName,
					RestartPolicy:      corev1.RestartPolicyNever,
					Volumes:            lcaDeployment.Spec.Template.Spec.Volumes,
				},
			},
		},
	}

	// set reference
	if err := ctrl.SetControllerReference(ibu, job, scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// set finalizer
	controllerutil.AddFinalizer(job, jobFinalizer)

	log.Info("Done rendering a new job", "job", job.Name)
	return job, nil
}

func getManagerContainer(lcaDeployment appsv1.Deployment) (corev1.Container, bool) {
	for _, container := range lcaDeployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			return container, true
		}
	}
	return corev1.Container{}, false
}

// DeleteStaterootSetupJob delete the stateroot setup job
func DeleteStaterootSetupJob(ctx context.Context, c client.Client, log logr.Logger) error {
	if err := removeStaterootSetupJobFinalizer(ctx, c, log); err != nil {
		return fmt.Errorf("failed to remove finalizer during cleanup: %w", err)
	}
	stateroot := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName,
			Namespace: common.LcaNamespace,
		},
	}
	if err := c.Delete(ctx, &stateroot, common.GenerateDeleteOptions()); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stateroot setup job: %w", err)
		}
	}

	return nil
}

// removePrecacheFinalizer remove the finalizer if present
func removeStaterootSetupJobFinalizer(ctx context.Context, c client.Client, log logr.Logger) error {
	staterootjob, err := GetStaterootSetupJob(ctx, c, log)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed get precache job to remove finalizer: %w", err)
	}

	if controllerutil.ContainsFinalizer(staterootjob, jobFinalizer) {
		finalizerRemoved := controllerutil.RemoveFinalizer(staterootjob, jobFinalizer)
		if finalizerRemoved {
			if err := c.Update(ctx, staterootjob); err != nil {
				return fmt.Errorf("failed to remove finalizer during update: %w", err)
			}
		}
	}

	log.Info("Removed stateroot setup finalizer", "finalizer", jobFinalizer)
	return nil
}
