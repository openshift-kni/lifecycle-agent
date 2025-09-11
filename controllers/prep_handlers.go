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
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	kbatch "k8s.io/api/batch/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coreos/go-semver/semver"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
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

func GetSeedImage(c client.Client, ctx context.Context, ibu *ibuv1.ImageBasedUpgrade, log logr.Logger, ops ops.Execute) error {
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

	return nil
}

// validateSeedImageConfig retrieves the labels for the seed image without downloading the image itself, then validates
// the config data from the labels
func (r *ImageBasedUpgradeReconciler) validateSeedImageConfig(ctx context.Context, labels map[string]string) error {
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

	seedHasFIPS := false
	if seedInfo != nil {
		// Older images may not have the seed cluster info label, in which case
		// we assume no FIPS so that if the current cluster has FIPS, it will
		// fail the compatibility check.
		seedHasFIPS = seedInfo.HasFIPS
	}

	clusterHasProxy, err := lcautils.HasProxy(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to check if cluster has proxy: %w", err)
	}

	r.Log.Info("Checking seed image proxy compatibility")
	if err := checkSeedImageProxyCompatibility(seedHasProxy, clusterHasProxy); err != nil {
		return fmt.Errorf("checking seed image compatibility: %w", err)
	}

	clusterHasFIPS, err := lcautils.HasFIPS(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to check if cluster has fips: %w", err)
	}

	r.Log.Info("Checking seed image FIPS compatibility")
	if err := checkSeedImageFIPSCompatibility(seedHasFIPS, clusterHasFIPS); err != nil {
		return fmt.Errorf("checking seed image compatibility: %w", err)
	}

	hasUserCaBundle, proxyConfigmapName, err := lcautils.GetClusterAdditionalTrustBundleState(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get cluster additional trust bundle state: %w", err)
	}

	//nolint:staticcheck //lint:ignore SA9003 // else branch intentionally left empty
	if seedInfo != nil && seedInfo.AdditionalTrustBundle != nil {
		if err := checkSeedImageAdditionalTrustBundleCompatibility(*seedInfo.AdditionalTrustBundle, hasUserCaBundle, proxyConfigmapName); err != nil {
			return fmt.Errorf("checking seed image additional trust bundle compatibility: %w", err)
		}
	} else {
		// For the sake of backwards compatibility, we allow older seed images
		// that don't have information about the additional trust bundle. This
		// means that upgrade will fail at the recert stage if there's a
		// mismatch between the seed and the seed reconfiguration data.
	}

	if seedInfo != nil && seedInfo.ContainerStorageMountpointTarget != "" {
		// Seed image data includes the container storage mountpoint target, so we can compare to running cluster
		r.Log.Info("Checking seed image container storage compatibility")
		if err := checkSeedImageContainerStorageCompatibility(seedInfo.ContainerStorageMountpointTarget, r.ContainerStorageMountpointTarget); err != nil {
			return fmt.Errorf("checking seed image compatibility: %w", err)
		}
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

// getLabelsForSeedImage uses skopeo inspect to retrieve the labels for the seed image without downloading the image itself
func (r *ImageBasedUpgradeReconciler) getLabelsForSeedImage(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (map[string]string, error) {
	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err := lcautils.GetSecretData(ctx, ibu.Spec.SeedImageRef.PullSecretRef.Name,
			common.LcaNamespace, corev1.DockerConfigJsonKey, r.Client)
		if err != nil {
			err = fmt.Errorf("failed to retrieve pull-secret from secret %s, err: %w", ibu.Spec.SeedImageRef.PullSecretRef.Name, err)
			return nil, err
		}

		pullSecretFilename = filepath.Join(utils.IBUWorkspacePath, "seed-pull-secret")
		if err = os.WriteFile(common.PathOutsideChroot(pullSecretFilename), []byte(pullSecret), 0o600); err != nil {
			err = fmt.Errorf("failed to write seed image pull-secret to file %s, err: %w", pullSecretFilename, err)
			return nil, err
		}
		defer os.Remove(common.PathOutsideChroot(pullSecretFilename))
	}

	inspectArgs := []string{
		"inspect",
		"--retry-times", "10",
		"--authfile", pullSecretFilename,
		"--format", "json",
		"docker://" + ibu.Spec.SeedImageRef.Image,
	}

	var inspect struct {
		Labels map[string]string `json:"Labels"`
	}

	// TODO: use the context when execute supports it
	if inspectRaw, err := r.Executor.Execute("skopeo", inspectArgs...); err != nil || inspectRaw == "" {
		return nil, fmt.Errorf("failed to inspect image: %w", err)
	} else {
		if err := json.Unmarshal([]byte(inspectRaw), &inspect); err != nil {
			return nil, fmt.Errorf("failed to unmarshal image inspect output: %w", err)
		}
	}

	return inspect.Labels, nil
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

// checkSeedImageAdditionalTrustBundleCompatibility checks for proxy
// configuration compatibility of the seed image vs the current cluster. If the
// seed image has a proxy and the cluster being upgraded doesn't, we cannot
// proceed as recert does not support proxy rename under those conditions.
// Similarly, we cannot proceed if the cluster being upgraded has a proxy but
// the seed image doesn't.
func checkSeedImageAdditionalTrustBundleCompatibility(seedAdditionalTrustBundle seedclusterinfo.AdditionalTrustBundle, hasUserCaBundle bool, proxyConfigmapName string) error {
	if seedAdditionalTrustBundle.HasUserCaBundle && !hasUserCaBundle {
		return fmt.Errorf("seed image has an %s/%s configmap but the cluster being upgraded does not, this combination is not supported",
			common.OpenshiftConfigNamespace, common.ClusterAdditionalTrustBundleName)
	}

	if !seedAdditionalTrustBundle.HasUserCaBundle && hasUserCaBundle {
		return fmt.Errorf("seed image does not have an %s/%s configmap but the cluster being upgraded does, this combination is not supported",
			common.OpenshiftConfigNamespace, common.ClusterAdditionalTrustBundleName)
	}

	if seedAdditionalTrustBundle.ProxyConfigmapName != proxyConfigmapName {
		return fmt.Errorf("seed image's Proxy trustedCA configmap name %q (oc get proxy -oyaml) mismatches cluster's name %q, this combination is not supported",
			seedAdditionalTrustBundle.ProxyConfigmapName, proxyConfigmapName)
	}

	return nil
}

// checkSeedImageContainerStorageCompatibility checks for .
func checkSeedImageContainerStorageCompatibility(seedContainerStorageMountpoint, clusterContainerStorageMountpoint string) error {
	if seedContainerStorageMountpoint != clusterContainerStorageMountpoint {
		return fmt.Errorf("container storage mountpoint target mismatch: seed=%s, cluster=%s", seedContainerStorageMountpoint, clusterContainerStorageMountpoint)
	}

	return nil
}

// checkSeedImageFIPSCompatibility checks for FIPS configuration compatibility
// of the seed image vs the current cluster. If the seed image has FIPS enabled
// and the cluster being upgraded doesn't, we cannot proceed as recert does not
// support FIPS rename under those conditions. Similarly, we cannot proceed if
// the cluster being upgraded has FIPS but the seed image doesn't.
func checkSeedImageFIPSCompatibility(seedHasFIPS, hasFIPS bool) error {
	if seedHasFIPS && !hasFIPS {
		return fmt.Errorf("seed image has FIPS enabled but the cluster being upgraded does not, this combination is not supported")
	}

	if !seedHasFIPS && hasFIPS {
		return fmt.Errorf("seed image does not have FIPS enabled but the cluster being upgraded does, this combination is not supported")
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

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile string, ibu *ibuv1.ImageBasedUpgrade) error {
	r.Log.Info("Getting release registry name")
	clusterRegistry, err := lcautils.GetReleaseRegistry(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get cluster registry: %w", err)
	}
	r.Log.Info("Got release registry name", "clusterRegistry", clusterRegistry)

	r.Log.Info("Checking seed image info")
	seedInfo, err := seedclusterinfo.ReadSeedClusterInfoFromFile(
		common.PathOutsideChroot(getSeedManifestPath(common.GetDesiredStaterootName(ibu))))
	if err != nil {
		return fmt.Errorf("failed to read seed info: %w", err)
	}
	// TODO: if seedInfo.hasProxy we also require that the LCA deployment contain "NO_PROXY" + "HTTP_PROXY" + "HTTPS_PROXY" as env vars. Produce a warning and/or document this.
	r.Log.Info("Collected seed info for precache", "seed info", fmt.Sprintf("%+v", seedInfo))

	r.Log.Info("Getting mirror registry source registries from cluster")
	mirrorRegistrySources, err := lcautils.GetMirrorRegistrySourceRegistries(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("failed to get mirror registry source registries from cluster %w", err)
	}

	r.Log.Info("Checking whether to override seed registry")
	shouldOverrideRegistry, err := lcautils.ShouldOverrideSeedRegistry(seedInfo.MirrorRegistryConfigured, seedInfo.ReleaseRegistry, mirrorRegistrySources)
	if err != nil {
		return fmt.Errorf("failed to check ShouldOverrideSeedRegistry %w", err)
	}
	r.Log.Info("Override registry status", "shouldOverrideRegistry", shouldOverrideRegistry)

	r.Log.Info("Reading precache list from seed image")
	imageList, err := prep.ReadPrecachingList(imageListFile, clusterRegistry, seedInfo.ReleaseRegistry, shouldOverrideRegistry)
	if err != nil {
		return fmt.Errorf("failed to read pre-caching image file: %s, %w", common.PathOutsideChroot(imageListFile), err)
	}
	r.Log.Info("Got pre-cache image list from seed image", "count", len(imageList))

	r.Log.Info("Getting env variables from lca manager for precache job")
	envVars, err := r.getPodEnvVars(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pod env vars: %w", err)
	}

	r.Log.Info("Creating pre-cache config and resources")
	config := precache.NewConfig(imageList, envVars)
	if err := r.Precache.CreateJobAndConfigMap(ctx, config, ibu); err != nil {
		return fmt.Errorf("failed to create precaching job: %w", err)
	}

	r.Log.Info("Successfully launched pre-caching")
	return nil
}

func getSeedManifestPath(osname string) string {
	return filepath.Join(
		common.GetStaterootPath(osname),
		filepath.Join(common.SeedDataDir, common.SeedClusterInfoFileName),
	)
}

// validateIBUSpec validates the fields in the IBU spec
func (r *ImageBasedUpgradeReconciler) validateIBUSpec(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) error {
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
		warn, err := r.ExtraManifest.ValidateExtraManifestConfigmaps(ctx, ibu.Spec.ExtraManifests)
		if err != nil {
			return fmt.Errorf("failed to validate extramanifest cms: %w", err)
		}
		if warn != "" {
			r.Log.Info(fmt.Sprintf("Adding IBU annotation '%s' with the extramanifest validation warning", extramanifest.ValidationWarningAnnotation))
			if err := extramanifest.AddAnnotationEMWarningValidation(r.Client, r.Log, ibu, warn); err != nil {
				return fmt.Errorf("failed to add extramanifest warning validation annotation: %w", err)
			}
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

// Cleanup container storage, if needed
func (r *ImageBasedUpgradeReconciler) containerStorageCleanup(ibu *ibuv1.ImageBasedUpgrade) error {
	// Check whether image cleanup is disabled using annotation
	if val, exists := ibu.GetAnnotations()[common.ImageCleanupOnPrepAnnotation]; exists {
		if val == common.ImageCleanupDisabledValue {
			r.Log.Info("Automatic image cleanup is disabled")
			return nil
		}
	}

	thresholdPercent := common.ContainerStorageUsageThresholdPercentDefault

	// Check for threshold override
	if val, exists := ibu.GetAnnotations()[common.ContainerStorageUsageThresholdPercentAnnotation]; exists {
		var err error
		if thresholdPercent, err = strconv.Atoi(val); err != nil {
			r.Log.Error(err, "Failed to parse threshold value from annotation", "value", val)
			r.Log.Info("Automatic image cleanup is disabled, due to failure to parse threshold annotation")
			return nil
		}
	}

	// Check container storage disk usage
	if exceeded, err := r.ImageMgmtClient.CheckDiskUsageAgainstThreshold(thresholdPercent); err != nil {
		return fmt.Errorf("failed to check container storage disk usage: %w", err)
	} else if !exceeded {
		// Container storage disk usage is within threshold
		return nil
	}

	r.Log.Info("Performing automatic image cleanup to reduce container storage disk usage to be within threshold")
	if err := r.ImageMgmtClient.CleanupUnusedImages(thresholdPercent); err != nil {
		return fmt.Errorf("failed during image cleanup: %w", err)
	}

	return nil
}

// Used to start and end phases in the Prep stage. Each of them must be used in exactly two places
var (
	PrepPhaseStateroot = "Stateroot"
	PrepPhasePrecache  = "Precache"
)

// handlePrep the main func to run prep stage
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	r.Log.Info("Running health check for Prep")
	if err := CheckHealth(ctx, r.NoncachedClient, r.Log.WithName("HealthCheck")); err != nil {
		msg := fmt.Sprintf("Waiting for system to stabilize before Prep stage can continue: %s", err.Error())
		r.Log.Info(msg)
		utils.SetPrepStatusInProgress(ibu, msg)
		return requeueWithHealthCheckInterval(), nil
	}
	r.Log.Info("Cluster is healthy")

	r.Log.Info("Fetching stateroot setup job")
	staterootSetupJob, err := prep.GetStaterootSetupJob(ctx, r.Client, r.Log)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Ensure there is a container storage mountpoint, required for shared storage between stateroots for IBU
			if r.ContainerStorageMountpointTarget == "" {
				return prepFailDoNotRequeue(r.Log, "container storage mountpoint target not found", ibu)
			}

			r.Log.Info("Validating IBU spec")
			if err := r.validateIBUSpec(ctx, ibu); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to validate IBU spec: %s", err.Error()), ibu)
			}

			r.Log.Info("Creating IBU workspace")
			if err := initIBUWorkspaceDir(); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to initialize IBU workspace: %s", err.Error()), ibu)
			}

			labels, err := r.getLabelsForSeedImage(ctx, ibu)
			if err != nil {
				return requeueWithError(fmt.Errorf("failed to get labels for seed image: %w", err))
			}

			// Validate config information from the seed image labels, prior to launching the job and downloading the image
			r.Log.Info("Validating seed information")
			if err := r.validateSeedImageConfig(ctx, labels); err != nil {
				return prepFailDoNotRequeue(r.Log, fmt.Sprintf("failed to validate seed image info: %s", err.Error()), ibu)
			}

			// Check the OCI labels of the seed image to detect bootc seed
			useBootc := false
			if val, exists := labels[common.SeedUseBootcOCILabel]; exists {
				if val == "true" {
					useBootc = true
				}
			}

			r.Log.Info("Checking container storage disk space")
			if err := r.containerStorageCleanup(ibu); err != nil {
				return requeueWithError(fmt.Errorf("failed container storage cleanup: %w", err))
			}

			r.Log.Info("Launching a new stateroot job")
			if _, err := prep.LaunchStaterootSetupJob(ctx, r.Client, ibu, r.Scheme, r.Log, useBootc); err != nil {
				return requeueWithError(fmt.Errorf("failed launch stateroot job: %w", err))
			}
			// start prep stage stateroot phase timing
			utils.StartPhase(r.Client, r.Log, ibu, PrepPhaseStateroot)
			return prepInProgressRequeue(r.Log, fmt.Sprintf("Successfully launched a new job for stateroot setup. %s", getJobMetadataString(staterootSetupJob)), ibu)
		}
		return requeueWithError(fmt.Errorf("failed to get stateroot setup job: %w", err))
	}

	r.Log.Info("Verifying stateroot setup job status")

	// job deletion is not allowed
	if staterootSetupJob.GetDeletionTimestamp() != nil {
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("stateroot job is marked to be deleted, this is not allowed. %s", getJobMetadataString(staterootSetupJob)), ibu)
	}

	// check .status
	_, staterootSetupFinishedType := common.IsJobFinished(staterootSetupJob)
	switch staterootSetupFinishedType {
	case "":
		common.LogPodLogs(staterootSetupJob, r.Log, r.Clientset)
		return prepInProgressRequeue(r.Log, fmt.Sprintf("Stateroot setup job in progress. %s", getJobMetadataString(staterootSetupJob)), ibu)
	case kbatch.JobFailed:
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("stateroot setup job failed to complete. %s", getJobMetadataString(staterootSetupJob)), ibu)
	case kbatch.JobComplete:
		// stop prep stage stateroot phase timing
		utils.StopPhase(r.Client, r.Log, ibu, PrepPhaseStateroot)
		r.Log.Info("Stateroot job completed successfully", "completion time", staterootSetupJob.Status.CompletionTime, "total time", staterootSetupJob.Status.CompletionTime.Sub(staterootSetupJob.Status.StartTime.Time))
	}

	r.Log.Info("Fetching precache job")
	precacheJob, err := precache.GetJob(ctx, r.Client)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			r.Log.Info("Launching a new precache job")
			if err := r.launchPrecaching(ctx, common.ContainersListFilePath, ibu); err != nil {
				return requeueWithError(fmt.Errorf("failed to launch precaching job: %w", err))
			}
			// start prep stage precache phase timing
			utils.StartPhase(r.Client, r.Log, ibu, PrepPhasePrecache)
			return prepInProgressRequeue(r.Log, fmt.Sprintf("Successfully launched a new job precache. %s", getJobMetadataString(precacheJob)), ibu)
		}
		return requeueWithError(fmt.Errorf("failed to get precache job: %w", err))
	}

	r.Log.Info("Verifying precache job status")

	// job deletion is not allowed
	if precacheJob.GetDeletionTimestamp() != nil {
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("precache job is marked to be deleted, this not allowed. %s", getJobMetadataString(precacheJob)), ibu)
	}

	// check .status
	_, precacheFinishedType := common.IsJobFinished(precacheJob)
	switch precacheFinishedType {
	case "":
		common.LogPodLogs(precacheJob, r.Log, r.Clientset) // pod logs
		return prepInProgressRequeue(r.Log, fmt.Sprintf("Precache job in progress. %s. %s", getJobMetadataString(precacheJob), precache.GetPrecacheStatusFileContent()), ibu)
	case kbatch.JobFailed:
		return prepFailDoNotRequeue(r.Log, fmt.Sprintf("precache job failed to complete. %s", getJobMetadataString(precacheJob)), ibu)
	case kbatch.JobComplete:
		// stop prep stage precache phase timing
		utils.StopPhase(r.Client, r.Log, ibu, PrepPhasePrecache)
		r.Log.Info("Precache job completed successfully", "completion time", precacheJob.Status.CompletionTime, "total time", precacheJob.Status.CompletionTime.Sub(precacheJob.Status.StartTime.Time))
	}

	r.Log.Info("All jobs completed successfully")
	utils.StopStageHistory(r.Client, r.Log, ibu) // stop prep history timing
	return prepSuccessDoNotRequeue(r.Log, ibu)
}

