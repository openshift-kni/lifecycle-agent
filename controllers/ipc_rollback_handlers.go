package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/samber/lo"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	IPConfigRollbackPhasePrepivot  string = "RollbackPrePivot"
	IPConfigRollbackPhasePostpivot string = "RollbackPostPivot"
)

type IPCRollbackTwoPhaseHandler struct {
	Client          client.Client
	NoncachedClient client.Reader
	RPMOstreeClient rpmostreeclient.IClient
	Ops             ops.Ops
}

func NewIPCRollbackTwoPhaseHandler(
	client client.Client,
	noncachedClient client.Reader,
	rpmostreeClient rpmostreeclient.IClient,
	ops ops.Ops,
) IPConfigTwoPhaseHandlerInterface {
	return &IPCRollbackTwoPhaseHandler{
		Client:          client,
		NoncachedClient: noncachedClient,
		RPMOstreeClient: rpmostreeClient,
		Ops:             ops,
	}
}

type IPCRollbackStageHandler struct {
	Client          client.Client
	RPMOstreeClient rpmostreeclient.IClient
	ChrootOps       ops.Ops
	PhasesHandler   IPConfigTwoPhaseHandlerInterface
}

func NewIPCRollbackStageHandler(
	client client.Client,
	rpmOstreeClient rpmostreeclient.IClient,
	chrootOps ops.Ops,
	phasesHandler IPConfigTwoPhaseHandlerInterface,
) IPConfigStageHandler {
	return &IPCRollbackStageHandler{
		Client:          client,
		RPMOstreeClient: rpmOstreeClient,
		ChrootOps:       chrootOps,
		PhasesHandler:   phasesHandler,
	}
}

// Handle executes the Rollback stage state machine
func (h *IPCRollbackStageHandler) Handle(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("IPConfig Rollback handler")
	logger.Info("Handler started")

	defer func() {
		logger.Info("Handler completed")
	}()

	if isIPTransitionRequested(ipc) {
		if err := validateIPConfigStage(ipc); err != nil {
			controllerutils.SetIPStatusInvalidTransition(
				ipc, fmt.Sprintf("invalid IPConfig stage: %s", ipc.Spec.Stage),
			)
			if err := h.Client.Status().Update(ctx, ipc); err != nil {
				logger.Error(err, "Failed to update IPConfig status after invalid transition")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}
			return doNotRequeue(), nil
		}

		if err := h.validateRollbackStart(); err != nil {
			controllerutils.SetIPRollbackStatusFailed(
				ipc,
				fmt.Sprintf("rollback validation failed: %s", err.Error()),
			)
			if err := h.Client.Status().Update(ctx, ipc); err != nil {
				logger.Error(err, "Failed to update IPConfig status after validation failure")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}

			logger.Info("Validation failed", "reason", err.Error())
			return doNotRequeue(), nil
		}

		controllerutils.ClearIPInvalidTransitionStatusConditions(ipc)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to clear IPConfig invalid transition status conditions")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		controllerutils.SetIPRollbackStatusInProgress(ipc, controllerutils.RollbackRequested)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to update IPConfig rollback status to rollback requested")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
	}

	// stop when completed or failed
	if !controllerutils.IsIPStageInProgress(ipc, ipcv1.IPStages.Rollback) {
		logger.Info("Stage is not in progress")
		return doNotRequeue(), nil
	}

	targetStaterootBooted, err := isTargetStaterootBooted(ipc, h.RPMOstreeClient)
	if err != nil {
		logger.Error(err, "Failed to determine whether target stateroot is booted")
		return requeueWithError(fmt.Errorf("failed to check if target stateroot is booted: %w", err))
	}

	if lo.FromPtr(targetStaterootBooted) {
		logger.Info(
			"Rollback stage handler: running pre-pivot",
			"targetStaterootBooted", lo.FromPtr(targetStaterootBooted),
		)
		result, err := h.PhasesHandler.PrePivot(ctx, ipc, logger)
		if err != nil {
			logger.Error(err, "Pre-pivot phase failed")
			return result, fmt.Errorf("failed to run rollback pre pivot: %w", err)
		}
		logger.Info("Returning after pre-pivot phase")
		return result, nil
	}

	controllerutils.StopIPPhase(h.Client, logger, ipc, IPConfigRollbackPhasePrepivot)

	result, err := h.PhasesHandler.PostPivot(ctx, ipc, logger)
	if err != nil {
		logger.Error(err, "Post-pivot phase failed")
		return result, fmt.Errorf("failed to run rollback post pivot: %w", err)
	}

	if result.RequeueAfter != 0 {
		logger.Info("Returning after post-pivot phase")
		return result, nil
	}

	controllerutils.StopIPStageHistory(h.Client, logger, ipc)
	controllerutils.SetIPRollbackStatusCompleted(ipc, controllerutils.RollbackCompleted)
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		logger.Error(err, "Failed to update IPConfig rollback status to completed")
		return requeueWithError(fmt.Errorf("failed to update ipconfig rollback status: %w", err))
	}

	logger.Info("Completed successfully")

	return result, nil
}

