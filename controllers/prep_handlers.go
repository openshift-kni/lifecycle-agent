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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"

	"github.com/openshift-kni/lifecycle-agent/internal/generated"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) launchGetSeedImage(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, imageListFile, progressfile string) (result ctrl.Result, err error) {
	if err = os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.IBUWorkspacePath, utils.PrepGetSeedImage)
	scriptcontent, _ := generated.Asset(utils.PrepGetSeedImage)
	err = os.WriteFile(common.PathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		err = fmt.Errorf("failed to write handler script: %s, %w", common.PathOutsideChroot(scriptname), err)
		return
	}
	r.Log.Info("Handler script written")

	// Use cluster wide pull-secret by default
	pullSecretFilename := common.ImageRegistryAuthFile

	if ibu.Spec.SeedImageRef.PullSecretRef != nil {
		var pullSecret string
		pullSecret, err = utils.LoadSecretData(ctx, r.Client, ibu.Spec.SeedImageRef.PullSecretRef.Name, ibu.Namespace, corev1.DockerConfigJsonKey)
		if err != nil {
			err = fmt.Errorf("failed to retrieve pull-secret from secret %s, err: %w", ibu.Spec.SeedImageRef.PullSecretRef.Name, err)
			return
		}

		pullSecretFilename = filepath.Join(utils.IBUWorkspacePath, "seed-pull-secret")
		if err = os.WriteFile(common.PathOutsideChroot(pullSecretFilename), []byte(pullSecret), 0o600); err != nil {
			err = fmt.Errorf("failed to write seed image pull-secret to file %s, err: %w", pullSecretFilename, err)
			return
		}
	}

	go r.Executor.Execute(scriptname, "--seed-image", ibu.Spec.SeedImageRef.Image, "--progress-file", progressfile, "--image-list-file", imageListFile, "--pull-secret", pullSecretFilename)
	result = requeueWithShortInterval()

	return
}

func readPrecachingList(imageListFile string) (imageList []string, err error) {
	var content []byte
	content, err = os.ReadFile(common.PathOutsideChroot(imageListFile))
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")

	// Filter out empty lines
	for _, line := range lines {
		if line != "" {
			imageList = append(imageList, line)
		}
	}

	return imageList, nil
}

func updateProgressFile(path, progress string) error {
	return os.WriteFile(common.PathOutsideChroot(path), []byte(progress), 0o700)
}

func (r *ImageBasedUpgradeReconciler) launchPrecaching(ctx context.Context, imageListFile, progressfile string) (result ctrl.Result, err error) {

	imageList, err := readPrecachingList(imageListFile)
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

func getSeedImageMountpoint(seedImage, output string) (string, error) {
	var data []interface{}
	if err := json.Unmarshal([]byte(output), &data); err != nil {
		return "", fmt.Errorf("failed to unmarshal: %w", err)
	}
	for _, img := range data {
		v := img.(map[string]interface{})
		repos := v["Repositories"].([]interface{})
		if len(repos) == 0 {
			continue
		}
		repoName := repos[0].(string)
		if repoName == seedImage {
			return v["mountpoint"].(string), nil
		}
	}
	return "", fmt.Errorf("failed to find seedImage mountpoint")
}

var osReadFile = os.ReadFile

func getBootedStaterootID(path string) (string, error) {
	data, err := osReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading %s: %w", path, err)
	}
	var status rpmostreeclient.Status
	if err := json.Unmarshal(data, &status); err != nil {
		return "", fmt.Errorf("failed unmarshalling %s: %w", path, err)
	}
	for _, deploy := range status.Deployments {
		if deploy.Booted {
			return deploy.ID, nil
		}
	}
	return "", fmt.Errorf("failed finding booted stateroot")
}

func getVersionFromManifest(path string) (string, error) {
	data, err := osReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading %s: %w", path, err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("failed to unmarshal %s: %w", path, err)
	}
	version, ok := v["version"].(string)
	if !ok {
		return "", fmt.Errorf("failed to get version from %s: %w", path, err)
	}
	return version, nil
}

func getKernelArgumentsFromMCOFile(path string) ([]string, error) {
	data, err := osReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed reading %s: %w", path, err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("failed to unsmarshal %s: %w", path, err)
	}
	spec, ok := v["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("failed getting spec in %s", path)
	}
	kargs, ok := spec["kernelArguments"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("failed getting kernelArguments in %s", path)
	}
	args := make([]string, len(kargs)*2)
	for i, karg := range kargs {
		args[2*i] = "--karg-append"
		args[2*i+1] = karg.(string)
	}
	return args, nil
}

func getStaterootPath(osname string) string {
	return fmt.Sprintf("/ostree/deploy/%s", osname)
}

