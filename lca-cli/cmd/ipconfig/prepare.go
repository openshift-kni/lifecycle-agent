package ipconfig

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	lcacli "github.com/openshift-kni/lifecycle-agent/lca-cli"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ipconfig"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/spf13/cobra"
)

var (
	ipPrepareScheme = runtime.NewScheme()

	newStaterootName   string
	installInitMonitor bool
)

const (
	newStaterootNameFlag   = "new-stateroot-name"
	installInitMonitorFlag = "install-init-monitor"
	prepareCmd             = "prepare"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(ipPrepareScheme))
	utilruntime.Must(mcfgv1.AddToScheme(ipPrepareScheme))

	ipConfigPrepareCmd.Flags().StringVar(&newStaterootName, newStaterootNameFlag, "", "New stateroot name")
	ipConfigPrepareCmd.MarkFlagRequired(newStaterootNameFlag)
	ipConfigPrepareCmd.Flags().BoolVar(
		&installInitMonitor,
		installInitMonitorFlag,
		false,
		"Install init monitor service in the new stateroot",
	)
}

var ipConfigPrepareCmd = &cobra.Command{
	Use:   prepareCmd,
	Short: "Prepare a new stateroot for IP configuration change and reboot to it",
	Run: func(cmd *cobra.Command, args []string) {
		if err := runIPConfigPrepare(); err != nil {
			pkgLog.Fatalf("Error executing ip-config prepare: %v", err)
		}
	},
}

