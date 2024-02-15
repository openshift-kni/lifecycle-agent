package seedcreator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	cp "github.com/otiai10/copy"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	runtime "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
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
	}
}

// CreateSeedImage comprises the lca-cli workflow for creating a single OCI seed image
func (s *SeedCreator) CreateSeedImage() error {
	s.log.Info("Creating seed image")
	ctx := context.TODO()

	if err := s.copyConfigurationFiles(); err != nil {
		return fmt.Errorf("failed to add configuration files: %w", err)
	}

	// create backup dir
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return fmt.Errorf("failed to backup dir for seed image to %s: %w", s.backupDir, err)
	}

	s.log.Info("Copy lca-cli binary")
	err := cp.Copy("/usr/local/bin/lca-cli", "/var/usrlocal/bin/lca-cli", cp.Options{AddPermission: os.FileMode(0o777)})
	if err != nil {
		return fmt.Errorf("failed to copy lca-cli binary: %w", err)
	}

	if err := os.MkdirAll(common.BackupChecksDir, 0o700); err != nil {
		return fmt.Errorf("failed to make dir in %s: %w", common.BackupChecksDir, err)
	}

	if err := utils.RunOnce("create_container_list", common.BackupChecksDir, s.log, s.createContainerList, ctx); err != nil {
		return fmt.Errorf("failed to run once create_container_list: %w", err)
	}

	if err := utils.RunOnce("gather_cluster_info", common.BackupChecksDir, s.log, s.gatherClusterInfo, ctx); err != nil {
		return fmt.Errorf("failed to run once gather_cluster_info: %w", err)
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping seed certificates backing up.")
	} else {
		s.log.Info("Backing up seed cluster certificates for recert tool")
		if err := utils.BackupKubeconfigCrypto(ctx, s.client, common.BackupCertsDir); err != nil {
			return fmt.Errorf("failed to backing up seed cluster certificates for recert tool: %w", err)
		}
		s.log.Info("Seed cluster certificates backed up successfully for recert tool")
	}

	if err := s.stopServices(); err != nil {
		return fmt.Errorf("failed to stop services to create seed image: %w", err)
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping recert validation.")
	} else {
		if err := utils.RunOnce("recert", common.BackupChecksDir, s.log, s.ops.ForceExpireSeedCrypto, s.recertContainerImage, s.authFile); err != nil {
			return fmt.Errorf("failed to run once recert: %w", err)
		}
	}
	if err := s.removeOvnCertsFolders(); err != nil {
		return fmt.Errorf("failed remove all OVN certs folders: %w", err)
	}

	if err := utils.RunOnce("backup_var", common.BackupChecksDir, s.log, s.backupVar); err != nil {
		return fmt.Errorf("failed to run once backup_var: %w", err)
	}

	if err := utils.RunOnce("backup_etc", common.BackupChecksDir, s.log, s.backupEtc); err != nil {
		return fmt.Errorf("failed to run once backup_etc: %w", err)
	}

	if err := utils.RunOnce("backup_ostree", common.BackupChecksDir, s.log, s.backupOstree); err != nil {
		return fmt.Errorf("failed to run once backup_ostree: %w", err)
	}

	if err := utils.RunOnce("backup_rpmostree", common.BackupChecksDir, s.log, s.backupRPMOstree); err != nil {
		return fmt.Errorf("failed to run once backup_rpmostree: %w", err)
	}

	if err := utils.RunOnce("backup_mco_config", common.BackupChecksDir, s.log, s.backupMCOConfig); err != nil {
		return fmt.Errorf("failed to run once backup_mco_config: %w", err)
	}

	if err := s.createAndPushSeedImage(); err != nil {
		return fmt.Errorf("failed to create and push seed image: %w", err)
	}

	return nil
}

func (s *SeedCreator) copyConfigurationFiles() error {

	return s.handleServices()
}

