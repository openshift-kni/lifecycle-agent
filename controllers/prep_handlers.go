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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
)

func (r *ImageBasedUpgradeReconciler) getSeedImage(
	ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err := lcautils.GetSecretData(ctx, ibu.Spec.SeedImageRef.PullSecretRef.Name,
			common.LcaNamespace, corev1.DockerConfigJsonKey, r.Client)
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

	r.Log.Info("Checking seed image compatibility")
	if err := r.checkSeedImageCompatibility(ctx, ibu.Spec.SeedImageRef.Image); err != nil {
		return fmt.Errorf("checking seed image compatibility: %w", err)
	}

	return nil
}

// checkSeedImageCompatibility checks if the seed image is compatible with the
// current version of the lifecycle-agent by inspecting the OCI image's labels
// and checking if the specified format version equals the hard-coded one that
// this version of the lifecycle agent expects. That format version is set by
// the lca-cli during the image build process, and is only manually bumped by
// developers when the image format changes in a way that is incompatible with
// previous versions of the lifecycle-agent.
func (r *ImageBasedUpgradeReconciler) checkSeedImageCompatibility(_ context.Context, seedImageRef string) error {
	inspectArgs := []string{
		"inspect",
		"--format", "json",
		seedImageRef,
	}

	var inspect []struct {
		Labels map[string]string `json:"Labels"`
	}

	// TODO: use the context when execute supports it
	if inspectRaw, err := r.Executor.Execute("podman", inspectArgs...); err != nil || inspectRaw == "" {
		return fmt.Errorf("failed to inspect image: %w", err)
	} else {
		if err := json.Unmarshal([]byte(inspectRaw), &inspect); err != nil {
			return fmt.Errorf("failed to unmarshal image inspect output: %w", err)
		}
	}

	if len(inspect) != 1 {
		return fmt.Errorf("expected 1 image inspect result, got %d", len(inspect))
	}

	seedFormatLabelValue, ok := inspect[0].Labels[common.SeedFormatOCILabel]
	if !ok {
		return fmt.Errorf(
			"seed image %s is missing the %s label, please build a new image using the latest version of the lca-cli",
			seedImageRef, common.SeedFormatOCILabel)
	}

	// Hard equal since we don't have backwards compatibility guarantees yet.
	// In the future we might want to have backwards compatibility code to
	// handle older seed formats and in that case we'll look at the version
	// number and do the right thing.
	if seedFormatLabelValue != fmt.Sprintf("%d", common.SeedFormatVersion) {
		return fmt.Errorf("seed image format version mismatch: expected %d, got %s",
			common.SeedFormatVersion, seedFormatLabelValue)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) getPodEnvVars(ctx context.Context) (envVars []corev1.EnvVar, err error) {
	pod := &corev1.Pod{}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: os.Getenv("MY_POD_NAME"), Namespace: common.LcaNamespace}, pod); err != nil {
		err = fmt.Errorf("failed to get pod info: %w", err)
		return
	}

	for _, container := range pod.Spec.Containers {
		if container.Name == "manager" {
			for _, envVar := range container.Env {
				if envVar.ValueFrom != nil {
					// Skipping any valueFrom env variables
					continue
				}
				envVars = append(envVars, envVar)
			}
			break
		}
	}

	return
}

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile string, ibu *lcav1alpha1.ImageBasedUpgrade) (bool, error) {
	clusterRegistry, err := lcautils.GetReleaseRegistry(ctx, r.Client)
	if err != nil {
		r.Log.Error(err, "Failed to get cluster registry")
		return false, err
	}
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(common.GetDesiredStaterootName(ibu))))
	if err != nil {
		r.Log.Error(err, "Failed to read seed info")
		return false, err
	}
	shouldOverrideRegistry, err := lcautils.ShouldOverrideSeedRegistry(ctx, r.Client, seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry)
	if err != nil {
		return false, err
	}

	imageList, err := prep.ReadPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry, shouldOverrideRegistry)
	if err != nil {
		err = fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
		return false, err
	}

	envVars, err := r.getPodEnvVars(ctx)
	if err != nil {
		err = fmt.Errorf("failed to get pod env vars: %w", err)
		return false, err
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList, envVars)
	err = r.Precache.CreateJob(ctx, config)
	if err != nil {
		r.Log.Error(err, "Failed to create precaching job")
		return false, err
	}

	return true, nil
}

