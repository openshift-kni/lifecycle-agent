package seedcreator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	ignconfig "github.com/coreos/ignition/v2/config"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	cp "github.com/otiai10/copy"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	runtime "sigs.k8s.io/controller-runtime/pkg/client"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	_ "embed"
	controller_utils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	ostree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

//go:embed Containerfile
var defaultContainerfileContent string

//go:embed Containerfile.bootc
var bootcContainerileContent string

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
	useBootc             bool
}

// NewSeedCreator is a constructor function for SeedCreator
func NewSeedCreator(client runtime.Client, log *logrus.Logger, ops ops.Ops, ostreeClient *ostree.Client, backupDir,
	kubeconfig, containerRegistry, authFile, recertContainerImage string, recertSkipValidation bool, useBootc bool) *SeedCreator {

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
		useBootc:             useBootc,
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

	lcaBinaryPath := "/usr/local/bin/lca-cli"
	s.log.Info("Copy lca-cli binary")
	err := cp.Copy(lcaBinaryPath, "/var/usrlocal/bin/lca-cli", cp.Options{AddPermission: os.FileMode(0o777)})
	if err != nil {
		return fmt.Errorf("failed to copy lca-cli binary: %w", err)
	}

	// copied to backup dir in order to be included in the seed image without being part of the var archive
	s.log.Info("Copy lca-cli binary, into backup dir")
	if err := cp.Copy(lcaBinaryPath, path.Join(s.backupDir, "lca-cli"), cp.Options{AddPermission: os.FileMode(0o777)}); err != nil {
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

	seedHasKubeadminPassword := false
	if s.recertSkipValidation {
		s.log.Info("Skipping seed certificates backing up.")
	} else {
		s.log.Info("Backing up seed cluster certificates for recert tool")
		if err := utils.BackupKubeconfigCrypto(ctx, s.client, common.BackupCertsDir); err != nil {
			return fmt.Errorf("failed to backing up seed cluster certificates for recert tool: %w", err)
		}
		if seedHasKubeadminPassword, err = utils.BackupKubeadminPasswordHash(ctx, s.client, common.BackupCertsDir); err != nil {
			return fmt.Errorf("failed to backup kubeadmin password hash: %w", err)
		}
		s.log.Info("Seed cluster certificates backed up successfully for recert tool")
	}

	if err := s.stopServices(); err != nil {
		return fmt.Errorf("failed to stop services to create seed image: %w", err)
	}

	if s.recertSkipValidation {
		s.log.Info("Skipping recert validation.")
	} else {
		if err := utils.RunOnce("recert", common.BackupChecksDir, s.log, s.ops.ForceExpireSeedCrypto,
			s.recertContainerImage, s.authFile, seedHasKubeadminPassword); err != nil {
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

	if !s.useBootc {
		// ostree archive is not needed for bootc images
		if err := utils.RunOnce("backup_ostree", common.BackupChecksDir, s.log, s.backupOstree); err != nil {
			return fmt.Errorf("failed to run once backup_ostree: %w", err)
		}
	}

	if err := utils.RunOnce("backup_rpmostree", common.BackupChecksDir, s.log, s.backupRPMOstree); err != nil {
		return fmt.Errorf("failed to run once backup_rpmostree: %w", err)
	}

	if err := utils.RunOnce("backup_mco_config", common.BackupChecksDir, s.log, s.backupMCOConfig); err != nil {
		return fmt.Errorf("failed to run once backup_mco_config: %w", err)
	}

	clusterInfoJSONSBytes, err := os.ReadFile(path.Join(s.backupDir, common.SeedClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to read cluster info file: %w", err)
	}

	clusterInfoJSON := string(clusterInfoJSONSBytes)
	if err := s.createAndPushSeedImage(clusterInfoJSON); err != nil {
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

func GetSeedAdditionalTrustBundleState(ctx context.Context, client runtimeclient.Client) (*seedclusterinfo.AdditionalTrustBundle, error) {
	hasUserCaBundle, proxyConfigmapName, err := utils.GetClusterAdditionalTrustBundleState(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster additional trust bundle state: %w", err)
	}

	result := seedclusterinfo.AdditionalTrustBundle{
		HasUserCaBundle:    hasUserCaBundle,
		ProxyConfigmapName: proxyConfigmapName,
	}

	return &result, nil
}

func (s *SeedCreator) gatherClusterInfo(ctx context.Context) error {
	s.log.Info("Saving seed cluster configuration")
	clusterInfo, err := utils.GetClusterInfo(ctx, s.client)
	if err != nil {
		return fmt.Errorf("failed to get cluster info: %w", err)
	}

	hasProxy, err := utils.HasProxy(ctx, s.client)
	if err != nil {
		return fmt.Errorf("failed to get proxy information: %w", err)
	}

	hasFIPS, err := utils.HasFIPS(ctx, s.client)
	if err != nil {
		return fmt.Errorf("failed to get FIPS information: %w", err)
	}

	seedAdditionalTrustBundle, err := GetSeedAdditionalTrustBundleState(ctx, s.client)
	if err != nil {
		return fmt.Errorf("failed to get additional trust bundle information: %w", err)
	}

	containerStorageMountpointTarget, err := s.ops.GetContainerStorageTarget()
	if err != nil {
		return fmt.Errorf("failed to get container storage mountpoint target: %w", err)
	}

	seedClusterInfo := seedclusterinfo.NewFromClusterInfo(
		clusterInfo,
		s.recertContainerImage,
		hasProxy,
		hasFIPS,
		seedAdditionalTrustBundle,
		containerStorageMountpointTarget,
		clusterInfo.IngressCertificateCN,
	)

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

func (s *SeedCreator) getCsvRelatedImages(ctx context.Context) (imageList []string, rc error) {
	clusterServiceVersionList := operatorsv1alpha1.ClusterServiceVersionList{}
	err := s.client.List(ctx, &clusterServiceVersionList)
	if err != nil {
		rc = fmt.Errorf("failed to get csv list: %w", err)
		return
	}

	// Filter images from list based on a pattern, to exclude images we know aren't applicable
	filter := regexp.MustCompile(`-for-microsoft-azure|-for-gcp|-for-csi`)

	for _, csv := range clusterServiceVersionList.Items {
		for _, img := range csv.Spec.RelatedImages {
			if !filter.MatchString(img.Image) {
				imageList = append(imageList, img.Image)
			}
		}
	}

	return
}

func (s *SeedCreator) createContainerList(ctx context.Context) error {
	s.log.Info("Saving list of running containers and catalogsources.")
	containersListFileName := filepath.Join(s.backupDir, common.ContainersListFileName)

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

	s.log.Infof("Adding related images from CSVs")
	relatedImages, err := s.getCsvRelatedImages(ctx)
	if err != nil {
		return fmt.Errorf("failed to get related images from CSVs: %w", err)
	}

	for _, img := range relatedImages {
		images = utils.AppendToListIfNotExists(images, img)
	}

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

// getVarLibFilelist parses the MCD currentconfig to get the list of managed files under /var/lib
func (s *SeedCreator) getVarLibFilelist() ([]string, error) {
	var filelist []string
	varlibRegex := regexp.MustCompile(`^/var/lib/`)

	data, err := os.ReadFile(common.MCDCurrentConfig)
	if err != nil {
		return filelist, fmt.Errorf("unable to read MCD currentconfig: %w", err)
	}

	var mc mcv1.MachineConfig

	if err := json.Unmarshal(data, &mc); err != nil {
		return filelist, fmt.Errorf("unable to parse MCD currentconfig: %w", err)
	}

	if mc.Spec.Config.Raw == nil {
		return filelist, fmt.Errorf("unable to find config in MCD currentconfig")
	}

	ign, _, err := ignconfig.Parse(mc.Spec.Config.Raw)
	if err != nil {
		return filelist, fmt.Errorf("unable to parse ignition config from MCD currentconfig: %w", err)
	}

	for _, f := range ign.Storage.Files {
		if varlibRegex.MatchString(f.Path) {
			filelist = append(filelist, f.Path)
		}
	}

	return filelist, nil
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
		common.ContainerStoragePath + "/*",
		"/var/lib/kubelet/pods/*",
		common.OvnIcEtcFolder + "/*",
	}

	// Build the tar command
	tarArgs := []string{"czf", varTarFile}

	// Ensure all MCD-managed files in /var/lib are explicitly included, to avoid accidental exclusion
	if managedfiles, err := s.getVarLibFilelist(); err != nil {
		return fmt.Errorf("unable to get list of MCD managed files: %w", err)
	} else {
		for _, fname := range managedfiles {
			tarArgs = append(tarArgs, "--add-file", fname)
		}
	}

	for _, pattern := range excludePatterns {
		// We're handling the excluded patterns in bash, we need to single quote them to prevent expansion
		tarArgs = append(tarArgs, "--exclude", fmt.Sprintf("'%s'", pattern))
	}
	tarArgs = append(tarArgs, common.VarFolder)
	tarArgs = append(tarArgs, common.TarOpts...)

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
		"/etc/openvswitch/conf.db",
		"/etc/openvswitch/.conf.db.~lock~",
		"/etc/hostname",
	}
	tarArgs := []string{"tar", "czf", path.Join(s.backupDir + "/etc.tgz")}
	for _, pattern := range excludePatterns {
		// We're handling the excluded patterns in bash, we need to single quote them to prevent expansion
		tarArgs = append(tarArgs, "--exclude", fmt.Sprintf("'%s'", pattern))
	}
	tarArgs = append(tarArgs, "-T", "-")
	tarArgs = append(tarArgs, common.TarOpts...)

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
	args := []string{"czf", ostreeTar, "-C", "/ostree/repo", "."}
	args = append(args, common.TarOpts...)
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
	if _, err := s.ops.RunBashInHostNamespace("cp", common.MCDCurrentConfig, mcoJSON); err != nil {
		return fmt.Errorf("failed to backup MCO config: %w", err)
	}
	s.log.Info("Backup of mco-currentconfig created successfully.")
	return nil
}

func getOsImageURL() (string, error) {
	data, err := os.ReadFile(common.MCDCurrentConfig)
	if err != nil {
		return "", fmt.Errorf("unable to read MCD currentconfig: %w", err)
	}

	var mc mcv1.MachineConfig

	if err := json.Unmarshal(data, &mc); err != nil {
		return "", fmt.Errorf("unable to parse MCD currentconfig: %w", err)
	}

	if mc.Spec.OSImageURL == "" {
		return "", fmt.Errorf("unable to find osimageurl in MCD currentconfig")
	}

	return mc.Spec.OSImageURL, nil
}

// Building and pushing OCI image
func (s *SeedCreator) createAndPushSeedImage(clusterInfo string) error {
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

	osImageURL, err := getOsImageURL()
	if err != nil {
		return fmt.Errorf("failed to get osimageurl from MCD currentconfig: %w", err)
	}

	containerFileContent := defaultContainerfileContent
	if s.useBootc {
		containerFileContent = bootcContainerileContent
	}

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
		"--label", fmt.Sprintf("%s=%s", common.SeedClusterInfoOCILabel, clusterInfo),
		s.backupDir,
	}

	if s.useBootc {
		podmanBuildArgs = append(podmanBuildArgs,
			// A pull secret is only needed for bootc images because normal ones are FROM scratch.
			// We must use the backed up pull secret and not /var/lib/kubelet/config.json (because
			// the latter is wiped clean with dummy values before seed generation so that the pull
			// secret is not leaked in the seed, so it is useless)
			"--authfile", controller_utils.StoredPullSecret,
			"--build-arg", fmt.Sprintf("SEED_STAGE_BASE_IMAGE=%s", osImageURL),
			"--label", fmt.Sprintf("%s=%s", common.SeedUseBootcOCILabel, "true"),
		)
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
