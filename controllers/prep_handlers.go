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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/coreos/go-semver/semver"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	configv1 "github.com/openshift/api/config/v1"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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

	labels, err := r.getLabelsForSeedImage(ibu.Spec.SeedImageRef.Image)
	if err != nil {
		return fmt.Errorf("failed to get seed image labels: %w", err)
	}

	r.Log.Info("Checking seed image version compatibility")
	if err := checkSeedImageVersionCompatibility(labels); err != nil {
		return fmt.Errorf("checking seed image compatibility: %w", err)
	}

	seedInfo, err := getSeedConfigFromLabel(labels)
	if err != nil {
		return fmt.Errorf("failed to get seed cluster info from label: %w", err)
	}

	seedHasProxy := false
	if seedInfo != nil {
		// Older images may not have the seed cluster info label, in which case
		// we assume no proxy so that if the current cluster has proxy, it will
		// fail the compatibility check.
		seedHasProxy = seedInfo.HasProxy
	}

	clusterHasProxy, err := lcautils.HasProxy(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to check if cluster has proxy: %w", err)
	}

	r.Log.Info("Checking seed image proxy compatibility")
	if err := checkSeedImageProxyCompatibility(seedHasProxy, clusterHasProxy); err != nil {
		return fmt.Errorf("checking seed image compatibility: %w", err)
	}

	return nil
}

func getSeedConfigFromLabel(labels map[string]string) (*seedclusterinfo.SeedClusterInfo, error) {
	seedFormatLabelValue, ok := labels[common.SeedClusterInfoOCILabel]
	if !ok {
		return nil, nil
	}

	var seedInfo seedclusterinfo.SeedClusterInfo
	if err := json.Unmarshal([]byte(seedFormatLabelValue), &seedInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal seed cluster info: %w", err)
	}

	return &seedInfo, nil
}

func (r *ImageBasedUpgradeReconciler) getLabelsForSeedImage(seedImageRef string) (map[string]string, error) {
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
		return nil, fmt.Errorf("failed to inspect image: %w", err)
	} else {
		if err := json.Unmarshal([]byte(inspectRaw), &inspect); err != nil {
			return nil, fmt.Errorf("failed to unmarshal image inspect output: %w", err)
		}
	}

	if len(inspect) != 1 {
		return nil, fmt.Errorf("expected 1 image inspect result, got %d", len(inspect))
	}

	return inspect[0].Labels, nil
}

// checkSeedImageVersionCompatibility checks if the seed image is compatible with the
// current version of the lifecycle-agent by inspecting the OCI image's labels
// and checking if the specified format version equals the hard-coded one that
// this version of the lifecycle agent expects. That format version is set by
// the LCA during the image build process to the value of the code constant,
// and the code constant is only manually bumped by developers when the image
// format changes in a way that is incompatible with previous versions of the
// lifecycle-agent.
func checkSeedImageVersionCompatibility(labels map[string]string) error {
	seedFormatLabelValue, ok := labels[common.SeedFormatOCILabel]
	if !ok {
		return fmt.Errorf(
			"seed image is missing the %s label, please build a new image using the latest version of the lca-cli",
			common.SeedFormatOCILabel)
	}

	// Hard equal since we don't have backwards compatibility guarantees yet.
	// In the future we might want to have backwards compatibility code to
	// handle older seed formats and in that case we'll look at the version
	// number and do the right thing accordingly.
	if seedFormatLabelValue != fmt.Sprintf("%d", common.SeedFormatVersion) {
		return fmt.Errorf("seed image format version mismatch: expected %d, got %s",
			common.SeedFormatVersion, seedFormatLabelValue)
	}

	return nil
}

// checkSeedImageProxyCompatibility checks for proxy configuration
// compatibility of the seed image vs the current cluster. If the seed image
// has a proxy and the cluster being upgraded doesn't, we cannot proceed as
// recert does not support proxy rename under those conditions. Similarly, we
// cannot proceed if the cluster being upgraded has a proxy but the seed image
// doesn't.
func checkSeedImageProxyCompatibility(seedHasProxy, hasProxy bool) error {
	if seedHasProxy && !hasProxy {
		return fmt.Errorf("seed image has a proxy but the cluster being upgraded does not, this combination is not supported")
	}

	if !seedHasProxy && hasProxy {
		return fmt.Errorf("seed image does not have a proxy but the cluster being upgraded does, this combination is not supported")
	}

	return nil
}

