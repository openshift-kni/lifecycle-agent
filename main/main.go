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
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	ocpV1 "github.com/openshift/api/config/v1"
	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/config/leaderelection"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	seedgenv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/seedgenerator/v1alpha1"
	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	"github.com/openshift-kni/lifecycle-agent/internal/clusterconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/extramanifest"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	//+kubebuilder:scaffold:imports
)

var logger = &logrus.Logger{
	Out:   os.Stdout,
	Level: logrus.InfoLevel,
}

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
	logger.SetFormatter(&logrus.TextFormatter{
		DisableColors:   true,
		TimestampFormat: "2006-01-02 15:04:05",
		FullTimestamp:   true,
	})
	log := ctrl.Log.WithName("controllers").WithName("ImageBasedUpgrade")

	executor := ops.NewChrootExecutor(newLogger, true, utils.Host)
	rpmOstreeClient := rpmostreeclient.NewClient("ibu-controller", executor)

	if err := initIBU(context.TODO(), mgr.GetClient(), &setupLog); err != nil {
		setupLog.Error(err, "unable to initialize IBU CR")
		os.Exit(1)
	}

	if err = (&controllers.ImageBasedUpgradeReconciler{
		Client:          mgr.GetClient(),
		Log:             log,
		Scheme:          mgr.GetScheme(),
		ClusterConfig:   &clusterconfig.UpgradeClusterConfigGather{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Log: log},
		NetworkConfig:   &clusterconfig.UpgradeNetworkConfigGather{Log: log},
		Precache:        &precache.PHandler{Client: mgr.GetClient(), Log: log.WithName("Precache")},
		BackupRestore:   &backuprestore.BRHandler{Client: mgr.GetClient(), Log: log.WithName("BackupRestore")},
		ExtraManifest:   &extramanifest.EMHandler{Client: mgr.GetClient(), Log: log.WithName("ExtraManifest")},
		RPMOstreeClient: rpmOstreeClient,
		Executor:        executor,
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

func initIBU(ctx context.Context, client client.Client, log *logr.Logger) error {
	ibu := &lcav1alpha1.ImageBasedUpgrade{}
	filePath := common.PathOutsideChroot(utils.IBUFilePath)
	if err := lcautils.ReadYamlOrJSONFile(filePath, ibu); err != nil {
		if os.IsNotExist(err) {
			ibu = &lcav1alpha1.ImageBasedUpgrade{
				ObjectMeta: metav1.ObjectMeta{
					Name: utils.IBUName,
				},
				Spec: lcav1alpha1.ImageBasedUpgradeSpec{
					Stage: lcav1alpha1.Stages.Idle,
				},
			}
			if err := client.Create(ctx, ibu); err != nil {
				if errors.IsAlreadyExists(err) {
					return nil
				}
				return err
			}
			log.Info("Initial IBU created")
		}
		return err
	}

	log.Info("Saved IBU CR found, restoring ...")
	if err := client.Delete(ctx, ibu); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}

	// Save status as the ibu structure gets over-written by the create call
	// with the result which has no status
	status := ibu.Status
	if err := client.Create(ctx, ibu); err != nil {
		return err
	}

	// Put the saved status into the newly create ibu with the right resource
	// version which is required for the update call to work
	ibu.Status = status
	if err := client.Status().Update(ctx, ibu); err != nil {
		return err
	}

	if err := os.Remove(filePath); err != nil {
		return err
	}
	log.Info("Restore successful and saved IBU CR removed")
	return nil
}
