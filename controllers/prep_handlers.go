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

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	kbatch "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/coreos/go-semver/semver"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func GetSeedImage(c client.Client, ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, log logr.Logger, ops ops.Execute) error {
	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err := lcautils.GetSecretData(ctx, ibu.Spec.SeedImageRef.PullSecretRef.Name,
			common.LcaNamespace, corev1.DockerConfigJsonKey, c)
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

	if _, err := ops.Execute("podman", "pull", "--authfile", pullSecretFilename, ibu.Spec.SeedImageRef.Image); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	log.Info("Successfully pulled seed image", "image", ibu.Spec.SeedImageRef.Image)

	labels, err := getLabelsForSeedImage(ibu.Spec.SeedImageRef.Image, ops)
	if err != nil {
		return fmt.Errorf("failed to get seed image labels: %w", err)
	}

	log.Info("Checking seed image version compatibility")
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

	clusterHasProxy, err := lcautils.HasProxy(ctx, c)
	if err != nil {
		return fmt.Errorf("failed to check if cluster has proxy: %w", err)
	}

	log.Info("Checking seed image proxy compatibility")
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

func getLabelsForSeedImage(seedImageRef string, ops ops.Execute) (map[string]string, error) {
	inspectArgs := []string{
		"inspect",
		"--format", "json",
		seedImageRef,
	}

	var inspect []struct {
		Labels map[string]string `json:"Labels"`
	}

	// TODO: use the context when execute supports it
	if inspectRaw, err := ops.Execute("podman", inspectArgs...); err != nil || inspectRaw == "" {
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

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile string, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	clusterRegistry, err := lcautils.GetReleaseRegistry(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get cluster registry: %w", err)
	}
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(common.GetDesiredStaterootName(ibu))))
	if err != nil {
		return fmt.Errorf("failed to read seed info: %w", err)
	}
	// TODO: if seedInfo.hasProxy we also require that the LCA deployment contain "NO_PROXY" + "HTTP_PROXY" + "HTTPS_PROXY" as env vars. Produce a warning and/or document this.
	r.Log.Info("Collected seed info for precache", "seed info", fmt.Sprintf("%+v", seedInfo))

	shouldOverrideRegistry, err := lcautils.ShouldOverrideSeedRegistry(ctx, r.Client, seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry)
	if err != nil {
		return fmt.Errorf("failed to check ShouldOverrideSeedRegistry %w", err)
	}
	r.Log.Info("Override registry status", "shouldOverrideRegistry", shouldOverrideRegistry)

	imageList, err := prep.ReadPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry, shouldOverrideRegistry)
	if err != nil {
		return fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
	}

	envVars, err := r.getPodEnvVars(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pod env vars: %w", err)
	}

	// Create pre-cache config using default values
	config := precache.NewConfig(imageList, envVars)
	err = r.Precache.CreateJobAndConfigMap(ctx, config, ibu)
	if err != nil {
		return fmt.Errorf("failed to create precaching job: %w", err)
	}

	return nil
}

