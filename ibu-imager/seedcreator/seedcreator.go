package seedcreator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/openshift/api/config/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	cp "github.com/otiai10/copy"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	runtime "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// containerFileContent is the Dockerfile content for the IBU seed image
const containerFileContent = `
FROM scratch
COPY . /
`

// SeedCreator TODO: move params to Options
type SeedCreator struct {
	client               runtime.Client
	log                  *logrus.Logger
	ops                  ops.Ops
	ostreeClient         *ostree.Client
	backupDir            string
	kubeconfig           string
	containerRegistry    string
	authFile             string
	recertContainerImage string
	recertSkipValidation bool
	manifestClient       *clusterinfo.InfoClient
}

// NewSeedCreator is a constructor function for SeedCreator
func NewSeedCreator(client runtime.Client, log *logrus.Logger, ops ops.Ops, ostreeClient *ostree.Client, backupDir,
	kubeconfig, containerRegistry, authFile, recertContainerImage string, recertSkipValidation bool) *SeedCreator {

	return &SeedCreator{
		client:               client,
		log:                  log,
		ops:                  ops,
		ostreeClient:         ostreeClient,
		backupDir:            backupDir,
		kubeconfig:           kubeconfig,
		containerRegistry:    containerRegistry,
		authFile:             authFile,
		recertContainerImage: recertContainerImage,
		recertSkipValidation: recertSkipValidation,
		manifestClient:       clusterinfo.NewClusterInfoClient(client),
	}
}

// CreateSeedImage comprises the Imager workflow for creating a single OCI seed image
func (s *SeedCreator) CreateSeedImage() error {
	s.log.Info("Creating seed image")
	ctx := context.TODO()

	if err := s.copyConfigurationFiles(); err != nil {
		return fmt.Errorf("failed to add configuration files: %w", err)
	}

	// create backup dir
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return err
	}

	s.log.Info("Copy ibu-imager binary")
	err := cp.Copy("/usr/local/bin/ibu-imager", "/var/usrlocal/bin/ibu-imager", cp.Options{AddPermission: os.FileMode(0o777)})
	if err != nil {
		return err
	}

	if err := os.MkdirAll(common.BackupChecksDir, 0o700); err != nil {
		return err
	}

	if err := utils.RunOnce("create_container_list", common.BackupChecksDir, s.log, s.createContainerList, ctx); err != nil {
		return err
	}

	if err := utils.RunOnce("gather_cluster_info", common.BackupChecksDir, s.log, s.gatherClusterInfo, ctx); err != nil {
		return err
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping seed certificates backing up.")
	} else {
		s.log.Info("Backing up seed cluster certificates for recert tool")
		if err := utils.BackupCertificates(ctx, s.client, common.BackupCertsDir); err != nil {
			return err
		}
		s.log.Info("Seed cluster certificates backed up successfully for recert tool")
	}

	if err := utils.RunOnce("delete_node", common.BackupChecksDir, s.log, s.deleteNode, ctx); err != nil {
		return err
	}

	_ = utils.RunOnce("wait_for_ovn_to_go_down", common.BackupChecksDir, s.log, s.waitTillOvnKubeNodeIsDown, ctx)

	if err := s.stopServices(); err != nil {
		return err
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping recert validation.")
	} else {
		if err := s.ops.ForceExpireSeedCrypto(s.recertContainerImage, s.authFile); err != nil {
			return err
		}
	}

	if err := utils.RunOnce("backup_var", common.BackupChecksDir, s.log, s.backupVar); err != nil {
		return err
	}

	if err := utils.RunOnce("backup_etc", common.BackupChecksDir, s.log, s.backupEtc); err != nil {
		return err
	}

	if err := utils.RunOnce("backup_ostree", common.BackupChecksDir, s.log, s.backupOstree); err != nil {
		return err
	}

	if err := utils.RunOnce("backup_rpmostree", common.BackupChecksDir, s.log, s.backupRPMOstree); err != nil {
		return err
	}

	if err := utils.RunOnce("backup_mco_config", common.BackupChecksDir, s.log, s.backupMCOConfig); err != nil {
		return err
	}

	if err := s.createAndPushSeedImage(); err != nil {
		return err
	}

	return nil
}

func (s *SeedCreator) copyConfigurationFiles() error {
	// copy scripts
	err := s.copyConfigurationScripts()
	if err != nil {
		return err
	}

	return s.handleServices()
}