func (s *SeedCreator) handleServices() error {
	dir := filepath.Join(common.InstallationConfigurationFilesDir, "services")
	return utils.HandleFilesWithCallback(dir, func(path string) error { //nolint:wrapcheck
		serviceName := filepath.Base(path)

		s.log.Infof("Creating service %s", serviceName)
		if err := cp.Copy(path, filepath.Join("/etc/systemd/system/", serviceName)); err != nil {
			return fmt.Errorf("failed create service %s: %w", serviceName, err)
		}

		s.log.Infof("Enabling service %s", serviceName)
		if _, err := s.ops.SystemctlAction("enable", serviceName); err != nil {
			return fmt.Errorf("failed to enabling service %s: %w", serviceName, err)
		}
		return nil
	})
}

func (s *SeedCreator) gatherClusterInfo(ctx context.Context) error {
	s.log.Info("Saving seed cluster configuration")
	clusterInfo, err := utils.GetClusterInfo(ctx, s.client)
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	seedClusterInfo := seedclusterinfo.NewFromClusterInfo(clusterInfo, s.recertContainerImage)

	if err := os.MkdirAll(common.SeedDataDir, os.ModePerm); err != nil {
		return fmt.Errorf("error creating SeedDataDir %s: %w", common.SeedDataDir, err)
	}

	s.log.Infof("Creating seed information file in %s", common.SeedClusterInfoFileName)
	p := path.Join(common.SeedDataDir, common.SeedClusterInfoFileName)
	if err := utils.MarshalToFile(seedClusterInfo, p); err != nil {
		return fmt.Errorf("error creating seed info file in %s: %w", p, err)
	}

	// in order to allow lca to verify version we need to provide file not as part of var archive too
	src := path.Join(common.SeedDataDir, common.SeedClusterInfoFileName)
	dest := path.Join(s.backupDir, common.SeedClusterInfoFileName)
	if err := cp.Copy(src, dest); err != nil {
		return fmt.Errorf("error copying from %s to %s: %w", src, dest, err)
	}

	return nil
}

func (s *SeedCreator) createContainerList(ctx context.Context) error {
	s.log.Info("Saving list of running containers and catalogsources.")
	containersListFileName := s.backupDir + "/containers.list"

	// purge all unknown image if exists
	s.log.Info("Cleaning image list")
	// Don't ever add -a option as we don't want to delete unused images
	if _, err := s.ops.RunBashInHostNamespace("podman", "image", "prune", "-f"); err != nil {
		return fmt.Errorf("failed to prune with podmamn: %w", err)
	}

	// Execute 'crictl images -o json' command, parse the JSON output and extract image references using 'jq'
	s.log.Info("Save list of downloaded images")
	args := []string{"images", "-o", "json", "|", "jq", "-r",
		"'.images[] | if .repoTags | length > 0 then .repoTags[] else .repoDigests[] end'"}

	output, err := s.ops.RunBashInHostNamespace("crictl", args...)
	if err != nil {
		return fmt.Errorf("failed to run crictl with args %s: %w", args, err)
	}
	images := strings.Split(output, "\n")
	images, err = s.filterCatalogImages(ctx, images)
	if err != nil {
		return fmt.Errorf("failed to filter catalog images: %w", err)
	}

	s.log.Infof("Adding recert %s image to image list", s.recertContainerImage)
	images = utils.AppendToListIfNotExists(images, s.recertContainerImage)

	s.log.Infof("Creating %s file", containersListFileName)
	if err := os.WriteFile(containersListFileName, []byte(strings.Join(images, "\n")), 0o600); err != nil {
		return fmt.Errorf("failed to write container list file %s, err %w", containersListFileName, err)
	}

	s.log.Info("List of containers  saved successfully.")
	return nil
}

