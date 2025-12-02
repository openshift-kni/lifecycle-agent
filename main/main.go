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
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"sync"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/openshift-kni/lifecycle-agent/internal/clusterconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/imagemgmt"
	"github.com/openshift-kni/lifecycle-agent/internal/networkpolicies"
	kbatchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	seedgenv1 "github.com/openshift-kni/lifecycle-agent/api/seedgenerator/v1"
	ocpV1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	lsov1 "github.com/openshift/local-storage-operator/api/v1"
	lvmv1alpha1 "github.com/openshift/lvm-operator/api/v1alpha1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"github.com/go-logr/logr"
	sriovv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/openshift/library-go/pkg/config/leaderelection"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	policyv1 "open-cluster-management.io/config-policy-controller/api/v1"
	policiesv1 "open-cluster-management.io/governance-policy-propagator/api/v1"
	//+kubebuilder:scaffold:imports
)

// +kubebuilder:rbac:groups="security.openshift.io",resources=securitycontextconstraints,resourceNames=privileged,verbs=use
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs="*"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	bakExt = ".bak"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ibuv1.AddToScheme(scheme))
	utilruntime.Must(seedgenv1.AddToScheme(scheme))
	utilruntime.Must(ipcv1.AddToScheme(scheme))
	utilruntime.Must(ocpV1.AddToScheme(scheme))
	utilruntime.Must(mcv1.AddToScheme(scheme))
	utilruntime.Must(velerov1.AddToScheme(scheme))
	utilruntime.Must(operatorsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(operatorv1alpha1.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(policyv1.AddToScheme(scheme))
	utilruntime.Must(policiesv1.AddToScheme(scheme))
	utilruntime.Must(sriovv1.AddToScheme(scheme))
	utilruntime.Must(lvmv1alpha1.AddToScheme(scheme))
	utilruntime.Must(lsov1.AddToScheme(scheme))

	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertDir string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&metricsCertDir, "metrics-tls-cert-dir", "",
		"The directory containing the tls.crt and tls.key.")
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
	log := ctrl.Log.WithName("controllers").WithName("ImageBasedUpgrade")

	scheme.AddKnownTypes(ocpV1.GroupVersion,
		&ocpV1.ClusterVersion{},
		&ocpV1.Ingress{},
		&ocpV1.ImageDigestMirrorSet{},
		&ocpV1.Infrastructure{},
	)

	le := leaderelection.LeaderElectionSNOConfig(ocpV1.LeaderElection{})

	mux := &sync.Mutex{}

	tlsOpts := []func(*tls.Config){
		func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		},
	}

	cfg := ctrl.GetConfigOrDie()
	cfg.Wrap(lcautils.RetryMiddleware(log.WithName("ibu-manager-client"))) // allow all client calls to be retriable

	// OLM installs the default-deny, operator API egress NetworkPolicies
	// Operator installs policies for the jobs, for metrics
	np := networkpolicies.Policy{
		Namespace:  common.LcaNamespace,
		MetricAddr: metricsAddr,
	}
	msg, err := np.InstallPolicies(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create network policy:")
		os.Exit(1)
	}
	setupLog.Info(msg)

	if msg := networkpolicies.Check(cfg, common.LcaNamespace); msg != "" {
		setupLog.Info("NetworkPolicies", "msg", msg)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                enableLeaderElection,
		LeaderElectionID:              "lca.openshift.io",
		LeaseDuration:                 &le.LeaseDuration.Duration,
		RenewDeadline:                 &le.RenewDeadline.Duration,
		RetryPeriod:                   &le.RetryPeriod.Duration,
		LeaderElectionReleaseOnCancel: true,
		Metrics: server.Options{
			BindAddress:    metricsAddr,
			SecureServing:  metricsCertDir != "",
			CertDir:        metricsCertDir,
			TLSOpts:        tlsOpts,
			FilterProvider: filters.WithAuthenticationAndAuthorization,
		},
		Cache: cache.Options{ // https://github.com/kubernetes-sigs/controller-runtime/blob/main/designs/cache_options.md
			ByObject: map[client.Object]cache.ByObject{
				&kbatchv1.Job{}: { // cache all job resources in LCA ns
					Namespaces: map[string]cache.Config{
						common.LcaNamespace: {}},
				},
			},
		},
	})

	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// We want to remove logr.Logger first step to move to logrus
	// in the future we will have only one of them
	newLogger := logrus.New()

	if err := os.MkdirAll(common.PathOutsideChroot(common.LCAConfigDir), 0o700); err != nil {
		setupLog.Error(err, fmt.Sprintf("unable to create config dir: %s", common.LCAConfigDir))
		os.Exit(1)
	}

	chrootExecutor := ops.NewChrootExecutor(newLogger, true, common.Host)
	chrootOp := ops.NewOps(newLogger, chrootExecutor)
	nsenterExecutor := ops.NewNsenterExecutor(newLogger, true)
	nsenterOp := ops.NewOps(newLogger, nsenterExecutor)
	rpmOstreeClient := rpmostreeclient.NewClient("ibu-controller", chrootExecutor)
	ostreeClient := ostreeclient.NewClient(chrootExecutor, false)
	ibuRebootClient := reboot.NewIBURebootClient(&log, chrootExecutor, rpmOstreeClient, ostreeClient, chrootOp)
	ipcRebootClient := reboot.NewIPCRebootClient(&log, chrootExecutor, rpmOstreeClient, ostreeClient, chrootOp)
	imageMgmtClient := imagemgmt.NewImageMgmtClient(&log, chrootExecutor, common.PathOutsideChroot(common.ContainerStoragePath))

	if err := lcautils.InitIBU(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize IBU CR")
		os.Exit(1)
	}

	if err := initSeedGen(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize SeedGenerator CR")
		os.Exit(1)
	}

	if err := lcautils.InitIPConfig(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize IPConfig CR")
		os.Exit(1)
	}

	dynamicClient, err := lcautils.CreateDynamicClient(common.PathOutsideChroot(common.KubeconfigFile), true, log.WithName("ibu-dynamic-client"))
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}

	backupRestore := &backuprestore.BRHandler{
		Client: mgr.GetClient(), DynamicClient: dynamicClient, Log: log.WithName("BackupRestore")}
	extraManifest := &extramanifest.EMHandler{
		Client: mgr.GetClient(), DynamicClient: dynamicClient, Log: log.WithName("ExtraManifest")}

	containerStorageMountpointTarget, err := chrootOp.GetContainerStorageTarget()
	if err != nil {
		setupLog.Error(err, "unable to get container storage mountpoint target")
		os.Exit(1)
	}

	// a simple in-cluster client-go based client, useful for getting pod logs
	// as runtime-controller client currently doesn't support pod sub resources
	// https://github.com/kubernetes-sigs/controller-runtime/issues/452#issuecomment-792266582
	config, err := rest.InClusterConfig()
	if err != nil {
		setupLog.Error(err, "Failed to get InClusterConfig")
		os.Exit(1)
	}
	config.Wrap(lcautils.RetryMiddleware(log.WithName("ibu-clientset"))) // allow all client calls to be retriable
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		setupLog.Error(err, "Failed to get clientset")
		os.Exit(1)
	}

	if err = (&controllers.ImageBasedUpgradeReconciler{
		Client:          mgr.GetClient(),
		NoncachedClient: mgr.GetAPIReader(),
		Log:             log,
		Scheme:          mgr.GetScheme(),
		Precache:        &precache.PHandler{Client: mgr.GetClient(), Log: log.WithName("Precache"), Scheme: mgr.GetScheme()},
		RPMOstreeClient: rpmOstreeClient,
		Executor:        chrootExecutor,
		OstreeClient:    ostreeClient,
		Ops:             chrootOp,
		RebootClient:    ibuRebootClient,
		BackupRestore:   backupRestore,
		ExtraManifest:   extraManifest,
		UpgradeHandler: &controllers.UpgHandler{
			Client:          mgr.GetClient(),
			NoncachedClient: mgr.GetAPIReader(),
			Log:             log.WithName("UpgradeHandler"),
			BackupRestore:   backupRestore,
			ExtraManifest:   extraManifest,
			ClusterConfig:   &clusterconfig.UpgradeClusterConfigGather{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: log},
			Executor:        chrootExecutor,
			Ops:             chrootOp,
			Recorder:        mgr.GetEventRecorderFor("ImageBasedUpgrade"),
			RPMOstreeClient: rpmOstreeClient,
			OstreeClient:    ostreeClient,
			RebootClient:    ibuRebootClient,
		},
		Mux:       mux,
		Clientset: clientset,

		ImageMgmtClient: imageMgmtClient,

		// Cluster data retrieved once during init
		ContainerStorageMountpointTarget: containerStorageMountpointTarget,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ImageBasedUpgrade")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err = (&controllers.IPConfigReconciler{
		Client:          mgr.GetClient(),
		NoncachedClient: mgr.GetAPIReader(),
		Scheme:          mgr.GetScheme(),
		ChrootOps:       chrootOp,
		NsenterOps:      nsenterOp,
		RebootClient:    ipcRebootClient,
		OstreeClient:    ostreeClient,
		RPMOstreeClient: rpmOstreeClient,
		Mux:             mux,
		IdleHandler: controllers.NewIPConfigIdleStageHandler(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			chrootOp,
			ostreeClient,
			rpmOstreeClient,
		),
		ConfigHandler: controllers.NewIPConfigConfigStageHandler(
			mgr.GetClient(),
			mgr.GetAPIReader(),
			rpmOstreeClient,
			chrootOp,
			controllers.NewIPConfigConfigPhasesHandler(
				mgr.GetClient(),
				mgr.GetAPIReader(),
				rpmOstreeClient,
				ostreeClient,
				chrootOp,
				ipcRebootClient,
			),
		),
		RollbackHandler: controllers.NewIPConfigRollbackStageHandler(
			mgr.GetClient(),
			chrootOp,
			controllers.NewIPConfigRollbackPhasesHandler(
				mgr.GetClient(),
				mgr.GetAPIReader(),
				rpmOstreeClient,
				chrootOp,
			),
		),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "IPConfig")
		os.Exit(1)
	}

	seedgenLog := ctrl.Log.WithName("controllers").WithName("SeedGenerator")
	if err = (&controllers.SeedGeneratorReconciler{
		Client:          mgr.GetClient(),
		NoncachedClient: mgr.GetAPIReader(),
		Log:             seedgenLog,
		Scheme:          mgr.GetScheme(),
		BackupRestore:   backupRestore,
		Executor:        chrootExecutor,
		Mux:             mux,

		// Cluster data retrieved once during init
		ContainerStorageMountpointTarget: containerStorageMountpointTarget,
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
// so that it isn't included in the seed image. When the lca-cli is launched in a
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
		return fmt.Errorf("unable to read secret from file for init seedGen in %s: %w", secretFilePath, err)
	}

	// Strip the ResourceVersion, otherwise the restore fails
	secret.SetResourceVersion("")

	seedgen := &seedgenv1.SeedGenerator{}
	if err := lcautils.ReadYamlOrJSONFile(seedgenFilePath, seedgen); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unable to read seedGen CR from file for init seedGen in %s: %w", seedgenFilePath, err)
	}

	// Strip the ResourceVersion, otherwise the restore fails
	seedgen.SetResourceVersion("")

	// Restore Secret CR
	log.Info("Saved SeedGenerator Secret CR found, restoring ...")
	if err := c.Delete(ctx, secret); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed delete SeedGenerator Secret: %w", err)
		}
	}

	if err := c.Create(ctx, secret); err != nil {
		return fmt.Errorf("failed to create SeedGenerator Secret: %w", err)
	}

	// Restore SeedGenerator CR

	log.Info("Saved SeedGenerator CR found, restoring ...")
	if err := c.Delete(ctx, seedgen); err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete SeedGenerator: %w", err)
		}
	}

	// Save status as the seedgen structure gets over-written by the create call
	// with the result which has no status
	status := seedgen.Status
	if err := c.Create(ctx, seedgen); err != nil {
		return fmt.Errorf("failed to create seedgen: %w", err)
	}

	// Put the saved status into the newly create seedgen with the right resource
	// version which is required for the update call to work
	seedgen.Status = status
	if err := c.Status().Update(ctx, seedgen); err != nil {
		return fmt.Errorf("failed to update seedgen status: %w", err)
	}

	// Rename files for debugging in case of error
	os.Remove(seedgenFilePath + bakExt)
	if err := os.Rename(seedgenFilePath, seedgenFilePath+bakExt); err != nil {
		return fmt.Errorf("failed to rename %s: %w", seedgenFilePath, err)
	}

	os.Remove(secretFilePath + bakExt)
	if err := os.Rename(secretFilePath, secretFilePath+bakExt); err != nil {
		return fmt.Errorf("failed to rename %s: %w", secretFilePath, err)
	}

	// Restore original pull-secret after seed creation.
	// During seed creation, the pull-secret was removed; it needs to be restored back
	// to allow the cluster operators to fully recover in the seed cluster.
	log.Info("Restore original pull-secret after seed creation")
	dockerConfigJSON, err := os.ReadFile(common.PathOutsideChroot(utils.StoredPullSecret))
	if err != nil {
		return fmt.Errorf("failed to read original pull-secret from %s file: %w", utils.StoredPullSecret, err)
	}

	if _, err := lcautils.UpdatePullSecretFromDockerConfig(ctx, c, dockerConfigJSON); err != nil {
		return fmt.Errorf("failed to restore original pull-secret in seed cluster: %w", err)
	}

	log.Info("Restore successful and saved SeedGenerator CR removed")
	return nil
}