func (s *SeedCreator) copyConfigurationScripts() error {
	s.log.Infof("Copying installation_configuration_files/scripts to local/bin")
	return cp.Copy(filepath.Join(common.InstallationConfigurationFilesDir, "scripts"), "/var/usrlocal/bin", cp.Options{AddPermission: os.FileMode(0o777)})
}

func (s *SeedCreator) handleServices() error {
	dir := filepath.Join(common.InstallationConfigurationFilesDir, "services")
	return utils.HandleFilesWithCallback(dir, func(path string) error {
		serviceName := filepath.Base(path)

		s.log.Infof("Creating service %s", serviceName)
		if err := cp.Copy(path, filepath.Join("/etc/systemd/system/", serviceName)); err != nil {
			return err
		}

		s.log.Infof("Enabling service %s", serviceName)
		_, err := s.ops.SystemctlAction("enable", serviceName)
		return err
	})
}

func (s *SeedCreator) gatherClusterInfo(ctx context.Context) error {
	// TODO: remove after removing usage of clusterversion.json
	manifestPath := path.Join(s.backupDir, common.ClusterInfoFileName)
	if _, err := os.Stat(manifestPath); err == nil {
		s.log.Info("Manifest file was already created, skipping")
		return nil
	}

	clusterVersion := &v1.ClusterVersion{}
	if err := s.client.Get(ctx, types.NamespacedName{Name: "version"}, clusterVersion); err != nil {
		return err
	}
	clusterManifest, err := utils.CreateClusterInfo(ctx, s.client)
	if err != nil {
		return err
	}

	s.log.Info("Creating manifest.json")
	if err := utils.MarshalToFile(clusterManifest, path.Join(s.backupDir, common.ClusterInfoFileName)); err != nil {
		return err
	}

	return s.renderInstallationEnvFile(s.recertContainerImage,
		fmt.Sprintf("%s.%s", clusterManifest.ClusterName, clusterManifest.Domain))
}

// TODO: split function per operation
func (s *SeedCreator) createContainerList(ctx context.Context) error {
	s.log.Info("Saving list of running containers and catalogsources.")
	containersListFileName := s.backupDir + "/containers.list"

	// purge all unknown image if exists
	s.log.Info("Cleaning image list")
	// Don't ever add -a option as we don't want to delete unused images
	if _, err := s.ops.RunBashInHostNamespace("podman", "image", "prune", "-f"); err != nil {
		return err
	}

	// Execute 'crictl images -o json' command, parse the JSON output and extract image references using 'jq'
	s.log.Info("Save list of downloaded images")
	args := []string{"images", "-o", "json", "|", "jq", "-r",
		"'.images[] | if .repoTags | length > 0 then .repoTags[] else .repoDigests[] end'"}

	output, err := s.ops.RunBashInHostNamespace("crictl", args...)
	if err != nil {
		return err
	}
	images := strings.Split(output, "\n")
	catalogImages, err := s.getCatalogImages(ctx)
	if err != nil {
		return err
	}
	s.log.Infof("Removing list of catalog images from full image list, catalog images to remove %s", catalogImages)
	images = funk.FilterString(images, func(image string) bool {
		return !funk.ContainsString(catalogImages, image)
	})

	s.log.Infof("Adding recert %s image to image list", s.recertContainerImage)
	images = append(images, s.recertContainerImage)

	s.log.Infof("Creating %s file", containersListFileName)
	if err := os.WriteFile(containersListFileName, []byte(strings.Join(images, "\n")), 0o644); err != nil {
		return fmt.Errorf("failed to write container list file %s, err %w", containersListFileName, err)
	}

	s.log.Info("List of containers  saved successfully.")
	return nil
}

func (s *SeedCreator) stopServices() error {
	s.log.Info("Stop kubelet service")
	_, err := s.ops.SystemctlAction("stop", "kubelet.service")
	if err != nil {
		return err
	}

	s.log.Info("Disabling kubelet service")
	_, err = s.ops.SystemctlAction("disable", "kubelet.service")
	if err != nil {
		return err
	}

	s.log.Info("Stopping containers and CRI-O runtime.")
	crioSystemdStatus, err := s.ops.SystemctlAction("is-active", "crio")
	var exitErr *exec.ExitError
	// If ExitCode is 3, the command succeeded and told us that crio is down
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() != 3 {
		return err
	}
	s.log.Info("crio status is ", crioSystemdStatus)
	if crioSystemdStatus == "active" {
		// CRI-O is active, so stop running containers with retry
		_ = wait.PollUntilContextCancel(context.TODO(), time.Second, true, func(ctx context.Context) (done bool, err error) {
			s.log.Info("Stop running containers")
			args := []string{"ps", "-q", "|", "xargs", "--no-run-if-empty", "--max-args", "1", "--max-procs", "10", "crictl", "stop", "--timeout", "5"}
			_, err = s.ops.RunBashInHostNamespace("crictl", args...)
			if err != nil {
				return false, err
			}
			return true, nil
		})

		// Execute a D-Bus call to stop the CRI-O runtime
		s.log.Debug("Stopping CRI-O engine")
		_, err = s.ops.SystemctlAction("stop", "crio.service")
		if err != nil {
			return err
		}
		s.log.Info("Running containers and CRI-O engine stopped successfully.")
	} else {
		s.log.Info("Skipping running containers and CRI-O engine already stopped.")
	}

	return nil
}