func (s *SeedCreator) stopServices() error {
	s.log.Info("Stop kubelet service")
	_, err := s.ops.SystemctlAction("stop", "kubelet.service")
	if err != nil {
		return fmt.Errorf("failed to stop kubelet: %w", err)
	}

	s.log.Info("Disabling kubelet service")
	_, err = s.ops.SystemctlAction("disable", "kubelet.service")
	if err != nil {
		return fmt.Errorf("failed to disable kubelet: %w", err)
	}

	s.log.Info("Stopping containers and CRI-O runtime.")
	crioSystemdStatus, err := s.ops.SystemctlAction("is-active", "crio")
	var exitErr *exec.ExitError
	// If ExitCode is 3, the command succeeded and told us that crio is down
	if err != nil && errors.As(err, &exitErr) && exitErr.ExitCode() != 3 {
		return fmt.Errorf("failed to checking crio status: %w", err)
	}
	s.log.Info("crio status is ", crioSystemdStatus)
	if crioSystemdStatus == "active" {
		// CRI-O is active, so stop running containers with retry
		_ = wait.PollUntilContextCancel(context.TODO(), time.Second, true, func(ctx context.Context) (done bool, err error) {
			s.log.Info("Stop running containers")
			args := []string{"ps", "-q", "|", "xargs", "--no-run-if-empty", "--max-args", "1", "--max-procs", "10", "crictl", "stop", "--timeout", "5"}
			_, err = s.ops.RunBashInHostNamespace("crictl", args...)
			if err != nil {
				return false, fmt.Errorf("failed to stop running containers: %w", err)
			}
			return true, nil
		})

		// Execute a D-Bus call to stop the CRI-O runtime
		s.log.Debug("Stopping CRI-O engine")
		_, err = s.ops.SystemctlAction("stop", "crio.service")
		if err != nil {
			return fmt.Errorf("failed to stop crio engine: %w", err)
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
		"*/.bash_history",
		"/var/tmp/*",
		"/var/log/*",
		"/var/lib/lca",
		"/var/lib/log/*",
		"/var/lib/cni/bin/*",
		"/var/lib/containers/*",
		"/var/lib/kubelet/pods/*",
		common.OvnNodeCerts + "/*",
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
		return fmt.Errorf("failed to run tar for backupVar: %w", err)
	}

	s.log.Infof("Backup of %s created successfully.", common.VarFolder)
	return nil
}

func (s *SeedCreator) backupEtc() error {
	s.log.Info("Backing up /etc")

	// Execute 'ostree admin config-diff' command and backup etc.deletions
	excludePatterns := []string{
		"/etc/NetworkManager/system-connections",
		"/etc/machine-config-daemon/orig/var/lib/kubelet",
	}
	tarArgs := []string{"tar", "czf", path.Join(s.backupDir + "/etc.tgz")}
	for _, pattern := range excludePatterns {
		// We're handling the excluded patterns in bash, we need to single quote them to prevent expansion
		tarArgs = append(tarArgs, "--exclude", fmt.Sprintf("'%s'", pattern))
	}
	tarArgs = append(tarArgs, "--selinux", "-T", "-")

	args := []string{"admin", "config-diff", "|", "awk", `'$1 == "D" {print "/etc/" $2}'`, ">",
		path.Join(s.backupDir, "/etc.deletions")}
	_, err := s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return fmt.Errorf("failed backing up /etc with args %s: %w", args, err)
	}

	args = []string{"admin", "config-diff", "|", "grep", "-v", "'cni/multus'",
		"|", "awk", `'$1 != "D" {print "/etc/" $2}'`, "|"}
	args = append(args, tarArgs...)

	_, err = s.ops.RunBashInHostNamespace("ostree", args...)
	if err != nil {
		return fmt.Errorf("failed backing up /etc with args %s: %w", args, err)
	}
	s.log.Info("Backup of /etc created successfully.")

	return nil
}

func (s *SeedCreator) backupOstree() error {
	s.log.Info("Backing up ostree")
	ostreeTar := s.backupDir + "/ostree.tgz"

	// Execute 'tar' command and backup /etc
	args := []string{"czf", ostreeTar, "--selinux", "-C", "/ostree/repo", "."}
	if _, err := s.ops.RunBashInHostNamespace("tar", args...); err != nil {
		return fmt.Errorf("failed backing ostree with args %s: %w", args, err)
	}

	return nil
}