func getDeploymentDirPath(osname, deploymentID string) string {
	deployDirName := strings.Split(deploymentID, "-")[1]
	return filepath.Join(getStaterootPath(osname), fmt.Sprintf("deploy/%s", deployDirName))
}

func getDeploymentOriginPath(osname, deploymentID string) string {
	originName := fmt.Sprintf("%s.origin", strings.Split(deploymentID, "-")[1])
	return filepath.Join(getStaterootPath(osname), fmt.Sprintf("deploy/%s", originName))
}

func (r *ImageBasedUpgradeReconciler) restoreETC(mountpoint, osname, deploymentID string) error {

	_, err := r.Executor.Execute(
		"tar", "xzf",
		filepath.Join(mountpoint, "etc.tgz"),
		"-C", getDeploymentDirPath(osname, deploymentID), "--selinux",
	)
	if err != nil {
		return fmt.Errorf("failed to extract seed etc: %w", err)
	}

	file, err := os.Open(filepath.Join(common.PathOutsideChroot(mountpoint), "etc.deletions"))
	if err != nil {
		return fmt.Errorf("failed to open etc.deletions: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fileToRemove := strings.Trim(scanner.Text(), " ")
		filePath := common.PathOutsideChroot(filepath.Join(getDeploymentDirPath(osname, deploymentID), fileToRemove))
		r.Log.Info("removing file: " + filePath)
		err = os.Remove(filePath)
		if err != nil {
			return fmt.Errorf("failed to remove %s: %w", filePath, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error while reading %s: %w", file.Name(), err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) backupCertificates(ctx context.Context, osname string) error {
	r.Log.Info("Backing up certificates")
	certsDir := filepath.Join(getStaterootPath(osname), "/var/opt/openshift/certs")
	if err := os.MkdirAll(common.PathOutsideChroot(certsDir), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create cert directory %s: %w", certsDir, err)
	}

	adminKubeConfigClientCA, err := r.ManifestClient.GetConfigMapData(ctx, "admin-kubeconfig-client-ca", "openshift-config", "ca-bundle.crt")
	if err != nil {
		return err
	}
	if err := os.WriteFile(common.PathOutsideChroot(filepath.Join(certsDir, "admin-kubeconfig-client-ca.crt")), []byte(adminKubeConfigClientCA), 0o644); err != nil {
		return err
	}

	for _, cert := range common.CertPrefixes {
		servingSignerKey, err := r.ManifestClient.GetSecretData(ctx, cert, "openshift-kube-apiserver-operator", "tls.key")
		if err != nil {
			return err
		}
		if err := os.WriteFile(common.PathOutsideChroot(path.Join(certsDir, cert+".key")), []byte(servingSignerKey), 0o644); err != nil {
			return err
		}
	}

	ingressOperatorKey, err := r.ManifestClient.GetSecretData(ctx, "router-ca", "openshift-ingress-operator", "tls.key")
	if err != nil {
		return err
	}
	if err := os.WriteFile(common.PathOutsideChroot(filepath.Join(certsDir, "ingresskey-ingress-operator.key")), []byte(ingressOperatorKey), 0o644); err != nil {
		return err
	}

	r.Log.Info("Certificates backed up successfully")
	return nil
}

func (r *ImageBasedUpgradeReconciler) SetupStateroot(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) error {
	r.Log.Info("Start setupstateroot")

	workspaceOutsideChroot, err := os.MkdirTemp(common.PathOutsideChroot("/var/tmp"), "")
	if err != nil {
		return fmt.Errorf("failed to create temp directory %w", err)
	}

	defer func() {
		if err := os.RemoveAll(workspaceOutsideChroot); err != nil {
			r.Log.Error(err, "failed to clean up "+workspaceOutsideChroot)
		}
	}()

	workspace, err := filepath.Rel(utils.Host, workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("failed to get workspace relative path %w", err)
	}
	r.Log.Info("workspace:" + workspace)

	if _, err = r.Executor.Execute("mount", "/sysroot", "-o", "remount,rw"); err != nil {
		return fmt.Errorf("failed to remount /sysroot: %w", err)
	}

	if _, err := r.Executor.Execute("podman", "pull", ibu.Spec.SeedImageRef.Image); err != nil {
		return fmt.Errorf("failed to pull seed image: %w", err)
	}

	mountpoint, err := r.Executor.Execute("podman", "image", "mount", ibu.Spec.SeedImageRef.Image)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}

	// podmanOutput, err := r.Executor.Execute("podman", "image", "mount", "--format", "json")
	// if err != nil {
	// return fmt.Errorf("failed get mounted images: %w", err)
	// }
	// mountpoint, err := getSeedImageMountpoint(ibu.Spec.SeedImageRef.Image, podmanOutput)
	// if err != nil {
	// return err
	// }

	ostreeRepo := filepath.Join(workspace, "ostree")
	if err = os.Mkdir(common.PathOutsideChroot(ostreeRepo), 0o755); err != nil {
		return fmt.Errorf("failed to create ostree repo directory: %w", err)
	}

	if _, err = r.Executor.Execute(
		"tar", "xzf", fmt.Sprintf("%s/ostree.tgz", mountpoint), "--selinux", "-C", ostreeRepo,
	); err != nil {
		return fmt.Errorf("failed to extract ostree.tgz: %w", err)
	}

	// example:
	// seedBootedId: rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedDeployment: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedRef: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c
	seedBootedId, err := getBootedStaterootID(filepath.Join(common.PathOutsideChroot(mountpoint), "rpm-ostree.json"))
	if err != nil {
		return fmt.Errorf("failed to get booted stateroot id: %w", err)
	}
	seedBootedDeployment := strings.Split(seedBootedId, "-")[1]
	seedBootedRef := strings.Split(seedBootedDeployment, ".")[0]

	version, err := getVersionFromManifest(filepath.Join(common.PathOutsideChroot(mountpoint), "manifest.json"))
	if err != nil {
		return fmt.Errorf("failed to get version from manifest.json: %w", err)
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

	kargs, err := getKernelArgumentsFromMCOFile(filepath.Join(common.PathOutsideChroot(mountpoint), "mco-currentconfig.json"))
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

	if _, err = r.Executor.Execute(
		"cp",
		filepath.Join(mountpoint, fmt.Sprintf("ostree-%s.origin", seedBootedDeployment)),
		getDeploymentOriginPath(osname, deploymentID),
	); err != nil {
		return fmt.Errorf("failed to restore origin file: %w", err)
	}

	if _, err = r.Executor.Execute(
		"tar", "xzf",
		filepath.Join(mountpoint, "var.tgz"),
		"-C", getStaterootPath(osname), "--selinux",
	); err != nil {
		return fmt.Errorf("failed to restore var directory: %w", err)
	}

	if err = r.restoreETC(mountpoint, osname, deploymentID); err != nil {
		return fmt.Errorf("failed to restore etc directory: %w", err)
	}

	// Why we need this?
	// TODO: WAIT FOR API

	r.backupCertificates(ctx, osname)

	if _, err = r.Executor.Execute(
		"cp",
		filepath.Join(mountpoint, "manifest.json"),
		filepath.Join(
			getStaterootPath(osname),
			"/var/opt/openshift/seed_manifest.json",
		),
	); err != nil {
		return fmt.Errorf("failed to copy manifest.json: %w", err)
	}

	return nil
}

func (r *ImageBasedUpgradeReconciler) launchSetupStateroot(ctx context.Context,
	ibu *lcav1alpha1.ImageBasedUpgrade, progressfile string) (result ctrl.Result, err error) {
	if err = updateProgressFile(progressfile, "started-stateroot"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return
	}
	if err = r.SetupStateroot(ctx, ibu); err != nil {
		if err = updateProgressFile(progressfile, "Failed"); err != nil {
			r.Log.Error(err, "failed to update progress file")
			return
		}
		return
	}
	if err = updateProgressFile(progressfile, "completed-stateroot"); err != nil {
		r.Log.Error(err, "failed to update progress file")
		return
	}

	result = requeueWithShortInterval()
	return
}

func (r *ImageBasedUpgradeReconciler) runCleanup(
	ibu *lcav1alpha1.ImageBasedUpgrade) {
	if err := os.Chdir("/"); err != nil {
		return
	}

	// Write the script
	scriptname := filepath.Join(utils.IBUWorkspacePath, utils.PrepCleanup)
	scriptcontent, _ := generated.Asset(utils.PrepCleanup)
	err := os.WriteFile(common.PathOutsideChroot(scriptname), scriptcontent, 0o700)

	if err != nil {
		r.Log.Error(err, fmt.Sprintf("failed to write handler script: %s", common.PathOutsideChroot(scriptname)))
		return
	}
	r.Log.Info("Handler script written")
	// This should be a quick operation, so we can run it within the handler thread
	r.Executor.Execute(scriptname, "--seed-image", ibu.Spec.SeedImageRef.Image)

	return
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handlePrep(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (result ctrl.Result, err error) {
	result = doNotRequeue()

	_, err = os.Stat(utils.Host)
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
				r.Log.Error(err, "Failed to launch get-seed-image phase")
				return
			}
		} else if progress == "completed-stateroot" {
			result, err = r.launchPrecaching(ctx, imageListFile, progressfile)
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
			r.runCleanup(ibu)

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
