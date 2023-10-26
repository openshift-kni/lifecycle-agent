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

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	ctrl "sigs.k8s.io/controller-runtime"
)

func cleanupUnbootedStateroot(stateroot string) error {
	status, err := rpmostreeclient.QueryStatusChroot(utils.Host)
	if err != nil {
		return err
	}

	undeployIndices := make([]int, 0)
	for index, deployment := range status.Deployments {
		if deployment.OSName != stateroot {
			continue
		}
		// make sure none of the deployments in the stateroot is booted
		if deployment.Booted {
			return fmt.Errorf("failed abort: deployment %d in stateroot %s is booted", index, stateroot)
		}
		undeployIndices = append(undeployIndices, index)
	}

	// since undeploy shifts the order, undeploy in the reverse order
	for i := len(undeployIndices) - 1; i >= 0; i-- {
		ostreeUndeployCommand := fmt.Sprintf("ostree admin undeploy %d", i)
		err := utils.ExecuteChrootCmd(utils.Host, ostreeUndeployCommand)
		if err != nil {
			return fmt.Errorf("ostree undeploy %d failed: %w", i, err)
		}
	}

	// remove stateroot
	sysrootPath := fmt.Sprintf("/sysroot/ostree/deploy/%s", stateroot)
	if _, err := os.Stat(pathOutsideChroot(sysrootPath)); err == nil {
		removeSysrootCommand := fmt.Sprintf("unshare -m /bin/sh -c \"mount -o remount,rw /sysroot && rm -rf %s\"", sysrootPath)
		err := utils.ExecuteChrootCmd(utils.Host, removeSysrootCommand)
		if err != nil {
			return fmt.Errorf("removing stateroot failed: %w", err)
		}
	}

	return nil
}

func cleanupIBUFiles() error {
	if _, err := os.Stat(pathOutsideChroot(utils.IBUWorkspacePath)); err == nil {
		if err := os.RemoveAll(filepath.Join(utils.Host, utils.IBUWorkspacePath)); err != nil {
			return fmt.Errorf("removing %s failed: %w", utils.IBUWorkspacePath, err)
		}
	}
	return nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbort(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	stateroot := getStaterootName(ibu.Spec.SeedImageRef.Version)
	r.Log.Info("Cleanup stateroot", "stateroot", stateroot)
	err := cleanupUnbootedStateroot(stateroot)
	if err != nil {
		return doNotRequeue(), err
	}
	err = cleanupIBUFiles()
	return doNotRequeue(), err
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbortFailure(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	// TODO actual steps
	// If succeeds, return doNotRequeue
	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleFinalize(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	// TODO actual steps
	// If succeeds, return doNotRequeue
	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleFinalizeFailure(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {

	// TODO actual steps
	// If succeeds, return doNotRequeue
	return doNotRequeue(), nil
}
