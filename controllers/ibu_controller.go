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
	PrepTask        *Task
	Mux             *sync.Mutex
}

// Task contains objects for executing a group of serial tasks asynchronously
type Task struct {
	Active             bool
	Success            bool
	Cancel             context.CancelFunc
	Progress           string
	AdditionalComplete string // additional completion msg
	done               chan struct{}
}

// Reset Re-initialize the Task variables to initial values
func (c *Task) Reset() {
	c.Active = false
	c.Success = false
	c.Cancel = nil
	c.Progress = ""
	c.AdditionalComplete = ""
	select {
	case _, open := <-c.done:
		if open {
			close(c.done)
		}
	case <-time.After(30 * time.Second): // max wait timeout in case c.done is still empty
	}
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
	ibu := &lcav1alpha1.ImageBasedUpgrade{}
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
		// Update in progress condition to true and idle condition to false when transitioning to non idle stage
		if validateStageTransition(ibu, isAfterPivot) {
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
			if updateErr := utils.UpdateIBUStatus(ctx, r.Client, ibu); updateErr != nil {
				r.Log.Error(updateErr, "failed to update IBU CR status")
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

func getValidNextStageList(ibu *lcav1alpha1.ImageBasedUpgrade, isAfterPivot bool) []lcav1alpha1.ImageBasedUpgradeStage {
	inProgressStage := utils.GetInProgressStage(ibu)
	if inProgressStage == lcav1alpha1.Stages.Idle || inProgressStage == lcav1alpha1.Stages.Rollback || utils.IsStageFailed(ibu, lcav1alpha1.Stages.Rollback) {
		// no valid transition if abort/finalize/rollback in progress or failed
		return []lcav1alpha1.ImageBasedUpgradeStage{}
	}

	if inProgressStage == lcav1alpha1.Stages.Prep || utils.IsStageFailed(ibu, lcav1alpha1.Stages.Prep) {
		return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Idle}
	}

	if inProgressStage == lcav1alpha1.Stages.Upgrade || utils.IsStageFailed(ibu, lcav1alpha1.Stages.Upgrade) {
		if isAfterPivot {
			return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Rollback}
		} else {
			return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Idle}
		}
	}

	// no in progress stage, check completed stages in reverse order
	if utils.IsStageCompleted(ibu, lcav1alpha1.Stages.Rollback) {
		return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Idle}
	}
	if utils.IsStageCompleted(ibu, lcav1alpha1.Stages.Upgrade) {
		return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Idle, lcav1alpha1.Stages.Rollback}
	}
	if utils.IsStageCompleted(ibu, lcav1alpha1.Stages.Prep) {
		return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Idle, lcav1alpha1.Stages.Upgrade}
	}
	if utils.IsStageCompleted(ibu, lcav1alpha1.Stages.Idle) {
		return []lcav1alpha1.ImageBasedUpgradeStage{lcav1alpha1.Stages.Prep}
	}
	return []lcav1alpha1.ImageBasedUpgradeStage{}
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
	if idleCondition != nil && idleCondition.Status == metav1.ConditionFalse && !isAfterPivot &&
		(rollbackInProgressCondition == nil || rollbackInProgressCondition.Reason == string(utils.ConditionReasons.InvalidTransition)) {
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
			if idleCondition == nil {
				utils.ResetStatusConditions(&ibu.Status.Conditions, ibu.Generation)
			} else {
				msg := "Abort or finalize not allowed"
				if rollbackCompletedCondition != nil && rollbackCompletedCondition.Status == metav1.ConditionFalse {
					msg = "Transition to Idle not allowed - Rollback failed"
				} else if isAfterPivot {
					rollbackInProgressCondition := meta.FindStatusCondition(ibu.Status.Conditions, string(utils.ConditionTypes.RollbackInProgress))
					if rollbackInProgressCondition != nil && rollbackInProgressCondition.Status == metav1.ConditionTrue {
						msg = "Transition to Idle not allowed - Rollback is in progress"
					} else {
						msg = "Transition to Idle not allowed - Rollback first"
					}
				}
				utils.SetStatusCondition(&ibu.Status.Conditions,
					utils.ConditionTypes.Idle,
					utils.ConditionReasons.InvalidTransition,
					metav1.ConditionFalse,
					msg,
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

// SetupWithManager sets up the controller with the Manager.
func (r *ImageBasedUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("ImageBasedUpgrade")

	//nolint:wrapcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&lcav1alpha1.ImageBasedUpgrade{}, builder.WithPredicates(predicate.Funcs{
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

				return false
			},
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc: func(de event.DeleteEvent) bool {
				if de.Object.GetName() == utils.IBUName {
					ibu := de.Object.(*lcav1alpha1.ImageBasedUpgrade)
					filePath := common.PathOutsideChroot(utils.IBUFilePath)
					// Re-create initial IBU instead of save/restore when it's idle or rollback failed
					if utils.IsStageCompleted(ibu, lcav1alpha1.Stages.Idle) || utils.IsStageFailed(ibu, lcav1alpha1.Stages.Rollback) {
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
