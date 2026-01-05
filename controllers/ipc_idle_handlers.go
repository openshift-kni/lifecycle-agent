package controllers

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type IPCIdleStageHandler struct {
	Client          client.Client
	NoncachedClient client.Reader
	ChrootOps       ops.Ops
	OstreeClient    ostreeclient.IClient
	RPMOstreeClient rpmostreeclient.IClient
}

func NewIPCIdleStageHandler(
	client client.Client,
	noncachedClient client.Reader,
	chrootOps ops.Ops,
	ostreeClient ostreeclient.IClient,
	rpmOstreeClient rpmostreeclient.IClient,
) IPConfigStageHandler {
	return &IPCIdleStageHandler{
		Client:          client,
		NoncachedClient: noncachedClient,
		ChrootOps:       chrootOps,
		OstreeClient:    ostreeClient,
		RPMOstreeClient: rpmOstreeClient,
	}
}

func (h *IPCIdleStageHandler) Handle(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("IPConfig Idle handler")
	logger.Info("Handler started")

	defer func() {
		logger.Info("Handler completed")
	}()

	// best effort to handle manual cleanup if failed
	if err := h.handleManualCleanup(ctx, ipc, logger); err != nil {
		logger.Error(err, "Manual cleanup handling failed")
		return requeueWithError(fmt.Errorf("failed to handle manual cleanup if failed: %w", err))
	}

	if isIPTransitionRequested(ipc) {
		if err := validateIPConfigStage(ipc); err != nil {
			controllerutils.SetIPStatusInvalidTransition(
				ipc, fmt.Sprintf("invalid IPConfig stage: %s", ipc.Spec.Stage),
			)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				logger.Error(uerr, "Failed to update IPConfig status after invalid transition")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
			}
			logger.Info(
				"Invalid transition requested",
				"stage", ipc.Spec.Stage,
			)
			return doNotRequeue(), nil
		}

		controllerutils.ClearIPInvalidTransitionStatusConditions(ipc)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to clear IPConfig invalid transition status conditions")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		controllerutils.SetIPIdleStatusFalse(
			ipc, controllerutils.ConditionReasons.InProgress,
			controllerutils.InProgressOfBecomingIdle,
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to update IPConfig status to in progress of becoming idle")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
	}

	if !controllerutils.IsIPStageInProgress(ipc, ipcv1.IPStages.Idle) {
		logger.Info("Stage is not in progress")
		return doNotRequeue(), nil
	}

	if shouldSkipClusterHealthChecks(ipc, controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation) {
		logger.Info(
			"Skipping cluster health checks due to annotation",
			"annotation", controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation,
		)
	} else {
		logger.Info("Running health checks")
		if err := CheckHealth(ctx, h.NoncachedClient, logger); err != nil {
			msg := fmt.Sprintf("Waiting for system to stabilize: %s", err.Error())
			controllerutils.SetIPIdleStatusFalse(ipc, controllerutils.ConditionReasons.Stabilizing, msg)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				logger.Error(uerr, "Failed to update IPConfig status after health check failure")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
			}
			logger.Info("Cluster not healthy yet", "reason", msg)
			return requeueWithHealthCheckInterval(), nil
		}
	}

	if err := h.cleanup(logger); err != nil {
		controllerutils.SetIPIdleStatusFalse(
			ipc,
			controllerutils.ConditionReasons.Failed,
			fmt.Sprintf("failed to cleanup: %v. Perform cleanup manually then add '%s' annotation to IPConfig CR to transition back to Idle",
				err,
				controllerutils.ManualCleanupAnnotation,
			),
		)
		if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
			logger.Error(uerr, "Failed to update IPConfig status after cleanup failure")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
		}
		logger.Error(err, "Cleanup failed")
		return requeueWithError(fmt.Errorf("failed to cleanup: %w", err))
	}

	controllerutils.ResetStatusConditions(&ipc.Status.Conditions, ipc.Generation)
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		logger.Error(err, "Failed to update IPConfig status after successful cleanup/reset")
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	logger.Info("Completed successfully")

	return doNotRequeue(), nil
}

func (h *IPCIdleStageHandler) cleanup(logger logr.Logger) error {
	if err := h.ChrootOps.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount sysroot: %w", err)
	}

	if err := h.cleanuoUnbootedStateroots(logger); err != nil {
		return fmt.Errorf("failed to clean up unbooted stateroots: %w", err)
	}

	if err := cleanupLCAWorkspace(h.ChrootOps); err != nil {
		return fmt.Errorf("failed to cleanup workspace: %w", err)
	}

	return nil
}