// validateSeedOcpVersion rejects upgrade request if seed image version is not higher than current cluster (target) OCP version
func (r *ImageBasedUpgradeReconciler) validateSeedOcpVersion(seedOcpVersion string) error {
	// get target OCP version
	targetClusterVersion := &configv1.ClusterVersion{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "version"}, targetClusterVersion); err != nil {
		return fmt.Errorf("failed to get ClusterVersion for target: %w", err)
	}
	targetOCP := targetClusterVersion.Status.Desired.Version

	// parse versions
	targetSemVer, err := semver.NewVersion(targetOCP)
	if err != nil {
		return fmt.Errorf("failed to parse target version %s: %w", targetOCP, err)
	}
	seedSemVer, err := semver.NewVersion(seedOcpVersion)
	if err != nil {
		return fmt.Errorf("failed to parse seed version %s: %w", seedOcpVersion, err)
	}

	// compare versions
	if seedSemVer.Compare(*targetSemVer) <= 0 {
		return fmt.Errorf("seed OCP version (%s) must be higher than current OCP version (%s)", seedOcpVersion, targetOCP)
	}

	r.Log.Info("OCP versions are validated", "seed", seedOcpVersion, "target", targetOCP)
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
		return false, fmt.Errorf("failed to get cluster registry: %w", err)
	}
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(common.GetDesiredStaterootName(ibu))))
	if err != nil {
		return false, fmt.Errorf("failed to read seed info: %w", err)
	}
	shouldOverrideRegistry, err := lcautils.ShouldOverrideSeedRegistry(ctx, r.Client, seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry)
	if err != nil {
		return false, fmt.Errorf("failed to check ShouldOverrideSeedRegistry %w", err)
	}

	imageList, err := prep.ReadPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry, shouldOverrideRegistry)
	if err != nil {
		return false, fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
	}

	envVars, err := r.getPodEnvVars(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get pod env vars: %w", err)
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList, envVars)
	err = r.Precache.CreateJob(ctx, config, ibu)
	if err != nil {
		return false, fmt.Errorf("failed to create precaching job: %w", err)
	}

	return true, nil
}

