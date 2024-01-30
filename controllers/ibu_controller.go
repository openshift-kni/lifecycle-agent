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
	"sync"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"

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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImageBasedUpgradeReconciler reconciles a ImageBasedUpgrade object
type ImageBasedUpgradeReconciler struct {
	client.Client
	UpgradeHandler  UpgradeHandler
	Log             logr.Logger
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	Precache        *precache.PHandler
	BackupRestore   backuprestore.BackuperRestorer
	RPMOstreeClient rpmostreeclient.IClient
	Executor        ops.Execute
	OstreeClient    ostreeclient.IClient
	Ops             ops.Ops
	PrepTask        *Task
	Mux             *sync.Mutex
}

// Task contains objects for executing a group of serial tasks asynchronously
type Task struct {
	Active   bool
	Success  bool
	Cancel   context.CancelFunc
	Progress string
	done     chan struct{}
}

// Reset Re-initialize the Task variables to initial values
func (c *Task) Reset() {
	c.Active = false
	c.Success = false
	c.Cancel = nil
	c.Progress = ""
	select {
	case _, open := <-c.done:
		if open {
			close(c.done)
		}
	}
}

func doNotRequeue() ctrl.Result {
	return ctrl.Result{}
}

func requeueWithError(err error) (ctrl.Result, error) {
	// can not be fixed by user during reconcile
	return ctrl.Result{}, err
}

//nolint:unused
func requeueImmediately() ctrl.Result {
	return ctrl.Result{Requeue: true}
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

//nolint:unused
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ImageBasedUpgrade object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
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

	ibu := &lcav1alpha1.ImageBasedUpgrade{}
	err = r.Get(ctx, req.NamespacedName, ibu)
	if err != nil {
		if errors.IsNotFound(err) {
			err = lcautils.InitIBU(ctx, r.Client, &r.Log)
			return
		}
		r.Log.Error(err, "Failed to get ImageBasedUpgrade")
		return
	}

	r.Log.Info("Loaded IBU", "name", req.NamespacedName, "version", ibu.GetResourceVersion(), "desired stage", ibu.Spec.Stage)

	currentInProgressStage := utils.GetCurrentInProgressStage(ibu)
	if currentInProgressStage != "" {
		nextReconcile, err = r.handleStage(ctx, ibu, currentInProgressStage)
		if err != nil {
			_ = utils.UpdateIBUStatus(ctx, r.Client, ibu)
			return
		}
	}

	if isTransitionRequested(ibu) {
		var isAfterPivot bool
		isAfterPivot, err = r.RPMOstreeClient.IsStaterootBooted(common.GetDesiredStaterootName(ibu))
		if err != nil {
			return
		}

		// Update in progress condition to true and idle condition to false when transitioning to non idle stage
		if validateStageTransition(ibu, isAfterPivot) {
			// Validate the IBU spec if the transition is to prep stage
			if ibu.Spec.Stage == lcav1alpha1.Stages.Prep {
				var isValid bool
				isValid, err = r.validateIBUSpec(ctx, ibu)
				if err != nil {
					return
				}
				if !isValid {
					err = utils.UpdateIBUStatus(ctx, r.Client, ibu)
					return
				}
			}
			nextReconcile = requeueImmediately()
		}
	}

	// Update status
	err = utils.UpdateIBUStatus(ctx, r.Client, ibu)
	return
}

func isTransitionRequested(ibu *lcav1alpha1.ImageBasedUpgrade) bool {
	desiredStage := ibu.Spec.Stage
	if desiredStage == lcav1alpha1.Stages.Idle {
		return !(utils.IsStageCompleted(ibu, desiredStage) || utils.IsStageInProgress(ibu, desiredStage))
	}
	return !(utils.IsStageCompletedOrFailed(ibu, desiredStage) || utils.IsStageInProgress(ibu, desiredStage))
}

func (r *ImageBasedUpgradeReconciler) handleStage(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade, stage lcav1alpha1.ImageBasedUpgradeStage) (nextReconcile ctrl.Result, err error) {
	switch stage {
	case lcav1alpha1.Stages.Idle:
		nextReconcile, err = r.handleAbortOrFinalize(ctx, ibu)
	case lcav1alpha1.Stages.Prep:
		nextReconcile, err = r.handlePrep(ctx, ibu)
	case lcav1alpha1.Stages.Upgrade:
		nextReconcile, err = r.handleUpgrade(ctx, ibu)
	case lcav1alpha1.Stages.Rollback:
		nextReconcile, err = r.handleRollback(ctx, ibu)
	}
	return
}

func (r *ImageBasedUpgradeReconciler) handleAbortOrFinalize(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (nextReconcile ctrl.Result, err error) {
	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
	if idleCondition != nil && idleCondition.Status == metav1.ConditionFalse {
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
		if nextReconcile.Requeue == false {
			utils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
		}
	}
	return
}

func isRollbackAllowed(ibu *lcav1alpha1.ImageBasedUpgrade, isAfterPivot bool) bool {
	if !isAfterPivot {
		rollbackInProgressCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.RollbackInProgress))
		if rollbackInProgressCondition != nil && rollbackInProgressCondition.Status == metav1.ConditionTrue {
			return true
		}
		return false
	}
	upgradeInProgressCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.UpgradeInProgress))
	if upgradeInProgressCondition != nil {
		// allowed if upgrade stage is in progress or has failed/completed
		return true
	}
	return false
}