// runIPConfigPrepare orchestrates the ip-config "prepare" flow:
// - sets up executors and logs flags
// - gathers OSTree state for old/new stateroots
// - writes progress status, runs preparation, optionally installs an init monitor
// - finalizes status and reboots into the new stateroot
func runIPConfigPrepare() error {
	var hostCommandsExecutor ops.Execute
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(pkgLog, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(pkgLog, true)
	}
	opsInterface := ops.NewOps(pkgLog, hostCommandsExecutor)

	logPrepareFlags()

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", common.PathOutsideChroot(common.KubeconfigFile))
	if err != nil {
		return fmt.Errorf("failed to create k8s config: %w", err)
	}
	client, err := runtimeClient.New(k8sConfig, runtimeClient.Options{Scheme: ipPrepareScheme})
	if err != nil {
		return fmt.Errorf("failed to create runtime client: %w", err)
	}

	rpmClient := rpmOstree.NewClient("lca-cli-ip-config-prepare", hostCommandsExecutor)
	ostreeClient := intOstree.NewClient(hostCommandsExecutor, false)
	rbClient := reboot.NewIPCRebootClient(
		&logr.Logger{},
		hostCommandsExecutor,
		rpmClient,
		ostreeClient,
		opsInterface,
	)

	ostreeData, err := getOstreeData(newStaterootName, rpmClient, ostreeClient, pkgLog)
	if err != nil {
		return fmt.Errorf("failed to get ostree data: %w", err)
	}

	preparer := ipconfig.NewPrepareHandler(
		pkgLog, opsInterface, ostreeData, ostreeClient, rpmClient, rbClient, client,
	)
	if err := common.WriteIPConfigStatus(common.IPConfigPrepareStatusFile, common.IPConfigRunStatus{
		Phase:     common.IPConfigPhaseRunning,
		Message:   "ip-config prepare started",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("failed to write initial prepare status: %w", err)
	}

	ctx := context.Background()
	if err := preparer.Run(ctx); err != nil {
		internalErr := common.FinalizeIPConfigStatus(
			common.IPConfigPrepareStatusFile,
			common.IPConfigPhaseFailed,
			fmt.Sprintf("ip-config prepare failed: %v", err),
		)
		if internalErr != nil {
			return fmt.Errorf("failed to finalize IP config prepare status: %w", internalErr)
		}
		return err
	}

	if installInitMonitor {
		if err := installMonitorInitializationServiceInNewStateroot(
			ostreeClient, opsInterface, ostreeData.NewStateroot.Name, pkgLog,
		); err != nil {
			return fmt.Errorf("failed to install monitor initialization service: %w", err)
		}
	}

	staterootPath := common.GetStaterootPath(ostreeData.NewStateroot.Name)
	statusFilePath := filepath.Join(staterootPath, common.IPConfigPrepareStatusFile)
	if err := common.FinalizeIPConfigStatus(
		statusFilePath,
		common.IPConfigPhaseSucceeded,
		"ip-config prepare completed successfully",
	); err != nil {
		return fmt.Errorf("failed to mark prepare as successful: %w", err)
	}

	if err := common.FinalizeIPConfigStatus(
		common.IPConfigPrepareStatusFile,
		common.IPConfigPhaseSucceeded,
		"ip-config prepare completed successfully",
	); err != nil {
		return fmt.Errorf("failed to mark prepare as successful: %w", err)
	}

	if err := rbClient.RebootToNewStateRoot("ip-config prepare"); err != nil {
		return fmt.Errorf("failed to reboot to new stateroot: %w", err)
	}

	return nil
}

// logPrepareFlags prints the flags used by the ip-config prepare command.
func logPrepareFlags() {
	pkgLog.Infof("ip-config prepare flags:")
	pkgLog.Infof("  --%s=%q", newStaterootNameFlag, newStaterootName)
	pkgLog.Infof("  --%s=%t", installInitMonitorFlag, installInitMonitor)
}

// installMonitorInitializationServiceInNewStateroot installs and enables the IPC init monitor service
// within the new stateroot deployment.
func installMonitorInitializationServiceInNewStateroot(
	ostree intOstree.IClient,
	ops ops.Ops,
	newStaterootName string,
	logger *logrus.Logger,
) error {
	deploymentDir, err := ostree.GetDeploymentDir(newStaterootName)
	if err != nil {
		return fmt.Errorf("failed to get deployment dir for %s: %w", newStaterootName, err)
	}

	initMonitorServiceFile, err := fs.ReadFile(lcacli.InstallationConfigurationServices, common.IPCInitMonitorService)
	if err != nil {
		return fmt.Errorf("failed to read init monitor service file: %w", err)
	}

	destinationFilePath := filepath.Join(deploymentDir, "etc/systemd/system", common.IPCInitMonitorService)
	logger.Infof("Creating service %s", common.IPCInitMonitorService)

	if err := os.MkdirAll(path.Dir(destinationFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create destination directory for %s: %w", common.IPCInitMonitorService, err)
	}
	if err := os.WriteFile(destinationFilePath, initMonitorServiceFile, 0o644); err != nil {
		return fmt.Errorf("failed to write init monitor service file: %w", err)
	}

	logger.Infof("Enabling service %s", common.IPCInitMonitorService)
	if _, err := ops.SystemctlAction(
		"enable",
		"--root",
		deploymentDir,
		common.IPCInitMonitorService,
	); err != nil {
		return fmt.Errorf("failed enabling service %s: %w", common.IPCInitMonitorService, err)
	}

	staterootPath := common.GetStaterootPath(newStaterootName)
	initMonitorModeFile := filepath.Join(staterootPath, common.InitMonitorModeFile)
	if err := os.WriteFile(initMonitorModeFile, []byte("ipconfig"), 0o644); err != nil {
		return fmt.Errorf("failed to write init monitor mode file: %w", err)
	}

	return nil
}

// getOstreeData returns information about the current (old) and target (new)
// stateroots, including names, paths, deployment names, and deployment dirs.
func getOstreeData(
	newStaterootName string,
	rpmOstree rpmOstree.IClient,
	ostree intOstree.IClient,
	logger *logrus.Logger,
) (*ipconfig.OstreeData, error) {
	common.OstreeDeployPathPrefix = "/sysroot"
	ostreeData := &ipconfig.OstreeData{}

	currentStaterootData, err := getCurrentStaterootData(rpmOstree, ostree)
	if err != nil {
		return nil, fmt.Errorf("failed to get current stateroot data: %w", err)
	}
	ostreeData.OldStateroot = currentStaterootData

	newStaterootData, err := getNewStaterootData(newStaterootName, ostree, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to get new stateroot data: %w", err)
	}
	ostreeData.NewStateroot = newStaterootData

	return ostreeData, nil
}

// getCurrentStaterootData queries the system for the current stateroot details.
func getCurrentStaterootData(rpmOstree rpmOstree.IClient, ostree intOstree.IClient) (*ipconfig.StaterootData, error) {
	staterootData := &ipconfig.StaterootData{}

	currentStaterootName, err := rpmOstree.GetCurrentStaterootName()
	if err != nil {
		return nil, fmt.Errorf("failed to get current stateroot name: %w", err)
	}
	staterootData.Name = currentStaterootName

	staterootData.Path = common.GetStaterootPath(currentStaterootName)

	oldDeploymentName, err := ostree.GetDeployment(currentStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment for %s: %w", currentStaterootName, err)
	}
	staterootData.DeploymentName = oldDeploymentName

	oldDeploymentDir, err := ostree.GetDeploymentDir(currentStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment dir for %s: %w", currentStaterootName, err)
	}
	staterootData.DeploymentDir = oldDeploymentDir

	return staterootData, nil
}

// getNewStaterootData resolves the metadata for the target new stateroot.
func getNewStaterootData(
	newStaterootName string,
	ostree intOstree.IClient,
	logger *logrus.Logger,
) (*ipconfig.StaterootData, error) {
	staterootData := &ipconfig.StaterootData{
		Name: newStaterootName,
		Path: common.GetStaterootPath(newStaterootName),
	}

	newDeploymentName, err := ostree.GetDeployment(newStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment name for %s: %w", newStaterootName, err)
	}
	if newDeploymentName != "" {
		staterootData.DeploymentName = newDeploymentName
		logger.Infof("Found deployment for new stateroot before creation %s: %s", newStaterootName, newDeploymentName)
	} else {
		return staterootData, nil
	}

	newDeploymentDir, err := ostree.GetDeploymentDir(newStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to get deployment dir for %s: %w", newStaterootName, err)
	}
	staterootData.DeploymentDir = newDeploymentDir

	return staterootData, nil
}
