package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/openshift-kni/lifecycle-agent/controllers"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/spf13/cobra"
)

// ibuStaterootSetupCmd represents the ibuStaterootSetup command
var ibuStaterootSetupCmd = &cobra.Command{
	Use:     "ibuStaterootSetup",
	Aliases: []string{"ibu-stateroot-setup"},
	Short:   "Setup a new stateroot during IBU",
	Long:    `Setup stateroot during IBU. This is meant to be used as k8s job!`,
	Run: func(cmd *cobra.Command, args []string) {
		if err := ibuStaterootSetupRun(); err != nil {
			log.Error(err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(ibuStaterootSetupCmd)
}

// ibuStaterootSetupRun main function to do stateroot setup
func ibuStaterootSetupRun() error {
	var (
		hostCommandsExecutor = ops.NewChrootExecutor(log, true, common.Host)
		opsClient            = ops.NewOps(log, hostCommandsExecutor)
		rpmOstreeClient      = ostree.NewClient("lca-cli-stateroot-setup", hostCommandsExecutor)
		ostreeClient         = ostreeclient.NewClient(hostCommandsExecutor, false)
		ctx, cancelCtx       = context.WithCancel(context.Background())
	)

	// additional logger setup
	loggerOpt := zap.Options{Development: true}
	logger := zap.New(zap.UseFlagOptions(&loggerOpt)).WithName("prep-stateroot-job")

	// defer cleanup
	defer cancelCtx()

	logger.Info("Starting a new client")
	c, err := getClient(logger)
	if err != nil {
		return fmt.Errorf("failed to create client for stateroot setup job: %w", err)
	}

	logger.Info("Fetching the latest IBU cr")
	ibu := &ibuv1.ImageBasedUpgrade{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: common.LcaNamespace, Name: utils.IBUName}, ibu); err != nil {
		return fmt.Errorf("failed get IBU cr: %w", err)
	}

	logger.Info("Starting signal handler")
	initStaterootSetupSigHandler(logger, opsClient, ibu.Spec.SeedImageRef.Image)

	logger.Info("Pulling seed image")
	if err := controllers.GetSeedImage(c, ctx, ibu, logger, hostCommandsExecutor); err != nil {
		return fmt.Errorf("failed to pull seed image: %w", err)
	}

	logger.Info("Setting up stateroot")
	if err := prep.SetupStateroot(logger, opsClient, ostreeClient, rpmOstreeClient, ibu.Spec.SeedImageRef.Image, ibu.Spec.SeedImageRef.Version, false); err != nil {
		return fmt.Errorf("failed to complete stateroot setup: %w", err)
	}

	logger.Info("Writing IBU AutoRollbackConfig file")
	if err := reboot.WriteIBUAutoRollbackConfigFile(logger, ibu); err != nil {
		return fmt.Errorf("failed to write auto-rollback config: %w", err)
	}

	logger.Info("Backing up kubeconfig crypto")
	if err := lcautils.BackupKubeconfigCrypto(ctx, c, common.GetStaterootCertsDir(ibu)); err != nil {
		return fmt.Errorf("failed to backup cerificaties: %w", err)
	}

	logger.Info("Setup stateroot job done successfully")
	return nil
}

// initStaterootSetupSigHandler handling signals here
func initStaterootSetupSigHandler(logger logr.Logger, opsClient ops.Ops, seedImage string) {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGTERM) // to handle any additional signals add a new param here and also handle it specifically in the switch below

	go func() {
		logger.Info("Listening for signal")
		s := <-signalChannel
		switch s {
		case syscall.SIGTERM:
			logger.Error(fmt.Errorf("unexpected SIGTERM received to stop stateroot setup"), "")

			// we consider all the steps after image pull as critical and must be allowed to proceed until SIGKILL arrives. The time between SIGTERM and SIGKILL is controlled with StaterootSetupTerminationGracePeriodSeconds
			if seedImageExists, _ := opsClient.ImageExists(seedImage); seedImageExists {
				sigkillArrivesIn := time.Duration(prep.StaterootSetupTerminationGracePeriodSeconds) * time.Second
				logger.Error(fmt.Errorf("seed image exists. prepare to shut down in at most %s", sigkillArrivesIn.String()), "")
				time.Sleep(sigkillArrivesIn) // likely anything beyond this point won't be executed since job should be done before this wakes up
			}

			logger.Error(fmt.Errorf("proceeding to shutdown stateroot setup job"), "")
			os.Exit(1)
		default:
			// nolint: staticcheck
			logger.Error(fmt.Errorf("unknown signal: %s", s.String()), "Unknown signal") // this is not expected to be hit
		}
	}()
}

// getClient returns a client for this job
func getClient(logger logr.Logger) (client.Client, error) {
	cfg := config.GetConfigOrDie()
	cfg.Wrap(lcautils.RetryMiddleware(logger)) // allow all client calls to be retriable

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return c, nil
}