// isFinalizeAllowed returns true if upgrade completed or rollback completed
func isFinalizeAllowed(ibu *lcav1alpha1.ImageBasedUpgrade) bool {
	for _, conditionType := range utils.FinalConditionTypes {
		condition := meta.FindStatusCondition(ibu.Status.Conditions, string(conditionType))
		if condition != nil && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func isAbortAllowed(ibu *lcav1alpha1.ImageBasedUpgrade, isAfterPivot bool) bool {
	idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
	rollbackInProgressCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.RollbackInProgress))
	if idleCondition != nil && idleCondition.Status == metav1.ConditionFalse && !isAfterPivot && rollbackInProgressCondition == nil {
		// allowed if in prep or upgrade before pivot
		return true
	}
	return false
}

// TODO unit test this function once the logic is stablized
func validateStageTransition(ibu *lcav1alpha1.ImageBasedUpgrade, isAfterPivot bool) bool {
	switch ibu.Spec.Stage {
	case lcav1alpha1.Stages.Rollback:
		if !isRollbackAllowed(ibu, isAfterPivot) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.ConditionTypes.RollbackInProgress,
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Upgrade not started or already finalized",
				ibu.Generation,
			)
			return false
		}
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.UpgradeInProgress,
			utils.ConditionReasons.Failed,
			metav1.ConditionFalse,
			"Rollback requested",
			ibu.Generation,
		)
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.ConditionTypes.UpgradeCompleted,
			utils.ConditionReasons.Failed,
			metav1.ConditionFalse,
			"Rollback requested",
			ibu.Generation,
		)

	case lcav1alpha1.Stages.Idle:
		if isFinalizeAllowed(ibu) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Finalizing,
				metav1.ConditionFalse,
				"Finalizing",
				ibu.Generation,
			)
		} else if isAbortAllowed(ibu, isAfterPivot) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.Aborting,
				metav1.ConditionFalse,
				"Aborting",
				ibu.Generation,
			)
		} else {
			rollbackCompletedCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.RollbackCompleted))
			idleCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.Idle))
			// Special cases for setting idle when the IBU just got created or after manual cleanup for rollback failure is done
			if (rollbackCompletedCondition != nil && rollbackCompletedCondition.Status == metav1.ConditionFalse) ||
				idleCondition == nil {
				utils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
			} else {
				utils.SetStatusCondition(&ibu.Status.Conditions,
					utils.ConditionTypes.Idle,
					utils.ConditionReasons.InvalidTransition,
					metav1.ConditionFalse,
					"Abort or finalize not allowed",
					ibu.Generation,
				)
				return false
			}
		}
	default:
		if !utils.IsStageCompleted(ibu, utils.GetPreviousStage(ibu.Spec.Stage)) {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.GetInProgressConditionType(ibu.Spec.Stage),
				utils.ConditionReasons.InvalidTransition,
				metav1.ConditionFalse,
				"Previous stage not succeeded",
				ibu.Generation,
			)
			return false
		}
		// Set idle to false when transitioning to prep
		if ibu.Spec.Stage == lcav1alpha1.Stages.Prep {
			utils.SetStatusCondition(&ibu.Status.Conditions,
				utils.ConditionTypes.Idle,
				utils.ConditionReasons.InProgress,
				metav1.ConditionFalse,
				"In progress",
				ibu.Generation)
		}
	}
	if ibu.Spec.Stage != lcav1alpha1.Stages.Idle {
		utils.SetStatusCondition(&ibu.Status.Conditions,
			utils.GetInProgressConditionType(ibu.Spec.Stage),
			utils.ConditionReasons.InProgress,
			metav1.ConditionTrue,
			"In progress",
			ibu.Generation,
		)
	}
	return true
}

// validateIBUSpec validates the IBU CR, returns true if the spec is valid, false otherwise
func (r *ImageBasedUpgradeReconciler) validateIBUSpec(ctx context.Context, ibu *lcav1alpha1.ImageBasedUpgrade) (bool, error) {
	r.Log.Info("Validating IBU spec")

	// If OADP configmap is provided, validate the configmap and check if OADP operator is available
	if len(ibu.Spec.OADPContent) != 0 {
		err := r.BackupRestore.ValidateOadpConfigmap(ctx, ibu.Spec.OADPContent)
		if err != nil {
			if backuprestore.IsBRFailedValidationError(err) {
				utils.SetPrepStatusFailed(ibu, err.Error())
				return false, nil
			}
			return false, err
		}

		err = r.BackupRestore.CheckOadpOperatorAvailability(ctx)
		if err != nil {
			if backuprestore.IsBRFailedValidationError(err) {
				utils.SetPrepStatusFailed(ibu, err.Error())
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageBasedUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("ImageBasedUpgrade")

	return ctrl.NewControllerManagedBy(mgr).
		For(&lcav1alpha1.ImageBasedUpgrade{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Generation is only updated on spec changes (also on deletion),
				// not metadata or status
				oldGeneration := e.ObjectOld.GetGeneration()
				newGeneration := e.ObjectNew.GetGeneration()
				// spec update only for IBU
				return oldGeneration != newGeneration
			},
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc: func(de event.DeleteEvent) bool {
				if de.Object.GetName() == utils.IBUName {
					filePath := common.PathOutsideChroot(utils.IBUFilePath)
					if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
						return lcautils.MarshalToFile(de.Object, filePath)
					}); err != nil {
						fmt.Printf("Failed to save deleted IBU: %v", err)
					}
					return true
				}
				return false
			},
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
