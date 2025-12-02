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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	ctrl "sigs.k8s.io/controller-runtime"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	corev1 "k8s.io/api/core/v1"
)

func (r *ImageBasedUpgradeReconciler) getRollbackAvailabilityExpiration() (time.Time, error) {
	expiry := time.Time{}

	stateroot, err := r.RPMOstreeClient.GetUnbootedStaterootName()
	if err != nil {
		return expiry, fmt.Errorf("unable to determine unbooted stateroot: %w", err)
	}

	staterootPath := common.PathOutsideChroot(common.GetStaterootPath(stateroot))

	certfiles := []string{
		"/var/lib/kubelet/pki/kubelet-client-current.pem",
		"/var/lib/kubelet/pki/kubelet-server-current.pem",
	}

	for _, certfile := range certfiles {
		fname := filepath.Join(staterootPath, certfile)

		// Evaluate symlinks, if needed
		if _, err := os.Stat(fname); err != nil {
			if _, err = os.Lstat(fname); err != nil {
				r.Log.Error(err, "unable to read file", "filepath", fname)
				continue
			} else if target, err := os.Readlink(fname); err != nil {
				r.Log.Error(err, "unable to read link", "filepath", fname)
				continue
			} else {
				fname = filepath.Join(staterootPath, target)
			}
		}

		certs, err := tls.LoadX509KeyPair(fname, fname)
		if err != nil {
			r.Log.Error(err, "failed to parse cert file", "certfile", certfile)
			continue
		}

		for _, cert := range certs.Certificate {
			// Check certificate expiry
			parsed, err := x509.ParseCertificate(cert)
			if err != nil {
				r.Log.Error(err, "failed to parse cert from file", "certfile", certfile)
				continue
			}

			if expiry.Equal(time.Time{}) || expiry.After(parsed.NotAfter) {
				expiry = parsed.NotAfter
			}
		}
	}

	if expiry.Equal(time.Time{}) {
		return expiry, fmt.Errorf("unable to determine control plane expiry for staterootPath=%s", staterootPath)
	}

	// Subtract 30 minutes from the expiry time
	return expiry.Add(time.Minute * -30), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) startRollback(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	utils.SetRollbackStatusInProgress(ibu, "Initiating rollback")

	stateroot, err := r.RPMOstreeClient.GetUnbootedStaterootName()
	if err != nil {
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	if err := r.Ops.RemountSysroot(); err != nil {
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	r.Log.Info("Finding unbooted deployment")
	deploymentIndex, err := r.RPMOstreeClient.GetUnbootedDeploymentIndex()
	if err != nil {
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	// Set the new default deployment
	r.Log.Info("Checking for set-default feature")

	if r.OstreeClient.IsOstreeAdminSetDefaultFeatureEnabled() {
		r.Log.Info("set-default feature available")

		if err = r.OstreeClient.SetDefaultDeployment(deploymentIndex); err != nil {
			utils.SetRollbackStatusFailed(ibu, err.Error())
			return doNotRequeue(), nil
		}
	} else {
		r.Log.Info("set-default feature not available")

		// Check to make sure the default deployment is set
		if deploymentIndex != 0 {
			msg := "default deployment must be manually set for next boot"
			utils.SetRollbackStatusInProgress(ibu, msg)
			r.Log.Info(msg)
			return requeueWithShortInterval(), nil
		}
	}

	// Clear the rollback availability expiration status field
	ibu.Status.RollbackAvailabilityExpiration.Reset()

	// Update in-progress message
	utils.SetRollbackStatusInProgress(ibu, "Completing rollback")
	if updateErr := utils.UpdateIBUStatus(ctx, r.Client, ibu); updateErr != nil {
		r.Log.Error(updateErr, "failed to update IBU CR status")
	}

	// Save the CR for post-reboot restore
	r.Log.Info("Save the IBU CR to the old state root before pivot")
	filePath := common.PathOutsideChroot(filepath.Join(common.GetStaterootPath(stateroot), utils.IBUFilePath))
	if err := lcautils.MarshalToFile(ibu, filePath); err != nil {
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	// Write an event to indicate reboot attempt
	r.Recorder.Event(ibu, corev1.EventTypeNormal, "Reboot", "System will now reboot for rollback")
	err = r.RebootClient.RebootToNewStateRoot("rollback")
	if err != nil {
		r.Log.Error(err, "")
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	return doNotRequeue(), nil
}

func (r *ImageBasedUpgradeReconciler) finishRollback(ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	utils.SetRollbackStatusCompleted(ibu)

	return doNotRequeue(), nil
}

//nolint:unparam
func (r *ImageBasedUpgradeReconciler) handleRollback(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (ctrl.Result, error) {
	origStaterootBooted, err := r.RebootClient.IsOrigStaterootBooted(ibu.Spec.SeedImageRef.Version)
	if err != nil {
		utils.SetRollbackStatusFailed(ibu, err.Error())
		return doNotRequeue(), nil
	}

	if origStaterootBooted {
		r.Log.Info("Pivot for rollback successful, starting post pivot steps")
		utils.StopStageHistory(r.Client, r.Log, ibu)
		return r.finishRollback(ibu)
	} else {
		r.Log.Info("Starting pre pivot for rollback steps and will pivot to previous stateroot with a reboot")
		return r.startRollback(ctx, ibu)
	}
}
