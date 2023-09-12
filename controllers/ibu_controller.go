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
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
)

// ImageBasedUpgradeReconciler reconciles a ImageBasedUpgrade object
type ImageBasedUpgradeReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func doNotRequeue() ctrl.Result {
	return ctrl.Result{}
}

func requeueImmediately() ctrl.Result {
	return ctrl.Result{Requeue: true}
}

func requeueWithShortInterval() ctrl.Result {
	return requeueWithCustomInterval(30 * time.Second)
}

func requeueWithMediumInterval() ctrl.Result {
	return requeueWithCustomInterval(1 * time.Minute)
}

func requeueWithLongInterval() ctrl.Result {
	return requeueWithCustomInterval(5 * time.Minute)
}

func requeueWithCustomInterval(interval time.Duration) ctrl.Result {
	return ctrl.Result{RequeueAfter: interval}
}

//+kubebuilder:rbac:groups=ran.openshift.io,resources=imagebasedupgrades,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ran.openshift.io,resources=imagebasedupgrades/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ran.openshift.io,resources=imagebasedupgrades/finalizers,verbs=update
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
	r.Log.Info("Start reconciling IBU", "name", req.NamespacedName)
	defer func() {
		if nextReconcile.RequeueAfter > 0 {
			r.Log.Info("Finish reconciling IBU", "name", req.NamespacedName, "requeueAfter", nextReconcile.RequeueAfter.Seconds())
		} else {
			r.Log.Info("Finish reconciling IBU", "name", req.NamespacedName, "requeueRightAway", nextReconcile.Requeue)
		}
	}()

	nextReconcile = doNotRequeue()

	ibu := &ranv1alpha1.ImageBasedUpgrade{}
	err = r.Get(ctx, req.NamespacedName, ibu)
	if err != nil {
		if errors.IsNotFound(err) {
			err = nil
			return
		}
		r.Log.Error(err, "Failed to get ImageBasedUpgrade")
		return
	}

	r.Log.Info("Loaded IBU", "name", req.NamespacedName, "version", ibu.GetResourceVersion())
	var reconcileTime int
	reconcileTime, err = r.handleCguFinalizer(ctx, ibu)
	if err != nil {
		return
	}
	if reconcileTime == utils.ReconcileNow {
		nextReconcile = requeueImmediately()
		return
	} else if reconcileTime == utils.StopReconciling {
		return
	}
	// Update status
	err = r.updateStatus(ctx, ibu)
	return
}

func (r *ImageBasedUpgradeReconciler) handleCguFinalizer(
	ctx context.Context, ibu *ranv1alpha1.ImageBasedUpgrade) (int, error) {

	isCguMarkedToBeDeleted := ibu.GetDeletionTimestamp() != nil
	if isCguMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(ibu, utils.CleanupFinalizer) {
			// Run finalization logic for cguFinalizer. If the finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			// err := utils.FinalMultiCloudObjectCleanup(ctx, r.Client, ibu)
			// if err != nil {
			// 	return utils.StopReconciling, err
			// }
		}
		return utils.StopReconciling, nil
	}

	// Add finalizer for this CR.
	if !controllerutil.ContainsFinalizer(ibu, utils.CleanupFinalizer) {
		controllerutil.AddFinalizer(ibu, utils.CleanupFinalizer)
		err := r.Update(ctx, ibu)
		if err != nil {
			return utils.StopReconciling, err
		}
		return utils.ReconcileNow, nil
	}

	return utils.DontReconcile, nil
}

func (r *ImageBasedUpgradeReconciler) updateStatus(ctx context.Context, ibu *ranv1alpha1.ImageBasedUpgrade) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := r.Status().Update(ctx, ibu)
		return err
	})

	if err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageBasedUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("ImageBasedUpgrade")

	return ctrl.NewControllerManagedBy(mgr).
		For(&ranv1alpha1.ImageBasedUpgrade{}, builder.WithPredicates(predicate.Funcs{
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
			DeleteFunc:  func(de event.DeleteEvent) bool { return false },
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
