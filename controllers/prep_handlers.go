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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	commonUtils "github.com/openshift-kni/lifecycle-agent/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) launchGetSeedImage(
	ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile, progressfile string,
) (ctrl.Result, error) {
	result := requeueImmediately()
	if err := updateProgressFile(progressfile, "started-seed-image-pull"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return result, err
	}
	if err := r.getSeedImage(ctx, ibu, imageListFile); err != nil {
		r.Log.Error(err, "failed to get seed image")
		if err := updateProgressFile(progressfile, "Failed"); err != nil {
			r.Log.Error(err, "failed to update progress file")
			return result, err
		}
		return result, err
	}
	if err := updateProgressFile(progressfile, "completed-seed-image-pull"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return result, err
	}
	return result, nil
}

func (r *ImageBasedUpgradeReconciler) getSeedImage(
	ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile string,
) error {
	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err := utils.LoadSecretData(
			ctx, r.Client, ibu.Spec.SeedImageRef.PullSecretRef.Name, common.LcaNamespace, corev1.DockerConfigJsonKey,
		)
		if err != nil {
			err = fmt.Errorf("failed to retrieve pull-secret from secret %s, err: %w", ibu.Spec.SeedImageRef.PullSecretRef.Name, err)
			return err
		}

		pullSecretFilename = filepath.Join(utils.IBUWorkspacePath, "seed-pull-secret")
		if err = os.WriteFile(common.PathOutsideChroot(pullSecretFilename), []byte(pullSecret), 0o600); err != nil {
			err = fmt.Errorf("failed to write seed image pull-secret to file %s, err: %w", pullSecretFilename, err)
			return err
		}
		defer os.Remove(common.PathOutsideChroot(pullSecretFilename))
	}

	r.Log.Info("Pulling seed image")
	if _, err := r.Executor.Execute("podman", "pull", "--authfile", pullSecretFilename, ibu.Spec.SeedImageRef.Image); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	mountpoint, err := r.Executor.Execute("podman", "image", "mount", ibu.Spec.SeedImageRef.Image)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}

	if err := common.CopyOutsideChroot(filepath.Join(mountpoint, "containers.list"), imageListFile); err != nil {
		return fmt.Errorf("failed to copy image list file: %w", err)
	}
	return nil

}

func readPrecachingList(imageListFile, clusterRegistry, seedRegistry string) (imageList []string, err error) {
	var content []byte
	content, err = os.ReadFile(common.PathOutsideChroot(imageListFile))
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")

	// Filter out empty lines
	for _, line := range lines {
		if line == "" {
			continue
		}
		image, err := commonUtils.ReplaceImageRegistry(line, clusterRegistry, seedRegistry)
		if err != nil {
			return nil, err
		}
		imageList = append(imageList, image)
	}

	return imageList, nil
}

func updateProgressFile(path, progress string) error {
	return os.WriteFile(common.PathOutsideChroot(path), []byte(progress), 0o700)
}

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile, progressfile string, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	clusterRegistry, err := clusterinfo.NewClusterInfoClient(r.Client).GetReleaseRegistry(ctx)
	if err != nil {
		r.Log.Error(err, "Failed to get cluster registry")
		return
	}

	seedInfo, err := clusterinfo.ReadClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(getDesiredStaterootName(ibu))))
	if err != nil {
		r.Log.Error(err, "Failed to read seed info")
		return
	}

	imageList, err := readPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList)
	err = r.Precache.CreateJob(ctx, config)
	if err != nil {
		r.Log.Error(err, "Failed to create precaching job")
		if err = updateProgressFile(progressfile, "Failed"); err != nil {
			r.Log.Error(err, "Failed to update progress file for precaching")
			return
		}
		return
	}
	if err = updateProgressFile(progressfile, "precaching-in-progress"); err != nil {
		r.Log.Error(err, "Failed to update progress file for precaching")
		return
	}
	r.Log.Info("Precaching progress status updated to in-progress")

	result = requeueWithShortInterval()
	return
}