func (r *ImageBasedUpgradeReconciler) queryPrecachingStatus(ctx context.Context) (*precache.Status, error) {
	status, err := r.Precache.QueryJobStatus(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck
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

	return status, nil
}

func (r *ImageBasedUpgradeReconciler) SetupStateroot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile string) error {
	if err := prep.SetupStateroot(r.Log, r.Ops, r.OstreeClient, r.RPMOstreeClient, ibu.Spec.SeedImageRef.Image,
		ibu.Spec.SeedImageRef.Version, imageListFile, false); err != nil {
		return fmt.Errorf("failed to setup stateroot: %w", err)
	}

	if err := r.RebootClient.WriteIBUAutoRollbackConfigFile(ibu); err != nil {
		return fmt.Errorf("failed to write auto-rollback config: %w", err)
	}

	if err := lcautils.BackupKubeconfigCrypto(ctx, r.Client, common.GetStaterootCertsDir(ibu)); err != nil {
		return fmt.Errorf("failed to backup cerificaties: %w", err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) verifyPrecachingComplete(ctx context.Context) (bool, error) {
	status, err := r.queryPrecachingStatus(ctx)
	if err != nil {
		return false, err //nolint:wrapcheck
	}

	if status.Message != "" {
		r.PrepTask.Progress = fmt.Sprintf("Precaching progress: %s", status.Message)
	}
	r.Log.Info("Current precache job status", "Status", status.Status)
	if status.Status == precache.Succeeded {
		// precaching job succeeded
		return true, nil
	} else if status.Status == precache.Active {
		// precaching job still in-progress
		return false, nil
	}

	return false, nil
}

// validateIBUSpec validates the fields in the IBU spec
func (r *ImageBasedUpgradeReconciler) validateIBUSpec(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	r.Log.Info("Validating IBU spec")

	// Check spec against this cluster's version and possibly exit early
	if err := r.validateSeedOcpVersion(ibu.Spec.SeedImageRef.Version); err != nil {
		return fmt.Errorf("failed to validate seed image OCP version: %w", err)
	}

	// If OADP configmap is provided, check if OADP operator is available, validate the configmap
	// and remove stale Backups from object storage if any.
	if len(ibu.Spec.OADPContent) != 0 {
		err := r.BackupRestore.CheckOadpOperatorAvailability(ctx)
		if err != nil {
			return fmt.Errorf("failed to check oadp operator availability: %w", err)
		}

		err = r.BackupRestore.ValidateOadpConfigmaps(ctx, ibu.Spec.OADPContent)
		if err != nil {
			return fmt.Errorf("failed to validate oadp configMap: %w", err)
		}
	}

	// Validate the extraManifests configmap if it's provided
	if len(ibu.Spec.ExtraManifests) != 0 {
		err := r.ExtraManifest.ValidateExtraManifestConfigmaps(ctx, ibu.Spec.ExtraManifests)
		if err != nil {
			return fmt.Errorf("failed to validate extraManifests configMap: %w", err)
		}
	}

	// Validate the manifests from policies if related annotations are specified
	var validationAnns = map[string]string{}
	if count, exists := ibu.GetAnnotations()[extramanifest.TargetOcpVersionManifestCountAnnotation]; exists {
		validationAnns[extramanifest.TargetOcpVersionManifestCountAnnotation] = count
	}
	if len(validationAnns) != 0 {
		versions, err := extramanifest.GetMatchingTargetOcpVersionLabelVersions(ibu.Spec.SeedImageRef.Version)
		if err != nil {
			return fmt.Errorf("failed to get matching versions for target-ocp-version label: %w", err)
		}

		objectLabels := map[string]string{extramanifest.TargetOcpVersionLabel: strings.Join(versions, ",")}
		if _, err := r.ExtraManifest.ValidateAndExtractManifestFromPolicies(ctx, nil, objectLabels, validationAnns); err != nil {
			return fmt.Errorf("failed to validate manifests from policies: %w", err)
		}
	}
	return nil
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

		// Validate IBU spec
		if err := r.validateIBUSpec(ctx, ibu); err != nil {
			// Do not return unknownCRD error detected from extra manifests configmaps,
			// instead, attach a warning message in prep status condition
			if extramanifest.IsEMUnknownCRDError(err) {
				r.PrepTask.AdditionalComplete = fmt.Sprintf(". Warn: The requested CRD is not installed on the cluster. "+
					"Please verify if this is as expected before proceeding to next stage: %v", err)
			} else {
				return fmt.Errorf("failed to validate IBU spec: %w", err)
			}
		}

		// Pull seed image
		select {
		case <-derivedCtx.Done():
			return fmt.Errorf("context canceled before pulling seed image: %w", derivedCtx.Err())
		default:
			r.PrepTask.Progress = "Pulling seed image"
			if err = r.getSeedImage(derivedCtx, ibu); err != nil {
				return fmt.Errorf("failed to pull seed image: %w", err)
			}
			r.Log.Info("Successfully pulled seed image")
			r.PrepTask.Progress = "Successfully pulled seed image"
		}

		// Setup state-root
		select {
		case <-derivedCtx.Done():
			return fmt.Errorf("context canceled before setting up stateroot: %w", derivedCtx.Err())
		default:
			r.PrepTask.Progress = "Setting up stateroot"
			if err = r.SetupStateroot(derivedCtx, ibu, imageListFile); err != nil {
				return fmt.Errorf("failed to setup stateroot with prep stage worker: %w", err)
			}
			r.Log.Info("Successfully setup stateroot")
			r.PrepTask.Progress = "Successfully setup stateroot"
		}

		// Launch precaching job
		select {
		case <-derivedCtx.Done():
			return fmt.Errorf("context canceled before creating precaching job: %w", derivedCtx.Err())
		default:
			r.PrepTask.Progress = "Creating precaching job"
			ok, err = r.launchPrecaching(derivedCtx, imageListFile, ibu)
			if err != nil {
				return fmt.Errorf("failed to launch pre-caching phase: %w", err)
			}
			if !ok {
				return fmt.Errorf("failed to create precaching job")
			}
			r.Log.Info("Successfully created precaching job")
			r.PrepTask.Progress = "Successfully created precaching job"
		}

		return nil
	})

	if err := errGroup.Wait(); err != nil {
		r.PrepTask.Progress = fmt.Sprintf("Prep failed with error: %v", err)
		return fmt.Errorf("encountered error while running prep-stage worker goroutine: %w", err)
	}

	return nil
}

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
		r.Log.Info("Running health check for Prep")
		if err := CheckHealth(ctx, r.NoncachedClient, r.Log); err != nil {
			msg := fmt.Sprintf("Waiting for system to stabilize before Prep stage can continue: %s", err.Error())
			r.Log.Info(msg)
			utils.SetPrepStatusInProgress(ibu, msg)
			return requeueWithHealthCheckInterval(), nil
		}

		r.initPrepTask()
		go func() {
			err = r.prepStageWorker(ctx, ibu)
			if err != nil {
				r.failPrepTask(err, "prep stage failed with error from prep worker")
			}
		}()
		utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
		result = requeueWithShortInterval()
	case r.PrepTask.Active:
		// check pre-cache and update PrepTask
		r.updatePrepTaskForPrecacheJob(ctx)

		select {
		case <-r.PrepTask.done:
			if r.PrepTask.Success {
				r.Log.Info("Prep stage completed successfully!")
				r.PrepTask.Progress += r.PrepTask.AdditionalComplete
				utils.SetPrepStatusCompleted(ibu, r.PrepTask.Progress)
			} else {
				utils.SetPrepStatusFailed(ibu, r.PrepTask.Progress)
			}
			// Reset Task values
			r.PrepTask.Reset()
			r.Log.Info("Done handlePrep")
			result = doNotRequeue()
		default:
			r.Log.Info("Prep stage in progress", "current msg", r.PrepTask.Progress)
			utils.SetPrepStatusInProgress(ibu, r.PrepTask.Progress)
			result = requeueWithShortInterval()
		}
	}

	return
}