func (r *ImageBasedUpgradeReconciler) queryPrecachingStatus(ctx context.Context) (status *precache.Status, err error) {
	status, err = r.Precache.QueryJobStatus(ctx)
	if err != nil {
		r.Log.Info("Failed to get precaching job status")
		return
	}

	if status == nil {
		r.Log.Info("Precaching job status is nil")
		return
	}

	if status.Status == precache.Failed {
		return status, precache.ErrFailed
	}

	var logMsg string
	switch {
	case status.Status == precache.Active:
		logMsg = "Precaching in-progress"
	case status.Status == precache.Succeeded:
		logMsg = "Precaching completed"
	}

	// Augment precaching log message data with precache summary report (if available)
	if status.Message != "" {
		logMsg = fmt.Sprintf("%s: %s", logMsg, status.Message)
	}
	r.Log.Info(logMsg)

	return
}

func (r *ImageBasedUpgradeReconciler) SetupStateroot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile string) error {
	if err := prep.SetupStateroot(r.Log, r.Ops, r.OstreeClient, r.RPMOstreeClient, ibu.Spec.SeedImageRef.Image,
		ibu.Spec.SeedImageRef.Version, imageListFile, false); err != nil {
		return err
	}

	if err := r.RPMOstreeClient.RpmOstreeCleanup(); err != nil {
		return fmt.Errorf("failed rpm-ostree cleanup -b: %w", err)
	}

	if err := r.RebootClient.WriteIBUAutoRollbackConfigFile(ibu); err != nil {
		return fmt.Errorf("failed to write auto-rollback config: %w", err)
	}

	if err := lcautils.BackupKubeconfigCrypto(ctx, r.Client, common.GetStaterootCertsDir(ibu)); err != nil {
		return fmt.Errorf("failed to backup cerificaties: %w", err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) verifyPrecachingCompleteFunc(retries int, interval time.Duration) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		r.Log.Info("Querying pre-caching job for completion...")
		for retry := 0; retry < retries; retry++ {
			status, err := r.queryPrecachingStatus(ctx)
			if err != nil && errors.Is(err, precache.ErrFailed) {
				// precaching job failed - exit immediately
				return false, err
			} else if status != nil {
				if status.Message != "" {
					r.PrepTask.Progress = fmt.Sprintf("Precaching progress: %s", status.Message)
				}
				if status.Status == precache.Succeeded {
					// precaching job succeeded
					return true, nil
				} else if status.Status == precache.Active {
					// precaching job still in-progress
					return false, nil
				}
			}
			// retry after interval
			time.Sleep(interval)
		}
		// failed more than retries times to retrieve precaching status - exit with error
		return false, fmt.Errorf("failed more than %d times to fetch precaching job status", retries)
	}
}