// prepInProgressRequeue helper function to stop everything when fail detected
func prepFailDoNotRequeue(log logr.Logger, msg string, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	log.Error(fmt.Errorf("prep stage failed"), msg)
	utils.SetPrepStatusFailed(ibu, msg)
	return doNotRequeue(), nil
}

// prepInProgressRequeue helper function when everything is a success at the end
func prepSuccessDoNotRequeue(log logr.Logger, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	msg := "Prep stage completed successfully"
	if _, exists := ibu.GetAnnotations()[extramanifest.ValidationWarningAnnotation]; exists {
		msg = fmt.Sprintf("Prep stage completed with extramanifests validation warning. Please check the annotation '%s' for details.", extramanifest.ValidationWarningAnnotation)
	}

	log.Info(msg)
	utils.SetPrepStatusCompleted(ibu, msg)
	return doNotRequeue(), nil
}

// prepInProgressRequeue helper function when we are waiting prep work to complete.
// Though requeue is event based, we are still requeue-ing with an interval to watch out for any unknown/random activities in the system and also log anything important
func prepInProgressRequeue(log logr.Logger, msg string, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	log.Info("Prep stage in progress", "msg", msg)
	utils.SetPrepStatusInProgress(ibu, msg)
	return requeueWithShortInterval(), nil
}

// getJobMetadataString a helper to append job metadata for helpful logs
func getJobMetadataString(job *kbatch.Job) string {
	if job == nil {
		return "job is nil"
	}
	return fmt.Sprintf("job-name: %s, job-namespace: %s", job.GetName(), job.GetNamespace())
}
