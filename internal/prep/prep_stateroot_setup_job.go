package prep

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
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
	StaterootSetupJobName      = "lca-prep-stateroot-setup"
	staterootSetupJobFinalizer = "lca.openshift.io/stateroot-setup-finalizer"
)

// StaterootSetupTerminationGracePeriodSeconds max time wait before the stateroot job pod gets SIGKILL from k8s. Assuming the seed image is already in the system, the stateroot job should complete within this time.
var StaterootSetupTerminationGracePeriodSeconds int64 = 1800

func GetStaterootSetupJob(ctx context.Context, c client.Client, log logr.Logger) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: StaterootSetupJobName, Namespace: common.LcaNamespace}, job); err != nil {
		return job, err //nolint:wrapcheck
	}

	log.Info("Got stateroot setup job", "namespace", job.Namespace, "name", job.Name)
	return job, nil
}

func LaunchStaterootSetupJob(ctx context.Context, c client.Client, ibu *ibuv1.ImageBasedUpgrade, scheme *runtime.Scheme, log logr.Logger, useBootc bool) (*batchv1.Job, error) {
	job, err := constructJobForStaterootSetup(ctx, c, ibu, scheme, log, useBootc)
	if err != nil {
		return nil, fmt.Errorf("failed to render job: %w", err)
	}

	if err := c.Create(ctx, job); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return nil, err //nolint:wrapcheck
		}
	}

	log.Info("Successfully created job", "job", job.Name)
	return job, nil
}

func constructJobForStaterootSetup(ctx context.Context, c client.Client, ibu *ibuv1.ImageBasedUpgrade, scheme *runtime.Scheme, log logr.Logger, useBootc bool) (*batchv1.Job, error) {
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

	command := []string{"lca-cli", "ibu-stateroot-setup"}
	if useBootc {
		command = append(command, "--use-bootc")
	}

	var backoffLimit int32 = 0
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StaterootSetupJobName,
			Namespace: common.LcaNamespace,
			Annotations: map[string]string{
				"app.kubernetes.io/name": StaterootSetupJobName,
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
							Name:            StaterootSetupJobName,
							Image:           manager.Image,
							ImagePullPolicy: manager.ImagePullPolicy,
							Command:         command,
							Env:             manager.Env,
							EnvFrom:         manager.EnvFrom,
							SecurityContext: manager.SecurityContext, // this is needed for podman
							VolumeMounts:    manager.VolumeMounts,
							Resources:       manager.Resources,
						},
					},
					HostPID:                       lcaDeployment.Spec.Template.Spec.HostPID, // this is needed for rpmostree
					ServiceAccountName:            lcaDeployment.Spec.Template.Spec.ServiceAccountName,
					RestartPolicy:                 corev1.RestartPolicyNever,
					Volumes:                       lcaDeployment.Spec.Template.Spec.Volumes,
					TerminationGracePeriodSeconds: &StaterootSetupTerminationGracePeriodSeconds,
				},
			},
		},
	}

	// set reference
	if err := ctrl.SetControllerReference(ibu, job, scheme); err != nil {
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// set finalizer
	controllerutil.AddFinalizer(job, staterootSetupJobFinalizer)

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
			Name:      StaterootSetupJobName,
			Namespace: common.LcaNamespace,
		},
	}
	if err := c.Delete(ctx, &stateroot, common.GenerateDeleteOptions()); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete stateroot setup job: %w", err)
		}
	}

	log.Info(fmt.Sprintf("Waiting up to additional %s to verify that job's pod no longer exists", time.Duration(StaterootSetupTerminationGracePeriodSeconds)*time.Second), "job", stateroot.GetName())
	// todo: in most cases we expect this to just go through very quickly...but this is blocking call and can block up to StaterootSetupTerminationGracePeriodSeconds. Should look into using reconcile instead (this may affect other funcs called from Cleanup)
	if err := waitUntilStaterootSetupPodIsRemoved(ctx, c); err != nil {
		return fmt.Errorf("failed to wait until stateroot setup job pod is removed: %w", err)
	}

	log.Info("Successfully removed all stateroot setup job resources", "job", stateroot.GetName())
	return nil
}

// waitUntilStaterootSetupPodIsRemoved the delete client call is async, so we need to wait until the pod is completely removed make sure stateroot is cleaned up properly
func waitUntilStaterootSetupPodIsRemoved(ctx context.Context, c client.Client) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, time.Duration(StaterootSetupTerminationGracePeriodSeconds)*time.Second, true, func(context.Context) (bool, error) { //nolint:wrapcheck
		opts := []client.ListOption{
			client.InNamespace(common.LcaNamespace),
			client.MatchingLabels{"job-name": StaterootSetupJobName},
		}
		podList := &corev1.PodList{}
		if err := c.List(ctx, podList, opts...); err != nil {
			return false, fmt.Errorf("failed to list pods: %w", err)
		}

		return len(podList.Items) == 0, nil
	})
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

	if controllerutil.ContainsFinalizer(staterootjob, staterootSetupJobFinalizer) {
		finalizerRemoved := controllerutil.RemoveFinalizer(staterootjob, staterootSetupJobFinalizer)
		if finalizerRemoved {
			if err := c.Update(ctx, staterootjob); err != nil {
				return fmt.Errorf("failed to remove finalizer during update: %w", err)
			}
		}
	}

	log.Info("Removed stateroot setup finalizer", "finalizer", staterootSetupJobFinalizer)
	return nil
}
