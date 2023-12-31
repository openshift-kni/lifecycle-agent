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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	cro "github.com/RHsyseng/cluster-relocation-operator/api/v1beta1"
	seedgenv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/seedgenerator/v1alpha1"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	configv1 "github.com/openshift/api/config/v1"
	ocpV1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"github.com/go-logr/logr"
	"github.com/openshift/library-go/pkg/config/leaderelection"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"

	"github.com/openshift-kni/lifecycle-agent/controllers"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/clusterconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	//+kubebuilder:scaffold:imports
)

// +kubebuilder:rbac:groups="security.openshift.io",resources=securitycontextconstraints,resourceNames=privileged,verbs=use

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(lcav1alpha1.AddToScheme(scheme))
	utilruntime.Must(seedgenv1alpha1.AddToScheme(scheme))
	utilruntime.Must(ocpV1.AddToScheme(scheme))
	utilruntime.Must(mcv1.AddToScheme(scheme))
	utilruntime.Must(velerov1.AddToScheme(scheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	scheme.AddKnownTypes(ocpV1.GroupVersion, &ocpV1.ClusterVersion{}, &ocpV1.Ingress{},
		&ocpV1.ImageDigestMirrorSet{})
	scheme.AddKnownTypes(cro.GroupVersion, &cro.ClusterRelocation{})

	le := leaderelection.LeaderElectionSNOConfig(configv1.LeaderElection{})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "lca.openshift.io",
		LeaseDuration:                 &le.LeaseDuration.Duration,
		RenewDeadline:                 &le.RenewDeadline.Duration,
		RetryPeriod:                   &le.RetryPeriod.Duration,
		LeaderElectionReleaseOnCancel: true,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// We want to remove logr.Logger first step to move to logrus
	// in the future we will have only one of them
	newLogger := logrus.New()
	log := ctrl.Log.WithName("controllers").WithName("ImageBasedUpgrade")

	executor := ops.NewChrootExecutor(newLogger, true, common.Host)
	op := ops.NewOps(newLogger, executor)
	rpmOstreeClient := rpmostreeclient.NewClient("ibu-controller", executor)
	ostreeClient := ostreeclient.NewClient(executor, false)

	if err := lcautils.InitIBU(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize IBU CR")
		os.Exit(1)
	}

	if err := initSeedGen(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize SeedGenerator CR")
		os.Exit(1)
	}

	if err = (&controllers.ImageBasedUpgradeReconciler{
		Client:          mgr.GetClient(),
		Log:             log,
		Scheme:          mgr.GetScheme(),
		ClusterConfig:   &clusterconfig.UpgradeClusterConfigGather{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: log},
		Precache:        &precache.PHandler{Client: mgr.GetClient(), Log: log.WithName("Precache")},
		BackupRestore:   &backuprestore.BRHandler{Client: mgr.GetClient(), Log: log.WithName("BackupRestore")},
		ExtraManifest:   &extramanifest.EMHandler{Client: mgr.GetClient(), Log: log.WithName("ExtraManifest")},
		RPMOstreeClient: rpmOstreeClient,
		Executor:        executor,
		OstreeClient:    ostreeClient,
		Ops:             op,
		PrepTask:        &controllers.Task{Active: false, Success: false, Cancel: nil, Progress: ""},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageBasedUpgrade")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	seedgenLog := ctrl.Log.WithName("controllers").WithName("SeedGenerator")
	if err = (&controllers.SeedGeneratorReconciler{
		Client:   mgr.GetClient(),
		Log:      seedgenLog,
		Scheme:   mgr.GetScheme(),
		Executor: executor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SeedGenerator")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// Seed generator orchestration is done in two stages.
// In the first stage, the SeedGen CR is saved to filesystem and deleted from etcd,
// so that it isn't included in the seed image. When the imager is launched in a
// separate container to generate the image, it shuts down the pods. Once finished,
// it restarts kubelet, which restarts the pods.
// When LCA recovers, it is able to run the second stage by restoring the SeedGen CR,
// then running the reconciler to check and report the status.
//
// The initSeedGen function runs before the SeedGen controller is launched to restore
// the CR, if it exists on the filesystem, in order to complete the orchestration with
// the second stage.
//
// TODO: Determine if errors from this function can be handled better. If the saved files
// are incomplete or corrupted, for example, maybe we should create a generic SeedGen CR
// with a Failed state.
func initSeedGen(ctx context.Context, c client.Client, log *logr.Logger) error {
	storedCRFound := false
	storedSecretCRFound := false

	seedgenFilePath := common.PathOutsideChroot(utils.SeedGenStoredCR)
	secretFilePath := common.PathOutsideChroot(utils.SeedGenStoredSecretCR)

	if _, err := os.Stat(seedgenFilePath); err == nil {
		storedCRFound = true
	}

	if _, err := os.Stat(secretFilePath); err == nil {
		storedSecretCRFound = true
	}

	if !storedCRFound && !storedSecretCRFound {
		// Nothing to do
		return nil
	} else if storedCRFound != storedSecretCRFound {
		missing := utils.SeedGenStoredCR
		if storedCRFound {
			missing = utils.SeedGenStoredSecretCR
		}
		return fmt.Errorf("unable to recover SeedGenerator CR: Missing stored file %s", missing)
	}

	// Read CRs from file
	secret := &corev1.Secret{}
	if err := lcautils.ReadYamlOrJSONFile(secretFilePath, secret); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Strip the ResourceVersion, otherwise the restore fails
	secret.SetResourceVersion("")

	seedgen := &seedgenv1alpha1.SeedGenerator{}
	if err := lcautils.ReadYamlOrJSONFile(seedgenFilePath, seedgen); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Strip the ResourceVersion, otherwise the restore fails
	seedgen.SetResourceVersion("")

	// Restore Secret CR
	log.Info("Saved SeedGenerator Secret CR found, restoring ...")
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return client.IgnoreNotFound(c.Delete(ctx, secret))
	}); err != nil {
		return err
	}

	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return c.Create(ctx, secret)
	}); err != nil {
		return err
	}

	// Restore SeedGenerator CR

	log.Info("Saved SeedGenerator CR found, restoring ...")
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return client.IgnoreNotFound(c.Delete(ctx, seedgen))
	}); err != nil {
		return err
	}

	// Save status as the ibu structure gets over-written by the create call
	// with the result which has no status
	status := seedgen.Status
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return c.Create(ctx, seedgen)
	}); err != nil {
		return err
	}

	// Put the saved status into the newly create seedgen with the right resource
	// version which is required for the update call to work
	seedgen.Status = status
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return c.Status().Update(ctx, seedgen)
	}); err != nil {
		return err
	}

	// Rename files for debugging in case of error
	os.Remove(seedgenFilePath + ".bak")
	if err := os.Rename(seedgenFilePath, seedgenFilePath+".bak"); err != nil {
		return err
	}

	os.Remove(secretFilePath + ".bak")
	if err := os.Rename(secretFilePath, secretFilePath+".bak"); err != nil {
		return err
	}

	log.Info("Restore successful and saved SeedGenerator CR removed")
	return nil
}
