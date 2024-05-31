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
	"sync"
	"time"

	"k8s.io/client-go/util/retry"

	"k8s.io/client-go/kubernetes"

	"github.com/samber/lo"

	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	kbatch "k8s.io/api/batch/v1"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageBasedUpgradeReconciler reconciles a ImageBasedUpgrade object
type ImageBasedUpgradeReconciler struct {
	client.Client
	NoncachedClient client.Reader
	UpgradeHandler  UpgradeHandler
	Log             logr.Logger
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	Precache        *precache.PHandler
	BackupRestore   backuprestore.BackuperRestorer
	ExtraManifest   extramanifest.EManifestHandler
	RPMOstreeClient rpmostreeclient.IClient
	Executor        ops.Execute
	OstreeClient    ostreeclient.IClient
	Ops             ops.Ops
	RebootClient    reboot.RebootIntf
	Mux             *sync.Mutex
	Clientset       *kubernetes.Clientset
}

func doNotRequeue() ctrl.Result {
	return ctrl.Result{}
}

func requeueWithError(err error) (ctrl.Result, error) {
	// can not be fixed by user during reconcile
	return ctrl.Result{}, err
}

func requeueImmediately() ctrl.Result {
	// Allow a brief pause in case there's a delay with a DB Update
	return requeueWithCustomInterval(2 * time.Second)
}

func requeueWithShortInterval() ctrl.Result {
	return requeueWithCustomInterval(30 * time.Second)
}

func requeueWithMediumInterval() ctrl.Result {
	return requeueWithCustomInterval(1 * time.Minute)
}

//nolint:unused
func requeueWithLongInterval() ctrl.Result {
	return requeueWithCustomInterval(5 * time.Minute)
}

func requeueWithHealthCheckInterval() ctrl.Result {
	return requeueWithCustomInterval(20 * time.Second)
}

func requeueWithCustomInterval(interval time.Duration) ctrl.Result {
	return ctrl.Result{RequeueAfter: interval}
}

//+kubebuilder:rbac:groups=lca.openshift.io,resources=imagebasedupgrades,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lca.openshift.io,resources=imagebasedupgrades/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lca.openshift.io,resources=imagebasedupgrades/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ImageBasedUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (nextReconcile ctrl.Result, err error) {
	if r.Mux != nil {
		r.Mux.Lock()
		defer r.Mux.Unlock()
	}

	r.Log.Info("Start reconciling IBU", "name", req.NamespacedName)
	defer func() {
		if nextReconcile.RequeueAfter > 0 {
			r.Log.Info("Finish reconciling IBU", "name", req.NamespacedName, "requeueAfter", nextReconcile.RequeueAfter.Seconds())
		} else {
			r.Log.Info("Finish reconciling IBU", "name", req.NamespacedName, "requeueRightAway", nextReconcile.Requeue)
		}
	}()

	nextReconcile = doNotRequeue()

	if req.Name != utils.IBUName {
		r.Log.Error(fmt.Errorf("ibu CR must be named, %s", utils.IBUName), "")
		return
	}

	// Use a non-cached query to Get the IBU CR, to ensure we aren't running against a stale cached CR
	ibu := &ibuv1.ImageBasedUpgrade{}
	err = common.RetryOnRetriable(common.RetryBackoffTwoMinutes, func() error {
		return r.NoncachedClient.Get(ctx, req.NamespacedName, ibu) //nolint:wrapcheck
	})
	if err != nil {
		if errors.IsNotFound(err) {
			err = lcautils.InitIBU(ctx, r.Client, &r.Log)
			return
		}
		r.Log.Error(err, "Failed to get ImageBasedUpgrade")

		// This is likely a case where the API is down, so requeue and try again shortly
		nextReconcile = requeueWithShortInterval()
		return
	}

	r.Log.Info("Loaded IBU", "name", req.NamespacedName, "version", ibu.GetResourceVersion(), "desired stage", ibu.Spec.Stage)

	var isAfterPivot bool
	isAfterPivot, err = r.RPMOstreeClient.IsStaterootBooted(common.GetDesiredStaterootName(ibu))
	if err != nil {
		return
	}

	// Make sure "next stage" list is initialized
	ibu.Status.ValidNextStages = getValidNextStageList(ibu, isAfterPivot)

	if isTransitionRequested(ibu) {
		if validateStageTransition(ibu, isAfterPivot) {
			// The transition has occurred, regenerate the list of valid next stages
			ibu.Status.ValidNextStages = getValidNextStageList(ibu, isAfterPivot)
			// Update status
			if err = utils.UpdateIBUStatus(ctx, r.Client, ibu); err != nil {
				r.Log.Error(err, "failed to update IBU CR status")
				return
			}
		}
	}

	inProgressStage := utils.GetInProgressStage(ibu)
	if inProgressStage != "" {
		nextReconcile, err = r.handleStage(ctx, ibu, inProgressStage)
		if err != nil {
			ibu.Status.ValidNextStages = getValidNextStageList(ibu, isAfterPivot)
			// Note: the status update error must have a different var name other than err
			if updateErr := utils.UpdateIBUStatus(ctx, r.Client, ibu); updateErr != nil {
				r.Log.Error(err, "failed to update IBU CR status")
			}
			return
		}
	}

	ibu.Status.ValidNextStages = getValidNextStageList(ibu, isAfterPivot)

	// Update status
	if err = utils.UpdateIBUStatus(ctx, r.Client, ibu); err != nil {
		r.Log.Error(err, "failed to update IBU CR status")
	}

	return
}