func cleanupLCAWorkspace(chrootOps ops.Ops) error {
	if _, err := chrootOps.StatFile(
		common.PathOutsideChroot(controllerutils.IPCWorkspacePath),
	); err != nil {
		return nil
	}
	if err := chrootOps.RemoveAllFiles(
		common.PathOutsideChroot(controllerutils.IPCWorkspacePath),
	); err != nil {
		return fmt.Errorf("removing %s failed: %w", controllerutils.IPCWorkspacePath, err)
	}
	return nil
}

// checkIPManualCleanup looks for ManualCleanupAnnotation on the IPConfig CR. If present, it removes
// the annotation and returns true so the reconcile loop can retry idle tasks.
func (h *IPCIdleStageHandler) checkIPManualCleanup(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
) (bool, error) {
	if _, ok := ipc.Annotations[controllerutils.ManualCleanupAnnotation]; ok {
		delete(ipc.Annotations, controllerutils.ManualCleanupAnnotation)
		if err := h.Client.Update(ctx, ipc); err != nil {
			return false, fmt.Errorf("failed to remove manual cleanup annotation from IPConfig: %w", err)
		}
		return true, nil
	}
	return false, nil
}

func (h *IPCIdleStageHandler) cleanuoUnbootedStateroots(logger logr.Logger) error {
	staterootsToRemove, err := getStaterootsToRemove(h.RPMOstreeClient)
	if err != nil {
		return fmt.Errorf("failed to determine stateroots to remove: %w", err)
	}
	logger.Info("Stateroots to remove", "stateroots", staterootsToRemove)

	if err := h.ChrootOps.RemountBoot(); err != nil {
		return fmt.Errorf("failed to remount boot: %w", err)
	}

	if err := removeBootDirsByStaterootPrefixes(logger, h.ChrootOps, staterootsToRemove); err != nil {
		return err
	}

	if err := CleanupUnbootedStateroots(logger, h.ChrootOps, h.OstreeClient, h.RPMOstreeClient); err != nil {
		return fmt.Errorf("failed to clean up unbooted stateroots: %w", err)
	}

	return nil
}

// removeBootDirsByStaterootPrefixes removes directories under /boot/ostree that
// start with any of the given stateroot names followed by a hyphen.
func removeBootDirsByStaterootPrefixes(
	logger logr.Logger,
	chrootOps ops.Ops,
	staterootsToRemove []string,
) error {
	bootOstreePath := common.PathOutsideChroot("/boot/ostree")
	entries, err := chrootOps.ReadDir(bootOstreePath)
	if err != nil {
		if chrootOps.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to list boot ostree directory %s: %w", bootOstreePath, err)
	}

	for _, stateroot := range staterootsToRemove {
		prefix := stateroot + "-"
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			dirPath := filepath.Join(bootOstreePath, name)
			logger.Info("Removing orphaned boot directory", "path", dirPath)
			if err := chrootOps.RemoveAllFiles(dirPath); err != nil {
				return fmt.Errorf("failed to remove boot directory %s: %w", dirPath, err)
			}
		}
	}
	return nil
}

func getStaterootsToRemove(rpmOstreeClient rpmostreeclient.IClient) ([]string, error) {
	status, err := rpmOstreeClient.QueryStatus()
	if err != nil {
		return nil, fmt.Errorf("failed to query status with rpmostree: %w", err)
	}

	toRemove := make([]string, 0)

	for i := len(status.Deployments) - 1; i >= 0; i-- {
		deployment := &status.Deployments[i]
		if deployment.Booted {
			continue
		}
		toRemove = append(toRemove, deployment.OSName)
	}

	return toRemove, nil
}

func (h *IPCIdleStageHandler) handleManualCleanup(
	ctx context.Context, ipc *ipcv1.IPConfig, logger logr.Logger,
) error {
	done, err := h.checkIPManualCleanup(ctx, ipc)
	if err != nil {
		return fmt.Errorf("failed to check manual cleanup: %w", err)
	}

	if done {
		controllerutils.SetIPIdleStatusFalse(
			ipc, controllerutils.ConditionReasons.InProgress,
			controllerutils.InProgressOfBecomingIdle,
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return fmt.Errorf("failed to update ipconfig status to in progress of becoming idle: %w", err)
		}
		logger.Info("Manual cleanup annotation is found, removed annotation and retrying idle tasks")
	}

	return nil
}
