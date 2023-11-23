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

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"github.com/openshift-kni/lifecycle-agent/internal/precache"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	ctrl "sigs.k8s.io/controller-runtime"
)

func (r *ImageBasedUpgradeReconciler) cleanupUnbootedStateroot(stateroot string) error {
	status, err := r.RPMOstreeClient.QueryStatus()
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
		_, err = r.Executor.Execute("ostree", "admin", "undeploy", fmt.Sprint(i))
		if err != nil {
			return fmt.Errorf("ostree undeploy %d failed: %w", i, err)
		}
	}

	// remove stateroot
	sysrootPath := fmt.Sprintf("/sysroot/ostree/deploy/%s", stateroot)
	if _, err := os.Stat(common.PathOutsideChroot(sysrootPath)); err == nil {
		_, err = r.Executor.Execute("unshare", "-m", "/bin/sh", "-c",
			fmt.Sprintf("\"mount -o remount,rw /sysroot && rm -rf %s\"", sysrootPath))
		if err != nil {
			return fmt.Errorf("removing stateroot failed: %w", err)
		}
	}

	return nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleAbort(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (ctrl.Result, error) {
	// Cleanup precaching resources
	r.Log.Info("Cleanup precaching resources")
	if err := precache.Cleanup(ctx, r.Client); err != nil {
		r.Log.Info("Failed to cleanup precaching resources")
		return requeueWithError(err)
	}

	stateroot := getStaterootName(ibu.Spec.SeedImageRef.Version)
	r.Log.Info("Cleanup stateroot", "stateroot", stateroot)
	err := r.cleanupUnbootedStateroot(stateroot)
	if err != nil {
		return requeueWithError(err)
	}
	err = cleanupIBUFiles()
	return requeueWithError(err)
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

func cleanupIBUFiles() error {
	if _, err := os.Stat(common.PathOutsideChroot(utils.IBUWorkspacePath)); err == nil {
		if err := os.RemoveAll(filepath.Join(utils.Host, utils.IBUWorkspacePath)); err != nil {
			return fmt.Errorf("removing %s failed: %w", utils.IBUWorkspacePath, err)
		}
	}
	return nil
}