func getValidNextStageList(ibu *ibuv1.ImageBasedUpgrade, isAfterPivot bool) []ibuv1.ImageBasedUpgradeStage {
	inProgressStage := utils.GetInProgressStage(ibu)
	if inProgressStage == ibuv1.Stages.Idle || inProgressStage == ibuv1.Stages.Rollback || utils.IsStageFailed(ibu, ibuv1.Stages.Rollback) {
		// no valid transition if aborting/abort failed/finalizing/finalize failed/rollback in progress/rollback failed
		return []ibuv1.ImageBasedUpgradeStage{}
	}

	if inProgressStage == ibuv1.Stages.Prep || utils.IsStageFailed(ibu, ibuv1.Stages.Prep) {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle}
	}

	if inProgressStage == ibuv1.Stages.Upgrade || utils.IsStageFailed(ibu, ibuv1.Stages.Upgrade) {
		if isAfterPivot {
			return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Rollback}
		} else {
			return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle}
		}
	}

	// no in progress stage, check completed stages in reverse order
	if utils.IsStageCompleted(ibu, ibuv1.Stages.Rollback) {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle}
	}
	if utils.IsStageCompleted(ibu, ibuv1.Stages.Upgrade) {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle, ibuv1.Stages.Rollback}
	}
	if utils.IsStageCompleted(ibu, ibuv1.Stages.Prep) {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle, ibuv1.Stages.Upgrade}
	}
	if utils.IsStageCompleted(ibu, ibuv1.Stages.Idle) {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Prep}
	}

	// initial IBU creation - no idle condition
	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
	if idleCondition == nil {
		return []ibuv1.ImageBasedUpgradeStage{ibuv1.Stages.Idle}
	}

	return []ibuv1.ImageBasedUpgradeStage{}
}

func isTransitionRequested(ibu *ibuv1.ImageBasedUpgrade) bool {
	desiredStage := ibu.Spec.Stage
	if desiredStage == ibuv1.Stages.Idle {
		return !(utils.IsStageCompleted(ibu, desiredStage) || utils.IsStageInProgress(ibu, desiredStage))
	}
	return !(utils.IsStageCompletedOrFailed(ibu, desiredStage) || utils.IsStageInProgress(ibu, desiredStage))
}