// validateIBUSpec validates the fields in the IBU spec
func (r *ImageBasedUpgradeReconciler) validateIBUSpec(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
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
		if err := r.ExtraManifest.ValidateExtraManifestConfigmaps(ctx, ibu.Spec.ExtraManifests, ibu); err != nil {
			return fmt.Errorf("failed to validate extramanifest cms: %w", err)
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

func initIBUWorkspaceDir() error {
	if _, err := os.Stat(common.Host); err != nil {
		// fail without /host
		return fmt.Errorf("host dir does not exist: %w", err)
	}

	if _, err := os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); os.IsNotExist(err) {
		if err := os.Mkdir(common.PathOutsideChroot(utils.IBUWorkspacePath), 0o700); err != nil {
			return fmt.Errorf("failed to create IBU workspace: %w", err)
		}
	}

	return nil
}

// handlePrep the main func to run prep stage
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Fetching stateroot setup job")
	staterootSetupJob, err := prep.GetStaterootSetupJob(ctx, r.Client, r.Log)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("Validating Ibu spec")
			if err := r.validateIBUSpec(ctx, ibu); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to validate Ibu spec: %s", err.Error()), ibu)
			}

			r.Log.Info("Creating IBU workspace")
			if err := initIBUWorkspaceDir(); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to initialize IBU workspace: %s", err.Error()), ibu)
			}

			r.Log.Info("Running health check for Prep")
			if err := CheckHealth(ctx, r.NoncachedClient, r.Log); err != nil {
				msg := fmt.Sprintf("Waiting for system to stabilize before Prep stage can continue: %s", err.Error())
				r.Log.Info(msg)
				utils.SetPrepStatusInProgress(ibu, msg)
				return requeueWithHealthCheckInterval(), nil
			}

			r.Log.Info("Launching a new stateroot job")
			if _, err := prep.LaunchStaterootSetupJob(ctx, r.Client, ibu, r.Scheme, r.Log); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed launch stateroot job: %s", err.Error()), ibu)
			}
			return prepInProgressRequeue(r.Log, fmt.Sprintf("Successfully launched a new job `%s` in namespace `%s`", prep.JobName, common.LcaNamespace), ibu)
		}
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to get stateroot setup job: %s", err.Error()), ibu)
	}

	r.Log.Info("Verifying stateroot setup job status")

	// job deletion not allowed
	if staterootSetupJob.GetDeletionTimestamp() != nil {
		return prepFailDoNotRequeue(r.Log, "stateroot job is marked to be deleted, this is not allowed", ibu)
	}

	// check .status
	_, staterootSetupFinishedType := common.IsJobFinished(staterootSetupJob)
	switch staterootSetupFinishedType {
	case "":
		common.LogPodLogs(staterootSetupJob, r.Log, r.Clientset)
		return prepInProgressRequeue(r.Log, "Stateroot setup in progress", ibu)
	case kbatch.JobFailed:
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("stateroot setup job could not complete. Please check job logs for more, job_name: %s, job_ns: %s", staterootSetupJob.GetName(), staterootSetupJob.GetNamespace()), ibu)
	case kbatch.JobComplete:
		r.Log.Info("Stateroot job completed successfully", "completion time", staterootSetupJob.Status.CompletionTime, "total time", staterootSetupJob.Status.CompletionTime.Sub(staterootSetupJob.Status.StartTime.Time))
	}

	r.Log.Info("Fetching precache job")
	precacheJob, err := precache.GetJob(ctx, r.Client)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("Launching a new precache job")
			if err := r.launchPrecaching(ctx, precache.ImageListFile, ibu); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to launch precaching job: %s", err.Error()), ibu)
			}
			return prepInProgressRequeue(r.Log, fmt.Sprintf("Successfully launched a new job `%s` in namespace `%s`", precache.LcaPrecacheJobName, common.LcaNamespace), ibu)
		}
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to get precache job: %s", err.Error()), ibu)
	}

	r.Log.Info("Verifying precache job status")

	// job deletion not allowed
	if precacheJob.GetDeletionTimestamp() != nil {
		return prepFailDoNotRequeue(r.Log, "precache job is marked to be deleted, this not allowed", ibu)
	}

	// check .status
	_, precacheFinishedType := common.IsJobFinished(precacheJob)
	switch precacheFinishedType {
	case "":
		common.LogPodLogs(precacheJob, r.Log, r.Clientset) // pod logs
		return prepInProgressRequeue(r.Log, fmt.Sprintf("Precache job in progress: %s", precache.GetPrecacheStatusFileContent()), ibu)
	case kbatch.JobFailed:
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("precache job could not complete. Please check job logs for more, job_name: %s, job_ns: %s", precacheJob.GetName(), precacheJob.GetNamespace()), ibu)
	case kbatch.JobComplete:
		r.Log.Info("Precache job completed successfully", "completion time", precacheJob.Status.CompletionTime, "total time", precacheJob.Status.CompletionTime.Sub(precacheJob.Status.StartTime.Time))
	}

	r.Log.Info("All jobs completed successfully")
	return prepSuccessDoNotRequeue(r.Log, ibu)
}

// prepInProgressRequeue helper function to stop everything when fail detected
func prepFailDoNotRequeue(log logr.Logger, msg string, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	log.Error(fmt.Errorf("prep stage failed"), msg)
	utils.SetPrepStatusFailed(ibu, msg)
	return doNotRequeue(), nil
}

// prepInProgressRequeue helper function when everything is a success at the end
func prepSuccessDoNotRequeue(log logr.Logger, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	msg := "Prep stage completed successfully"
	log.Info(msg)
	utils.SetPrepStatusCompleted(ibu, msg)
	return doNotRequeue(), nil
}

// prepInProgressRequeue helper function when we are waiting prep work to complete.
// Though requeue is event based, we are still requeue-ing with an interval to watch out for any unknown/random activities in the system and also log anything important
func prepInProgressRequeue(log logr.Logger, msg string, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	log.Info("Prep stage in progress", "msg", msg)
	utils.SetPrepStatusInProgress(ibu, msg)
	return requeueWithShortInterval(), nil
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
