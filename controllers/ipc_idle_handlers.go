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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	logger := log.FromContext(ctx).WithName("IPConfigIdle")
	logger.Info("Starting handleIdle")

	if err := h.handleManualCleanupIfFailed(ctx, ipc, logger); err != nil {
		return requeueWithError(fmt.Errorf("failed to handle manual cleanup if failed: %w", err))
	}

	if isIPTransitionRequested(ipc) && ipc.Status.ValidNextStages != nil {
		if err := validateIPConfigStage(ipc); err != nil {
			controllerutils.SetIPIdleStatusFalse(
				ipc,
				controllerutils.ConditionReasons.InvalidTransition,
				fmt.Sprintf("invalid IPConfig stage: %s", ipc.Spec.Stage),
			)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}
			return doNotRequeue(), nil
		}
	}

	if shouldSkipIPClusterHealthChecks(ipc) {
		logger.Info(
			"Skipping cluster health checks due to annotation",
			"annotation", controllerutils.SkipIPConfigClusterHealthChecksAnnotation,
		)
	} else {
		logger.Info("Running health checks")
		if err := CheckHealth(ctx, h.NoncachedClient, logger); err != nil {
			msg := fmt.Sprintf("Waiting for system to stabilize: %s", err.Error())
			controllerutils.SetIPIdleStatusFalse(ipc, controllerutils.ConditionReasons.Stabilizing, msg)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
			}
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
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
		}
		return requeueWithError(fmt.Errorf("failed to cleanup: %w", err))
	}

	controllerutils.ResetStatusConditions(&ipc.Status.Conditions, ipc.Generation)
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	logger.Info("handleIdle completed successfully")

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

func (h *IPCIdleStageHandler) handleManualCleanupIfFailed(
	ctx context.Context, ipc *ipcv1.IPConfig, logger logr.Logger,
) error {
	idleCond := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.Idle))
	if idleCond != nil && idleCond.Status == metav1.ConditionFalse &&
		idleCond.Reason == string(controllerutils.ConditionReasons.Failed) {
		done, err := h.checkIPManualCleanup(ctx, ipc)
		if err != nil {
			return fmt.Errorf("failed to check manual cleanup: %w", err)
		}

		if done {
			logger.Info("Manual cleanup annotation is found, removed annotation and retrying idle tasks")
		}
	}

	return nil
}