func (r *ImageBasedUpgradeReconciler) handleStage(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade, stage ibuv1.ImageBasedUpgradeStage) (nextReconcile ctrl.Result, err error) {
	// append current logger with the stage name
	tempOrigLogger := r.Log
	r.Log = r.Log.WithName(string(stage))
	defer func() {
		r.Log = tempOrigLogger
	}()

	switch stage {
	case ibuv1.Stages.Idle:
		nextReconcile, err = r.handleAbortOrFinalize(ctx, ibu)
	case ibuv1.Stages.Prep:
		nextReconcile, err = r.handlePrep(ctx, ibu)
	case ibuv1.Stages.Upgrade:
		nextReconcile, err = r.handleUpgrade(ctx, ibu)
	case ibuv1.Stages.Rollback:
		nextReconcile, err = r.handleRollback(ctx, ibu)
	}
	return
}

func (r *ImageBasedUpgradeReconciler) handleAbortOrFinalize(ctx context.Context, ibu *ibuv1.ImageBasedUpgrade) (nextReconcile ctrl.Result, err error) {
	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
	if idleCondition == nil || idleCondition.Status == metav1.ConditionTrue {
		return
	}
	switch idleCondition.Reason {
	case string(utils.ConditionReasons.Aborting):
		nextReconcile, err = r.handleAbort(ctx, ibu)
	case string(utils.ConditionReasons.AbortFailed):
		nextReconcile, err = r.handleAbortFailure(ctx, ibu)
	case string(utils.ConditionReasons.Finalizing):
		nextReconcile, err = r.handleFinalize(ctx, ibu)
	case string(utils.ConditionReasons.FinalizeFailed):
		nextReconcile, err = r.handleFinalizeFailure(ctx, ibu)
	}
	return
}

// isFinalizeOrAbort determines whether the idle transition is for finalize or abort
// It returns the condition reason
func isFinalizeOrAbort(ibu *ibuv1.ImageBasedUpgrade, isAfterPivot bool) utils.ConditionReason {
	for _, conditionType := range utils.FinalConditionTypes {
		condition := meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
		if condition != nil && condition.Status == metav1.ConditionTrue {
			// finalize if upgrade or rollback completed
			return utils.ConditionReasons.Finalizing
		}
	}

	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
	rollbackInProgressCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.RollbackInProgress))
	if idleCondition != nil && idleCondition.Status == metav1.ConditionFalse && !isAfterPivot &&
		(rollbackInProgressCondition == nil || rollbackInProgressCondition.Reason == string(utils.ConditionReasons.InvalidTransition)) {
		// abort if in prep or upgrade before pivot
		return utils.ConditionReasons.Aborting
	}
	return ""
}