func (s *SeedCreator) backupVar() error {
	varTarFile := path.Join(s.backupDir, "var.tgz")

	// Define the 'exclude' patterns
	excludePatterns := []string{
		"/var/tmp/*",
		"/var/lib/log/*",
		"/var/log/*",
		"/var/lib/containers/*",
		"/var/lib/kubelet/pods/*",
		"/var/lib/cni/bin/*",
		"/var/lib/ovn-ic/etc/ovnkube-node-certs/*",
	}

	// Build the tar command
	tarArgs := []string{"czf", varTarFile}
	for _, pattern := range excludePatterns {
		// We're handling the excluded patterns in bash, we need to single quote them to prevent expansion
		tarArgs = append(tarArgs, "--exclude", fmt.Sprintf("'%s'", pattern))
	}
	tarArgs = append(tarArgs, "--selinux", common.VarFolder)

	// Run the tar command
	_, err := s.ops.RunBashInHostNamespace("tar", tarArgs...)
	if err != nil {
		return err
	}

	s.log.Infof("Backup of %s created successfully.", common.VarFolder)
	return nil
}

func (s *SeedCreator) backupEtc() error {
	s.log.Info("Backing up /etc")

	// Execute 'ostree admin config-diff' command and backup etc.deletions
	args := []string{"admin", "config-diff", "|", "awk", `'$1 == "D" {print "/etc/" $2}'`, ">",
		path.Join(s.backupDir, "/etc.deletions")}
	_, err := s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return err
	}

	args = []string{"admin", "config-diff", "|", "grep", "-v", "'cni/multus'",
		"|", "awk", `'$1 != "D" {print "/etc/" $2}'`, "|", "tar", "czf",
		path.Join(s.backupDir + "/etc.tgz"), "--selinux", "-T", "-"}

	_, err = s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return err
	}
	s.log.Info("Backup of /etc created successfully.")

	return nil
}

func (s *SeedCreator) backupOstree() error {
	s.log.Info("Backing up ostree")
	ostreeTar := s.backupDir + "/ostree.tgz"

	// Execute 'tar' command and backup /etc
	_, err := s.ops.RunBashInHostNamespace(
		"tar", []string{"czf", ostreeTar, "--selinux", "-C", "/ostree/repo", "."}...)

	return err
}

func (s *SeedCreator) backupRPMOstree() error {
	rpmJSON := s.backupDir + "/rpm-ostree.json"
	_, err := s.ops.RunBashInHostNamespace(
		"rpm-ostree", append([]string{"status", "-v", "--json"}, ">", rpmJSON)...)
	s.log.Info("Backup of rpm-ostree.json created successfully.")
	return err
}

func (s *SeedCreator) backupMCOConfig() error {
	mcoJSON := s.backupDir + "/mco-currentconfig.json"
	_, err := s.ops.RunBashInHostNamespace(
		"cp", "/etc/machine-config-daemon/currentconfig", mcoJSON)
	s.log.Info("Backup of mco-currentconfig created successfully.")
	return err
}