// updatePrepTaskForPrecacheJob check up on precache job and update PrepTask as needed
func (r *ImageBasedUpgradeReconciler) updatePrepTaskForPrecacheJob(ctx context.Context) {
	r.Log.Info("Checking if precaching job is complete")
	done, err := r.verifyPrecachingComplete(ctx)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("Precache job not launched yet")
		} else {
			r.failPrepTask(err, "prep stage failed, unable to verify precache status")
		}
		return
	}

	if done {
		r.successPrepTask("Prep completed successfully")
	}
}

// initPrepTask init PrepTask variables
func (r *ImageBasedUpgradeReconciler) initPrepTask() {
	r.PrepTask.done = make(chan struct{})
	r.PrepTask.Active = true
	r.PrepTask.Success = false
	r.PrepTask.Progress = "Prep stage initialized"
	r.PrepTask.AdditionalComplete = ""
}

// successPrepTask set PrepTask to success
func (r *ImageBasedUpgradeReconciler) successPrepTask(msg string) {
	r.PrepTask.Progress = msg
	r.PrepTask.Success = true
	close(r.PrepTask.done)
}

// failPrepTask set PrepTask to fail
func (r *ImageBasedUpgradeReconciler) failPrepTask(err error, msg string) {
	r.Log.Error(err, msg)
	r.PrepTask.Progress = fmt.Sprintf("%s: %s", msg, err.Error())
	r.PrepTask.Success = false
	close(r.PrepTask.done)
}

func getSeedManifestPath(osname string) string {
	return filepath.Join(
		common.GetStaterootPath(osname),
		filepath.Join(common.SeedDataDir, common.SeedClusterInfoFileName),
	)
}