func (r *ImageBasedUpgradeReconciler) prepStageWorker(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (err error) {
	var (
		derivedCtx context.Context
		errGroup   errgroup.Group
	)

	// Create a new context for the worker, derived from the original context
	derivedCtx, r.PrepTask.Cancel = context.WithCancel(ctx)
	defer r.PrepTask.Cancel() // Ensure that the cancel function is called when the prepStageWorker function exits

	errGroup.Go(func() error {
		var ok bool
		imageListFile := filepath.Join(utils.IBUWorkspacePath, "image-list-file")

		// Pull seed image
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before pulling seed image")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Pulling seed image"
			if err = r.getSeedImage(derivedCtx, ibu); err != nil {
				r.Log.Error(err, "failed to pull seed image")
				return err
			}
			r.Log.Info("Successfully pulled seed image")
			r.PrepTask.Progress = "Successfully pulled seed image"
		}

		// Setup state-root
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before setting up stateroot")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Setting up stateroot"
			if err = r.SetupStateroot(derivedCtx, ibu, imageListFile); err != nil {
				r.Log.Error(err, "failed to setup stateroot")
				return err
			}
			r.Log.Info("Successfully setup stateroot")
			r.PrepTask.Progress = "Successfully setup stateroot"
		}

		// Launch precaching job
		select {
		case <-derivedCtx.Done():
			r.Log.Info("Context canceled before creating precaching job")
			return derivedCtx.Err()
		default:
			r.PrepTask.Progress = "Creating precaching job"
			ok, err = r.launchPrecaching(derivedCtx, imageListFile, ibu)
			if err != nil {
				r.Log.Info("Failed to launch pre-caching phase")
				return err
			}
			if !ok {
				return fmt.Errorf("failed to create precaching job")
			}
			r.Log.Info("Successfully created precaching job")
			r.PrepTask.Progress = "Successfully created precaching job"
		}

		// Wait for precaching job to complete
		r.PrepTask.Progress = "Waiting for precaching job to complete"
		interval := 30 * time.Second
		if err = wait.PollUntilContextCancel(derivedCtx, interval, false, r.verifyPrecachingCompleteFunc(5, interval)); err != nil {
			r.Log.Info("Failed to precache images")
			return err
		}

		// Fetch final precaching job report summary
		msg := "Prep completed successfully"
		status, err := r.Precache.QueryJobStatus(ctx)
		if err == nil && status != nil && status.Message != "" {
			r.Log.Info(msg, "summary", status.Message)
		}
		r.PrepTask.Progress = msg

		// Prep-stage completed successfully
		return nil
	})

	if err := errGroup.Wait(); err != nil {
		r.Log.Info("Encountered error while running prep-stage worker goroutine", "error", err)
		r.PrepTask.Progress = fmt.Sprintf("Prep failed with error: %v", err)
		return err
	}

	return nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {

	result = doNotRequeue()

	_, err = os.Stat(common.Host)
	if err != nil {
		// fail without /host
		err = fmt.Errorf("host dir does not exist: %w", err)
		return
	}

	if _, err = os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); os.IsNotExist(err) {
		err = os.Mkdir(common.PathOutsideChroot(utils.IBUWorkspacePath), 0o700)
	}

	if err != nil {
		return
	}

	switch {
	case !r.PrepTask.Active:
		r.PrepTask.done = make(chan struct{})
		r.PrepTask.Active = true
		r.PrepTask.Success = false
		r.PrepTask.Progress = "Prep stage initialized"
		go func() {
			err = r.prepStageWorker(ctx, ibu)
			close(r.PrepTask.done)
			if err != nil {
				r.Log.Error(err, "Prep stage failed with error")
				r.PrepTask.Success = false
			} else {
				r.Log.Info("Prep stage completed successfully!")
				r.PrepTask.Success = true
			}
		}()
		utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
		result = requeueWithShortInterval()
	case r.PrepTask.Active:
		select {
		case <-r.PrepTask.done:
			if r.PrepTask.Success {
				utils.SetPrepStatusCompleted(ibu, r.PrepTask.Progress)
			} else {
				utils.SetPrepStatusFailed(ibu, r.PrepTask.Progress)
			}
			// Reset Task values
			r.PrepTask.Reset()
			result = doNotRequeue()
		default:
			utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
			result = requeueWithShortInterval()
		}
	}

	return
}

func getSeedManifestPath(osname string) string {
	return filepath.Join(
		common.GetStaterootPath(osname),
		filepath.Join(common.SeedDataDir, common.SeedClusterInfoFileName),
	)
}
