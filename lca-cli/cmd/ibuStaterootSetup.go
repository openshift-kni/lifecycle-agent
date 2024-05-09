package cmd

import (
	"context"
	"fmt"
	"os"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/precache"
	"github.com/openshift-kni/lifecycle-agent/internal/prep"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/spf13/cobra"
)

var (
	// ibuStaterootSetupCmd represents the ibuStaterootSetup command
	ibuStaterootSetupCmd = &cobra.Command{
		Use:     "ibuStaterootSetup",
		Aliases: []string{"ibu-stateroot-setup"},
		Short:   "Setup a new stateroot during IBU",
		Long:    `Setup stateroot during IBU. This is meant to be used as k8s job!`,
		Run: func(cmd *cobra.Command, args []string) {
			if err := ibuStaterootSetupRun(); err != nil {
				log.Error(err, "something went wrong!")
				os.Exit(1)
			}
		},
	}
)

func init() {
	rootCmd.AddCommand(ibuStaterootSetupCmd)
}

// ibuStaterootSetupRun main function for do stateroot setup
func ibuStaterootSetupRun() error {
	// k8s client
	c, err := getClient()
	if err != nil {
		return err
	}

	// internals
	hostCommandsExecutor := ops.NewChrootExecutor(log, true, common.Host)
	opsClient := ops.NewOps(log, hostCommandsExecutor)
	rpmOstreeClient := ostree.NewClient("lca-cli-stateroot-setup", hostCommandsExecutor)
	ostreeClient := ostreeclient.NewClient(hostCommandsExecutor, false)
	ctx := context.Background()
	loggerOpt := zap.Options{Development: true}
	// logger init...we have two loggers
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&loggerOpt)))
	logger := ctrl.Log.WithName("prep-stateroot-job")

	logger.Info("Fetching the latest IBU cr")
	ibu := &lcav1alpha1.ImageBasedUpgrade{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: common.LcaNamespace, Name: utils.IBUName}, ibu); err != nil {
		return fmt.Errorf("failed get IBU cr: %w", err)
	}

	logger.Info("Pulling seed image")
	if err := controllers.GetSeedImage(c, ctx, ibu, logger, hostCommandsExecutor); err != nil {
		return fmt.Errorf("failed to pull seed image: %w", err)
	}

	logger.Info("Setting up stateroot")
	if err := prep.SetupStateroot(logger, opsClient, ostreeClient, rpmOstreeClient, ibu.Spec.SeedImageRef.Image, ibu.Spec.SeedImageRef.Version, precache.ImageListFile, false); err != nil {
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

// getClient returns a client for this job
func getClient() (client.Client, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get k8s config: %w", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	return c, nil
}