// validateStageTransition verifies if the stage transition is permitted and sets the appropriate status condition.
// Any invalid stage transition should be rejected by the xvalidation rule. If an issue arises where the xvalidation
// rule fails to block the transition, this serves as the secondary safeguard.
func validateStageTransition(ibu *ibuv1.ImageBasedUpgrade, isAfterPivot bool) bool {
	validStages := getValidNextStageList(ibu, isAfterPivot)

	// Check if the stage is in the valid next stage list
	ok := lo.Contains(validStages, ibu.Spec.Stage)
	if ok {
		// The transition is valid, firstly clear any invalid transition conditions
		// if they exist and then set InProgress condition
		utils.ClearInvalidTransitionStatusConditions(ibu)

		switch ibu.Spec.Stage {
		case ibuv1.Stages.Prep:
			utils.SetIdleStatusInProgress(ibu, utils.ConditionReasons.InProgress, utils.InProgress)
			utils.SetPrepStatusInProgress(ibu, utils.InProgress)
		case ibuv1.Stages.Upgrade:
			utils.SetUpgradeStatusInProgress(ibu, utils.InProgress)
		case ibuv1.Stages.Rollback:
			utils.SetUpgradeStatusRollbackRequested(ibu)
			utils.SetRollbackStatusInProgress(ibu, utils.InProgress)
		case ibuv1.Stages.Idle:
			idleReason := isFinalizeOrAbort(ibu, isAfterPivot)
			switch idleReason {
			case utils.ConditionReasons.Finalizing:
				utils.SetIdleStatusInProgress(ibu, utils.ConditionReasons.Finalizing, utils.Finalizing)
			case utils.ConditionReasons.Aborting:
				utils.SetIdleStatusInProgress(ibu, utils.ConditionReasons.Aborting, utils.Aborting)
			default:
				// Special case for setting idle when the initial IBU just got created
				idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(ibu.Spec.Stage))
				if idleCondition == nil {
					utils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
				}
			}
		}
	} else {
		// The transition is invalid, set invalid transition condition
		switch ibu.Spec.Stage {
		case ibuv1.Stages.Prep,
			ibuv1.Stages.Upgrade:
			utils.SetStatusInvalidTransition(ibu, "Previous stage not succeeded")
		case ibuv1.Stages.Rollback:
			utils.SetStatusInvalidTransition(ibu, "Upgrade not started or already finalized")
		case ibuv1.Stages.Idle:
			msg := "Abort or finalize not allowed"
			if utils.IsStageFailed(ibu, ibuv1.Stages.Rollback) {
				msg = "Transition to Idle not allowed - Rollback failed"
			} else if isAfterPivot {
				if utils.IsStageInProgress(ibu, ibuv1.Stages.Rollback) {
					msg = "Transition to Idle not allowed - Rollback is in progress"
				} else {
					msg = "Transition to Idle not allowed - Rollback first"
				}
			}
			utils.SetStatusInvalidTransition(ibu, msg)
		}

		return false
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageBasedUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("ImageBasedUpgrade")

	//nolint:wrapcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&ibuv1.ImageBasedUpgrade{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Generation is only updated on spec changes (also on deletion),
				// not metadata or status
				oldGeneration := e.ObjectOld.GetGeneration()
				newGeneration := e.ObjectNew.GetGeneration()
				if oldGeneration != newGeneration {
					return true
				}

				// trigger reconcile upon adding or removing ManualCleanupAnnotation
				_, oldExist := e.ObjectOld.GetAnnotations()[utils.ManualCleanupAnnotation]
				_, newExist := e.ObjectNew.GetAnnotations()[utils.ManualCleanupAnnotation]
				if oldExist != newExist {
					return true
				}

				// trigger reconcile upon adding or updating TriggerReconcileAnnotation
				oldValue, oldExist := e.ObjectOld.GetAnnotations()[utils.TriggerReconcileAnnotation]
				newValue, newExist := e.ObjectNew.GetAnnotations()[utils.TriggerReconcileAnnotation]
				if !oldExist && newExist || (oldExist && newExist && oldValue != newValue) {
					return true
				}

				return false
			},
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc: func(de event.DeleteEvent) bool {
				if de.Object.GetName() == utils.IBUName {
					ibu := de.Object.(*ibuv1.ImageBasedUpgrade)
					filePath := common.PathOutsideChroot(utils.IBUFilePath)
					// Re-create initial IBU instead of save/restore when it's idle or rollback failed
					if utils.IsStageCompleted(ibu, ibuv1.Stages.Idle) || utils.IsStageFailed(ibu, ibuv1.Stages.Rollback) {
						if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
							err := os.Remove(filePath)
							if os.IsNotExist(err) {
								return nil
							}
							return err //nolint:wrapcheck
						}); err != nil {
							fmt.Printf("Failed to save deleted IBU: %v", err)
						}
					} else {
						if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
							return lcautils.MarshalToFile(de.Object, filePath) //nolint:wrapcheck
						}); err != nil {
							fmt.Printf("Failed to save deleted IBU: %v", err)
						}
					}
					return true
				}
				return false
			},
		})).
		Owns(&kbatch.Job{}). // note: job resource watched is restricted further using cache.Options during NewManager
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