func (s *SeedCreator) backupRPMOstree() error {
	rpmJSON := s.backupDir + "/rpm-ostree.json"
	args := append([]string{"status", "-v", "--json"}, ">", rpmJSON)
	if _, err := s.ops.RunBashInHostNamespace("rpm-ostree", args...); err != nil {
		return fmt.Errorf("failed to run backup rpmostree with args %s: %w", args, err)
	}
	s.log.Info("Backup of rpm-ostree.json created successfully.")
	return nil
}

func (s *SeedCreator) backupMCOConfig() error {
	mcoJSON := s.backupDir + "/mco-currentconfig.json"
	if _, err := s.ops.RunBashInHostNamespace("cp", "/etc/machine-config-daemon/currentconfig", mcoJSON); err != nil {
		return fmt.Errorf("failed to backup MCO config: %w", err)
	}
	s.log.Info("Backup of mco-currentconfig created successfully.")
	return nil
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
	podmanBuildArgs := []string{
		"build",
		"--file", tmpfile.Name(),
		"--tag", s.containerRegistry,
		"--label", fmt.Sprintf("%s=%d", common.SeedFormatOCILabel, common.SeedFormatVersion),
		s.backupDir,
	}
	_, err = s.ops.RunInHostNamespace(
		"podman", podmanBuildArgs...)
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
		return fmt.Errorf("failed to get file info for %s: %w", originFileName, err)
	}
	// Execute 'copy' command and backup .origin file
	_, err = s.ops.RunInHostNamespace(
		"cp", []string{"/ostree/deploy/" + bootedOSName + "/deploy/" + bootedDeployment + ".origin", originFileName}...)
	if err != nil {
		return fmt.Errorf("failed 'copy' command to backup .origin file,: %w", err)
	}
	s.log.Info("Backup of .origin created successfully.")
	return nil
}

// filterCatalogImages filters catalog source images as catalog sources have pull always policy
// and there is no point to precache them.
// List of catalog images are taken from catalog sources that are currently configured on cluster and known ocp ones.
// Filtering known images will allow us to fix the case where those images were pulled in seed
// and after it their catalog sources were removed but images are still part of podman images output as we create seed
func (s *SeedCreator) filterCatalogImages(ctx context.Context, images []string) ([]string, error) {
	// Regex matches list of known catalog images, there are 4 of them at least in 4.15
	// registry.redhat.io/redhat/community-operator-index:v4.15
	// registry.redhat.io/redhat/redhat-operator-index:v4.15
	// registry.redhat.io/redhat/certified-operator-index:v4.15
	// registry.redhat.io/redhat/redhat-marketplace-index:v4.15
	defaultCatalogsRegex := regexp.MustCompile(`^registry\.redhat\.io/redhat/.+-index:.+`)
	s.log.Info("Searching for catalog sources")
	var catalogImages []string

	catalogSources := &operatorsv1alpha1.CatalogSourceList{}
	allNamespaces := runtime.ListOptions{Namespace: metav1.NamespaceAll}
	if err := s.client.List(ctx, catalogSources, &allNamespaces); err != nil {
		return nil, fmt.Errorf("failed to list all catalogueSources %w", err)
	}

	for _, catalogSource := range catalogSources.Items {
		catalogImages = append(catalogImages, catalogSource.Spec.Image)
	}

	s.log.Infof("Removing list of catalog images from full image list, catalog images to remove %s", catalogImages)
	images = lo.Filter(images, func(image string, _ int) bool {
		// removing found catalog images + defaults
		return !lo.Contains(catalogImages, image) && !defaultCatalogsRegex.MatchString(image)
	})

	return images, nil
}

func (s *SeedCreator) removeOvnCertsFolders() error {
	s.log.Infof("Removing ovn certs folders")
	dirs := []string{common.OvnNodeCerts, common.MultusCerts}
	if err := utils.RemoveListOfFolders(s.log, dirs); err != nil {
		return fmt.Errorf("failed to remove ovn certs in %s: %w", dirs, err)
	}
	return nil
}