func (r *IPCRollbackTwoPhaseHandler) PrePivot(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
	logger logr.Logger,
) (ctrl.Result, error) {
	controllerutils.StartIPPhase(r.Client, logger, ipc, IPConfigRollbackPhasePrepivot)
	logger.Info("Starting pre-pivot phase")

	stateroot, err := r.RPMOstreeClient.GetUnbootedStaterootName()
	if err != nil {
		controllerutils.SetIPRollbackStatusFailed(
			ipc,
			fmt.Errorf("failed to determine unbooted stateroot: %w", err).Error(),
		)
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		return requeueWithError(fmt.Errorf("failed to determine unbooted stateroot: %w", err))
	}

	if err := r.Ops.RemountSysroot(); err != nil {
		controllerutils.SetIPRollbackStatusFailed(
			ipc,
			fmt.Errorf("failed to remount sysroot: %w", err).Error(),
		)
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		return requeueWithError(fmt.Errorf("failed to remount sysroot: %w", err))
	}

	logger.Info("Save the IPConfig CR to the old state root before pivot")
	ipcsavePath := common.PathOutsideChroot(
		filepath.Join(
			common.GetStaterootPath(stateroot),
			common.IPCFilePath,
		),
	)

	data, err := json.Marshal(ipc)
	if err != nil {
		controllerutils.SetIPRollbackStatusFailed(
			ipc,
			fmt.Errorf("failed to marshal IPConfig CR: %w", err).Error(),
		)
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		return requeueWithError(fmt.Errorf("failed to marshal IPConfig CR: %w", err))
	}

	if err := r.Ops.WriteFile(ipcsavePath, data, 0o600); err != nil {
		controllerutils.SetIPRollbackStatusFailed(
			ipc,
			fmt.Errorf("failed to save IPConfig CR before pivot: %w", err).Error(),
		)
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		return requeueWithError(fmt.Errorf("failed to save IPConfig CR before pivot: %w", err))
	}

	if err := r.scheduleIPConfigRollback(logger, stateroot); err != nil {
		controllerutils.SetIPRollbackStatusFailed(
			ipc,
			fmt.Errorf("failed to schedule ip-config rollback: %w", err).Error(),
		)
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		return requeueWithError(err)
	}

	// should not reach here on successful ip-config rollback

	return requeueWithShortInterval(), nil
}

func (r *IPCRollbackTwoPhaseHandler) PostPivot(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
	logger logr.Logger,
) (ctrl.Result, error) {
	controllerutils.StartIPPhase(r.Client, logger, ipc, IPConfigRollbackPhasePostpivot)
	logger.Info("Starting post-pivot phase")

	if shouldSkipClusterHealthChecks(
		ipc,
		controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation,
	) {
		logger.Info(
			"Skipping cluster health checks due to annotation",
			"annotation", controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation,
		)
	} else {
		log.FromContext(ctx).Info("Starting health check after rollback")
		if err := CheckHealth(ctx, r.NoncachedClient, log.FromContext(ctx)); err != nil {
			controllerutils.SetIPRollbackStatusInProgress(
				ipc,
				fmt.Sprintf("Waiting for system to stabilize: %s", err.Error()),
			)
			if err := r.Client.Status().Update(ctx, ipc); err != nil {
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}

			return requeueWithHealthCheckInterval(), nil
		}
	}

	controllerutils.StopIPPhase(r.Client, logger, ipc, IPConfigRollbackPhasePostpivot)
	logger.Info("Completed rollback PostPivot handler")

	return doNotRequeue(), nil
}

func (r *IPCRollbackTwoPhaseHandler) scheduleIPConfigRollback(
	logger logr.Logger,
	stateroot string,
) error {
	logger.Info("Scheduling lca-cli ip-config rollback via systemd-run", "stateroot", stateroot)
	args := []string{
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigRollbackUnit,
		"--description", controllerutils.IPConfigRollbackDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "rollback",
		"--stateroot", stateroot,
	}
	if _, err := r.Ops.RunSystemdAction(args...); err != nil {
		return fmt.Errorf("failed to schedule ip-config rollback: %w", err)
	}

	return nil
}

func (h *IPCRollbackStageHandler) validateRollbackStart() error {
	if err := h.validateUnbootedStaterootAvailable(); err != nil {
		return err
	}

	return nil
}

func (h *IPCRollbackStageHandler) validateUnbootedStaterootAvailable() error {
	isUnbootedStaterootAvailable, err := isUnbootedStaterootAvailable(h.RPMOstreeClient)
	if err != nil {
		return fmt.Errorf("failed to check if unbooted stateroot is available: %w", err)
	}

	if !lo.FromPtr(isUnbootedStaterootAvailable) {
		return fmt.Errorf("unbooted stateroot is not available")
	}

	return nil
}