func (r *ImageBasedUpgradeReconciler) queryPrecachingStatus(ctx context.Context, progressfile string) (result ctrl.Result, err error) {
	// fetch precaching status
	status, err := r.Precache.QueryJobStatus(ctx)
	if err != nil {
		r.Log.Error(err, "Failed to get precaching job status")
		return
	}

	result = requeueImmediately()

	var progressContent []byte
	var logMsg string
	switch {
	case status.Status == "Active":
		logMsg = "Precaching in-progress"
		result = requeueWithMediumInterval()
	case status.Status == "Succeeded":
		progressContent = []byte("completed-precache")
		logMsg = "Precaching completed"
	case status.Status == "Failed":
		progressContent = []byte("Failed")
		logMsg = "Precaching failed"
	}

	// Augment precaching summary data if available
	if status.Message != "" {
		logMsg = fmt.Sprintf("%s: %s", logMsg, status.Message)
	}
	r.Log.Info(logMsg)

	// update progress-file
	if len(progressContent) > 0 {
		if err = os.WriteFile(common.PathOutsideChroot(progressfile), progressContent, 0o700); err != nil {
			r.Log.Error(err, "Failed to update progress file for precaching")
			return
		}
	}

	return
}

func (r *ImageBasedUpgradeReconciler) SetupStateroot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	r.Log.Info("Start setupstateroot")

	defer r.Ops.UnmountAndRemoveImage(ibu.Spec.SeedImageRef.Image)

	workspaceOutsideChroot, err := os.MkdirTemp(common.PathOutsideChroot("/var/tmp"), "")
	if err != nil {
		return fmt.Errorf("failed to create temp directory %w", err)
	}

	defer func() {
		if err := os.RemoveAll(workspaceOutsideChroot); err != nil {
			r.Log.Error(err, "failed to cleanup workspace")
		}
	}()

	workspace, err := filepath.Rel(common.Host, workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("failed to get workspace relative path %w", err)
	}
	r.Log.Info("workspace:" + workspace)

	if err = r.Ops.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount /sysroot: %w", err)
	}

	mountpoint, err := r.Executor.Execute("podman", "image", "mount", ibu.Spec.SeedImageRef.Image)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}

	ostreeRepo := filepath.Join(workspace, "ostree")
	if err = os.Mkdir(common.PathOutsideChroot(ostreeRepo), 0o755); err != nil {
		return fmt.Errorf("failed to create ostree repo directory: %w", err)
	}

	if err := r.Ops.ExtractTarWithSELinux(
		fmt.Sprintf("%s/ostree.tgz", mountpoint), ostreeRepo,
	); err != nil {
		return fmt.Errorf("failed to extract ostree.tgz: %w", err)
	}

	// example:
	// seedBootedID: rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedDeployment: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedRef: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c
	seedBootedID, err := prep.GetBootedStaterootIDFromRPMOstreeJson(filepath.Join(common.PathOutsideChroot(mountpoint), "rpm-ostree.json"))
	if err != nil {
		return fmt.Errorf("failed to get booted stateroot id: %w", err)
	}
	seedBootedDeployment, err := prep.GetDeploymentFromDeploymentID(seedBootedID)
	if err != nil {
		return err
	}
	seedBootedRef := strings.Split(seedBootedDeployment, ".")[0]

	version, err := prep.GetVersionFromClusterInfoFile(filepath.Join(common.PathOutsideChroot(mountpoint), common.ClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get version from ClusterInfo: %w", err)
	}

	if version != ibu.Spec.SeedImageRef.Version {
		return fmt.Errorf("version specified in seed image (%s) differs from version in spec (%s)",
			version, ibu.Spec.SeedImageRef.Version)
	}

	osname := getDesiredStaterootName(ibu)

	if err = r.OstreeClient.PullLocal(ostreeRepo); err != nil {
		return fmt.Errorf("failed ostree pull-local: %w", err)
	}

	if err = r.OstreeClient.OSInit(osname); err != nil {
		return fmt.Errorf("failed ostree admin os-init: %w", err)
	}

	kargs, err := prep.BuildKernelArgumentsFromMCOFile(filepath.Join(common.PathOutsideChroot(mountpoint), "mco-currentconfig.json"))
	if err != nil {
		return fmt.Errorf("failed to build kargs: %w", err)
	}

	if err = r.OstreeClient.Deploy(osname, seedBootedRef, kargs); err != nil {
		return fmt.Errorf("failed ostree admin deploy: %w", err)
	}

	deploymentID, err := r.RPMOstreeClient.GetDeploymentID(osname)
	if err != nil {
		return fmt.Errorf("failed to get deploymentID: %w", err)
	}
	deployment, err := prep.GetDeploymentFromDeploymentID(deploymentID)
	if err != nil {
		return err
	}

	if err = common.CopyOutsideChroot(
		filepath.Join(mountpoint, fmt.Sprintf("ostree-%s.origin", seedBootedDeployment)),
		prep.GetDeploymentOriginPath(osname, deployment),
	); err != nil {
		return fmt.Errorf("failed to restore origin file: %w", err)
	}

	if err = r.Ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "var.tgz"),
		common.GetStaterootPath(osname),
	); err != nil {
		return fmt.Errorf("failed to restore var directory: %w", err)
	}

	if err := r.Ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "etc.tgz"),
		prep.GetDeploymentDirPath(osname, deployment),
	); err != nil {
		return fmt.Errorf("failed to extract seed etc: %w", err)
	}

	if err = prep.RemoveETCDeletions(mountpoint, osname, deployment); err != nil {
		return fmt.Errorf("failed to process etc.deletions: %w", err)
	}

	prep.BackupCertificates(ctx, osname, r.ManifestClient)

	if err = common.CopyOutsideChroot(
		filepath.Join(mountpoint, common.ClusterInfoFileName),
		getSeedManifestPath(osname),
	); err != nil {
		return fmt.Errorf("failed to copy ClusterInfo file: %w", err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) launchSetupStateroot(ctx context.Context,
	ibu *lcav1alpha1.ImageBasedUpgrade, progressfile string) (ctrl.Result, error) {
	result := requeueImmediately()
	if err := updateProgressFile(progressfile, "started-stateroot"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return result, err
	}
	if err := r.SetupStateroot(ctx, ibu); err != nil {
		r.Log.Error(err, "failed to setup stateroot")
		if err := updateProgressFile(progressfile, "Failed"); err != nil {
			r.Log.Error(err, "failed to update progress file")
			return result, err
		}
		return result, err
	}
	if err := updateProgressFile(progressfile, "completed-stateroot"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return result, err
	}
	return result, nil
}

func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	result = doNotRequeue()

	_, err = os.Stat(common.Host)
	if err != nil {
		// fail without /host
		return
	}

	if _, err = os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); os.IsNotExist(err) {
		err = os.Mkdir(common.PathOutsideChroot(utils.IBUWorkspacePath), 0o700)
	}

	if err != nil {
		return
	}

	progressfile := filepath.Join(utils.IBUWorkspacePath, "prep-progress")
	imageListFile := filepath.Join(utils.IBUWorkspacePath, "image-list-file")

	_, err = os.Stat(common.PathOutsideChroot(progressfile))

	if err == nil {
		// in progress
		var content []byte
		content, err = os.ReadFile(common.PathOutsideChroot(progressfile))
		if err != nil {
			return
		}

		progress := strings.TrimSpace(string(content))
		r.Log.Info("Prep progress: " + progress)

		if progress == "Failed" {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep failed",
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep failed",
				ibu.Generation)
		} else if progress == "completed-seed-image-pull" {
			result, err = r.launchSetupStateroot(ctx, ibu, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to setupStateroot")
				return
			}
		} else if progress == "completed-stateroot" {
			result, err = r.launchPrecaching(ctx, imageListFile, progressfile, ibu)
			if err != nil {
				r.Log.Error(err, "Failed to launch get-seed-image phase")
				return
			}
		} else if progress == "precaching-in-progress" {
			result, err = r.queryPrecachingStatus(ctx, progressfile)
			if err != nil {
				r.Log.Error(err, "Failed to query get-seed-image phase")
				return
			}
		} else if progress == "completed-precache" {
			// Fetch final precaching job report summary
			conditionMessage := "Prep completed"
			status, err := r.Precache.QueryJobStatus(ctx)
			if err == nil && status != nil && status.Message != "" {
				conditionMessage += fmt.Sprintf(": %s", status.Message)
			}

			// If completed, update conditions and return doNotRequeue
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetCompletedConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionTrue,
				conditionMessage,
				ibu.Generation)
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(lcav1alpha1.Stages.Prep),
				utils.ConditionReasons.Completed,
				metav1.ConditionFalse,
				"Prep completed",
				ibu.Generation)
		} else {
			result = requeueWithShortInterval()
		}
	} else if os.IsNotExist(err) {
		result, err = r.launchGetSeedImage(ctx, ibu, imageListFile, progressfile)
		if err != nil {
			r.Log.Error(err, "Failed to launch get-seed-image phase")
			return
		}
	}

	return
}

func getSeedManifestPath(osname string) string {
	return filepath.Join(
		common.GetStaterootPath(osname),
		filepath.Join("/var/", common.OptOpenshift, common.SeedManifest),
	)
}