// Building and pushing OCI image
func (s *SeedCreator) createAndPushSeedImage() error {
	s.log.Info("Build and push OCI image to ", s.containerRegistry)
	s.log.Debug(s.ostreeClient.RpmOstreeVersion()) // If verbose, also dump out current rpm-ostree version available

	// Get the current status of rpm-ostree daemon in the host
	statusRpmOstree, err := s.ostreeClient.QueryStatus()
	if err != nil {
		return fmt.Errorf("failed to query ostree status: %w", err)
	}
	if err := s.backupOstreeOrigin(statusRpmOstree); err != nil {
		return err
	}

	// Create a temporary file for the Dockerfile content
	tmpfile, err := os.CreateTemp("/var/tmp", "dockerfile-")
	if err != nil {
		return fmt.Errorf("error creating temporary file: %w", err)
	}
	defer os.Remove(tmpfile.Name()) // Clean up the temporary file

	// Write the content to the temporary file
	_, err = tmpfile.WriteString(containerFileContent)
	if err != nil {
		return fmt.Errorf("error writing to temporary file: %w", err)
	}
	_ = tmpfile.Close() // Close the temporary file

	// Build the single OCI image (note: We could include --squash-all option, as well)
	_, err = s.ops.RunInHostNamespace(
		"podman", []string{"build", "-f", tmpfile.Name(), "-t", s.containerRegistry, s.backupDir}...)
	if err != nil {
		return fmt.Errorf("failed to build seed image: %w", err)
	}

	// Push the created OCI image to user's repository
	_, err = s.ops.RunInHostNamespace(
		"podman", []string{"push", "--authfile", s.authFile, s.containerRegistry}...)
	if err != nil {
		return fmt.Errorf("failed to push seed image: %w", err)
	}

	return nil
}

func (s *SeedCreator) backupOstreeOrigin(statusRpmOstree *ostree.Status) error {

	// Get OSName for booted ostree deployment
	bootedOSName := statusRpmOstree.Deployments[0].OSName
	// Get ID for booted ostree deployment
	bootedID := statusRpmOstree.Deployments[0].ID
	// Get SHA for booted ostree deployment
	bootedDeployment := strings.Split(bootedID, "-")[1]

	// Check if the backup file for .origin doesn't exist
	originFileName := fmt.Sprintf("%s/ostree-%s.origin", s.backupDir, bootedDeployment)
	_, err := os.Stat(originFileName)
	if err == nil || !os.IsNotExist(err) {
		return err
	}
	// Execute 'copy' command and backup .origin file
	_, err = s.ops.RunInHostNamespace(
		"cp", []string{"/ostree/deploy/" + bootedOSName + "/deploy/" + bootedDeployment + ".origin", originFileName}...)
	if err != nil {
		return err
	}
	s.log.Info("Backup of .origin created successfully.")
	return nil
}

func (s *SeedCreator) renderInstallationEnvFile(recertContainerImage, seedFullDomain string) error {
	// Define source and destination file paths
	srcFile := filepath.Join(common.InstallationConfigurationFilesDir, "conf/installation-configuration.env")
	destFile := "/etc/systemd/system/installation-configuration.env"
	// Prepare variable substitutions for template files
	substitutions := map[string]any{
		"RecertContainerImage": recertContainerImage,
		"SeedFullDomain":       seedFullDomain,
	}

	return utils.RenderTemplateFile(srcFile, substitutions, destFile, 0o644)
}

func (s *SeedCreator) deleteNode(ctx context.Context) error {
	s.log.Info("Deleting node")
	node, err := utils.GetSNOMasterNode(ctx, s.client)
	if err != nil {
		return err
	}

	s.log.Info("Deleting node ", node.Name)
	err = s.client.Delete(ctx, node)
	if err != nil {
		return err
	}
	return nil
}

func (s *SeedCreator) waitTillOvnKubeNodeIsDown(ctx context.Context) {
	ovnKubeNode := "ovnkube-node"
	s.log.Infof("Waiting for %s to stop in order to give ovn to cleanup network", ovnKubeNode)
	// we will wait for 5 minutes and exit, we don't want to fail as it is not critical
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, 5*time.Minute)
	_ = wait.PollUntilContextCancel(deadlineCtx, 10*time.Second, true, func(ctx context.Context) (done bool, err error) {
		s.log.Infof("waiting for %s to stop", ovnKubeNode)
		pods := &corev1.PodList{}
		err = s.client.List(ctx, pods, &runtime.ListOptions{Namespace: "openshift-ovn-kubernetes"})
		if err != nil {
			return false, nil
		}
		for _, pod := range pods.Items {
			if strings.HasPrefix(pod.Name, "ovnkube-node") {
				return false, nil
			}
		}

		return true, nil
	})
	defer deadlineCancel()
}

func (s *SeedCreator) getCatalogImages(ctx context.Context) ([]string, error) {
	s.log.Info("Searching for catalog sources")
	var images []string
	catalogSources := &operatorsv1alpha1.CatalogSourceList{}
	allNamespaces := runtime.ListOptions{Namespace: metav1.NamespaceAll}
	if err := s.client.List(ctx, catalogSources, &allNamespaces); err != nil {
		return nil, fmt.Errorf("failed to list all catalogueSources %w", err)
	}

	for _, catalogSource := range catalogSources.Items {
		images = append(images, catalogSource.Spec.Image)
	}

	return images, nil
}
