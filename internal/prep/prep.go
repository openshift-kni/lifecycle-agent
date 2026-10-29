package prep

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/seedclusterinfo"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// need this for unit tests
var osReadFile = os.ReadFile
var osMkdirTemp = os.MkdirTemp

const seedSELinuxPackagesDir = "/usr/share/selinux/packages"

var seedSELinuxModuleName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.cil$`)

// getBootedStaterootIDFromRPMOstreeJson reads rpm-ostree.json file from the seed image
// and returns the deployment.ID of the booted stateroot
func getBootedStaterootIDFromRPMOstreeJson(path string) (string, error) {
	data, err := osReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading %s: %w", path, err)
	}
	var status rpmostreeclient.Status
	if err := json.Unmarshal(data, &status); err != nil {
		return "", fmt.Errorf("failed unmarshalling %s: %w", path, err)
	}
	for _, deploy := range status.Deployments {
		if deploy.Booted {
			return deploy.ID, nil
		}
	}
	return "", fmt.Errorf("failed finding booted stateroot")
}

// getVersionFromSeedClusterInfoFile reads ClusterInfo file and returns the ocp version
func getVersionFromSeedClusterInfoFile(path string) (string, error) {
	ci := &seedclusterinfo.SeedClusterInfo{}
	if err := utils.ReadYamlOrJSONFile(path, ci); err != nil {
		return "", fmt.Errorf("failed to read and decode ClusterInfo file: %w", err)
	}
	return ci.SeedClusterOCPVersion, nil
}

// getDeploymentOriginPath return the path to .origin file e.g:
// /ostree/deploy/<osname>/deploy/<deployment.id>.origin
func getDeploymentOriginPath(deploymentDir string) string {
	return deploymentDir + ".origin"
}

// removeETCDeletions remove the files that are listed in etc.deletions
func removeETCDeletions(mountpoint, deploymentDir string) error {
	file, err := os.Open(filepath.Join(common.PathOutsideChroot(mountpoint), "etc.deletions"))
	if err != nil {
		return fmt.Errorf("failed to open etc.deletions: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fileToRemove := strings.Trim(scanner.Text(), " ")
		filePath := common.PathOutsideChroot(filepath.Join(deploymentDir, fileToRemove))
		err = os.Remove(filePath)
		if err != nil {
			return fmt.Errorf("failed to remove %s: %w", filePath, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error while reading %s: %w", file.Name(), err)
	}
	return nil
}

// split the deploymentID by '-' and return the last item
// there should be at least one '-' in the deploymentID
func getDeploymentFromDeploymentID(deploymentID string) (string, error) {
	splitted := strings.Split(deploymentID, "-")
	if len(splitted) < 2 {
		return "", fmt.Errorf(
			"failed to get deployment from deploymentID, there should be a '-' in deploymentID %s",
			deploymentID)
	}
	return splitted[len(splitted)-1], nil
}

// VerifySeedOSTree checks the seed repository while the installation disk is still
// untouched. If the live ISO cannot restore a seed-specific SELinux label, load the
// seed's policy modules and retry extraction before treating the repository as corrupt.
func VerifySeedOSTree(log logr.Logger, ops ops.Ops, ostreeClient ostreeclient.IClient, seedImage string) error {
	defer func() { _ = ops.UnmountAndRemoveImage(seedImage) }()

	workspaceOutsideChroot, err := osMkdirTemp(common.PathOutsideChroot("/var/tmp"), "seed-ostree-preflight-")
	if err != nil {
		return fmt.Errorf("failed to create seed preflight workspace: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(workspaceOutsideChroot); err != nil {
			log.Error(err, "failed to clean up seed preflight workspace")
		}
	}()

	workspace, err := common.PathInsideChroot(workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("failed to resolve seed preflight workspace: %w", err)
	}
	mountpoint, err := ops.MountImage(seedImage)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}
	ostreeRepo := filepath.Join(workspace, "ostree")
	if err := os.Mkdir(filepath.Join(workspaceOutsideChroot, "ostree"), 0o700); err != nil {
		return fmt.Errorf("failed to create seed preflight repository directory: %w", err)
	}
	archivePath := filepath.Join(mountpoint, "ostree.tgz")
	extractErr := ops.ExtractTarWithSELinux(archivePath, ostreeRepo)
	var verifyErr error
	if extractErr == nil {
		verifyErr = ostreeClient.VerifyRepo(ostreeRepo)
	} else {
		// Some tar versions fail immediately on a label unknown to the live
		// policy. Read the repository once without xattrs so its own policy
		// modules can be located; the verified extraction below restores them.
		if err := resetSeedPreflightRepo(workspaceOutsideChroot); err != nil {
			return err
		}
		if err := ops.ExtractTarWithoutSELinux(archivePath, ostreeRepo); err != nil {
			return fmt.Errorf("failed to read seed ostree.tgz after SELinux extraction failed (%w): %w", extractErr, err)
		}
		verifyErr = extractErr
	}
	if verifyErr == nil {
		return nil
	}

	// A seed can contain policy supplied by the OpenShift machine OS image that is
	// absent from the matching base RHCOS live ISO. GNU tar can report an invalid
	// security.selinux xattr yet exit successfully, leaving a different object
	// checksum. The extracted repository still contains the seed's CIL modules, so
	// install those modules and repeat the extraction with the correct live policy.
	modulePaths, err := seedSELinuxPolicyModules(ops, mountpoint, ostreeRepo, workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("seed OSTree verification failed (%w); could not load seed SELinux policy: %w", verifyErr, err)
	}
	if len(modulePaths) == 0 {
		return fmt.Errorf("seed OSTree repository is corrupt before disk preparation: %w", verifyErr)
	}
	if _, err := ops.RunInHostNamespace("semodule", append([]string{"-i"}, modulePaths...)...); err != nil {
		return fmt.Errorf("failed to load seed SELinux policy before disk preparation: %w", err)
	}
	if err := resetSeedPreflightRepo(workspaceOutsideChroot); err != nil {
		return err
	}
	if err := ops.ExtractTarWithSELinux(archivePath, ostreeRepo); err != nil {
		return fmt.Errorf("failed to extract seed ostree.tgz with seed SELinux policy: %w", err)
	}
	if err := ostreeClient.VerifyRepo(ostreeRepo); err != nil {
		return fmt.Errorf("seed OSTree repository is corrupt after loading seed SELinux policy: %w", err)
	}
	return nil
}

func resetSeedPreflightRepo(workspaceOutsideChroot string) error {
	repo := filepath.Join(workspaceOutsideChroot, "ostree")
	if err := os.RemoveAll(repo); err != nil {
		return fmt.Errorf("failed to remove seed preflight repository before policy retry: %w", err)
	}
	if err := os.Mkdir(repo, 0o700); err != nil {
		return fmt.Errorf("failed to create seed preflight repository for policy retry: %w", err)
	}
	return nil
}

// seedSELinuxPolicyModules reads CIL packages from the booted seed deployment,
// rather than trusting file names or bytes outside the digest-pinned OSTree commit.
// Paths returned here are host paths for semodule; the files are removed with the
// preflight workspace after verification.
func seedSELinuxPolicyModules(ops ops.Ops, mountpoint, ostreeRepo, workspaceOutsideChroot string) ([]string, error) {
	seedBootedID, err := getBootedStaterootIDFromRPMOstreeJson(filepath.Join(common.PathOutsideChroot(mountpoint), "rpm-ostree.json"))
	if errors.Is(err, os.ErrNotExist) {
		// Older seed images may not carry deployment metadata. Preserve the
		// original fsck error rather than guessing at a commit.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	deployment, err := getDeploymentFromDeploymentID(seedBootedID)
	if err != nil {
		return nil, err
	}
	commit := strings.SplitN(deployment, ".", 2)[0]
	listing, err := ops.RunInHostNamespace("ostree", "ls", "--repo="+ostreeRepo,
		"--nul-filenames-only", commit, seedSELinuxPackagesDir)
	if err != nil {
		return nil, fmt.Errorf("listing seed SELinux policy packages: %w", err)
	}

	var modulePaths []string
	for _, path := range strings.Split(listing, "\x00") {
		name := filepath.Base(path)
		if filepath.Dir(path) != seedSELinuxPackagesDir || !seedSELinuxModuleName.MatchString(name) {
			continue
		}
		contents, err := ops.RunInHostNamespace("ostree", "cat", "--repo="+ostreeRepo, commit, path)
		if err != nil {
			return nil, fmt.Errorf("reading seed SELinux module %s: %w", name, err)
		}
		modulePath := filepath.Join(workspaceOutsideChroot, name)
		if err := os.WriteFile(modulePath, []byte(contents), 0o600); err != nil {
			return nil, fmt.Errorf("staging seed SELinux module %s: %w", name, err)
		}
		insidePath, err := common.PathInsideChroot(modulePath)
		if err != nil {
			return nil, fmt.Errorf("resolving seed SELinux module %s: %w", name, err)
		}
		modulePaths = append(modulePaths, insidePath)
	}
	return modulePaths, nil
}

func SetupStateroot(log logr.Logger, ops ops.Ops, ostreeClient ostreeclient.IClient,
	rpmOstreeClient rpmostreeclient.IClient, seedImage, expectedVersion string, ibi bool) error {
	log.Info("Start setupstateroot")

	defer func() { _ = ops.UnmountAndRemoveImage(seedImage) }()

	workspaceOutsideChroot, err := osMkdirTemp(common.PathOutsideChroot("/var/tmp"), "")
	if err != nil {
		return fmt.Errorf("failed to create temp directory %w", err)
	}

	defer func() {
		if err := os.RemoveAll(workspaceOutsideChroot); err != nil {
			log.Error(err, "failed to cleanup workspace")
		}
	}()

	workspace, err := common.PathInsideChroot(workspaceOutsideChroot)
	if err != nil {
		return fmt.Errorf("failed to get workspace relative path %w", err)
	}
	log.Info("workspace:" + workspace)

	if !ibi {
		if err = ops.RemountSysroot(); err != nil {
			return fmt.Errorf("failed to remount /sysroot: %w", err)
		}

	}

	mountpoint, err := ops.MountImage(seedImage)
	if err != nil {
		return fmt.Errorf("failed to mount seed image: %w", err)
	}

	ostreeRepo := filepath.Join(workspace, "ostree")
	if err = os.Mkdir(common.PathOutsideChroot(ostreeRepo), 0o700); err != nil {
		return fmt.Errorf("failed to create ostree repo directory: %w", err)
	}

	if err := ops.ExtractTarWithSELinux(
		fmt.Sprintf("%s/ostree.tgz", mountpoint), ostreeRepo,
	); err != nil {
		return fmt.Errorf("failed to extract ostree.tgz: %w", err)
	}
	if err := ostreeClient.VerifyRepo(ostreeRepo); err != nil {
		return fmt.Errorf("seed OSTree repository is corrupt after extraction: %w", err)
	}

	// example:
	// seedBootedID: rhcos-ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedDeployment: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c.1
	// seedBootedRef: ed4ab3244a76c6503a21441da650634b5abd25aba4255ca116782b2b3020519c
	seedBootedID, err := getBootedStaterootIDFromRPMOstreeJson(filepath.Join(common.PathOutsideChroot(mountpoint), "rpm-ostree.json"))
	if err != nil {
		return fmt.Errorf("failed to get booted stateroot id: %w", err)
	}
	seedBootedDeployment, err := getDeploymentFromDeploymentID(seedBootedID)
	if err != nil {
		return err
	}
	seedBootedRef := strings.Split(seedBootedDeployment, ".")[0]

	version, err := getVersionFromSeedClusterInfoFile(filepath.Join(common.PathOutsideChroot(mountpoint), common.SeedClusterInfoFileName))
	if err != nil {
		return fmt.Errorf("failed to get version from ClusterInfo: %w", err)
	}

	if version != expectedVersion {
		return fmt.Errorf("version specified in seed image (%s) differs from version in spec (%s)",
			version, expectedVersion)
	}

	osname := common.GetStaterootName(expectedVersion)

	if err = ostreeClient.PullLocal(ostreeRepo); err != nil {
		return fmt.Errorf("failed ostree pull-local: %w", err)
	}
	installedRepo := "/ostree/repo"
	if ibi {
		installedRepo = "/mnt/ostree/repo"
	}
	if err = ostreeClient.VerifyRepo(installedRepo); err != nil {
		return fmt.Errorf("installed OSTree repository is corrupt after pull-local: %w", err)
	}

	if err = ostreeClient.OSInit(osname); err != nil {
		return fmt.Errorf("failed ostree admin os-init: %w", err)
	}

	kargs, err := utils.BuildKernelArgumentsFromMCOFile(filepath.Join(common.PathOutsideChroot(mountpoint), "mco-currentconfig.json"))
	if err != nil {
		return fmt.Errorf("failed to build kargs: %w", err)
	}

	if !ibi {
		// Append a unique karg
		kargs = append(kargs, "--karg", "ibu="+expectedVersion)
	}

	if err = ostreeClient.Deploy(osname, seedBootedRef, kargs, rpmOstreeClient, ibi); err != nil {
		return fmt.Errorf("failed ostree admin deploy: %w", err)
	}

	deploymentDir, err := ostreeClient.GetDeploymentDir(osname)
	if err != nil {
		return fmt.Errorf("failed to get deployment dir: %w", err)
	}

	if err = common.CopyOutsideChroot(
		filepath.Join(mountpoint, fmt.Sprintf("ostree-%s.origin", seedBootedDeployment)),
		getDeploymentOriginPath(deploymentDir),
	); err != nil {
		return fmt.Errorf("failed to restore origin file: %w", err)
	}

	if err = ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "var.tgz"),
		common.GetStaterootPath(osname),
	); err != nil {
		return fmt.Errorf("failed to restore var directory: %w", err)
	}

	if err := ops.ExtractTarWithSELinux(
		filepath.Join(mountpoint, "etc.tgz"),
		deploymentDir,
	); err != nil {
		return fmt.Errorf("failed to extract seed etc: %w", err)
	}

	if err = removeETCDeletions(mountpoint, deploymentDir); err != nil {
		return fmt.Errorf("failed to process etc.deletions: %w", err)
	}

	if err := common.CopyOutsideChroot(filepath.Join(mountpoint, common.ContainersListFileName), common.ContainersListFilePath); err != nil {
		return fmt.Errorf("failed to copy image list file: %w", err)
	}

	if err := common.CopyOutsideChroot(filepath.Join(mountpoint, common.SeedClusterInfoFileName), common.IBISeedInfoFilePath); err != nil {
		return fmt.Errorf("failed to copy %s file: %w", common.SeedClusterInfoFileName, err)
	}

	log.Info("Stateroot setup done successfully")
	return nil
}

func ReadPrecachingList(imageListFile, clusterRegistry, seedRegistry string, overrideSeedRegistry bool) (imageList []string, err error) {
	var content []byte
	content, err = os.ReadFile(common.PathOutsideChroot(imageListFile))
	if err != nil {
		return
	}

	lines := strings.Split(string(content), "\n")
	// Filter out empty lines
	for _, line := range lines {
		image := line
		if line == "" {
			continue
		}
		if overrideSeedRegistry {
			image, err = utils.ReplaceImageRegistry(image, clusterRegistry, seedRegistry)
			if err != nil {
				return nil, fmt.Errorf("failed to replace image registry %s-%s-%s: %w", image, clusterRegistry, seedRegistry, err)
			}
		}
		imageList = append(imageList, image)
	}

	return imageList, nil
}
