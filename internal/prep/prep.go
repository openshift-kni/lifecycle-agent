package prep

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// need this for unit tests
var osReadFile = os.ReadFile

// GetBootedStaterootIDFromRPMOstreeJson reads rpm-ostree.json file from the seed image
// and returns the deployment.ID of the booted stateroot
func GetBootedStaterootIDFromRPMOstreeJson(path string) (string, error) {
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

// GetVersionFromClusterInfoFile reads ClusterInfo file and returns the ocp version
func GetVersionFromClusterInfoFile(path string) (string, error) {
	ci := &clusterinfo.ClusterInfo{}
	if err := utils.ReadYamlOrJSONFile(path, ci); err != nil {
		return "", fmt.Errorf("failed to read and decode ClusterInfo file: %w", err)
	}
	return ci.Version, nil
}

// BuildKernelArguementsFromMCOFile reads the kernel arguments from MCO file
// and builds the string arguments that ostree admin deploy requires
func BuildKernelArgumentsFromMCOFile(path string) ([]string, error) {
	mc := &mcfgv1.MachineConfig{}
	if err := utils.ReadYamlOrJSONFile(path, mc); err != nil {
		return nil, fmt.Errorf("failed to read and decode machine config json file: %w", err)
	}

	args := make([]string, len(mc.Spec.KernelArguments)*2)
	for i, karg := range mc.Spec.KernelArguments {
		// if we don't marshal the karg, `"` won't appear in the kernel arguments after reboot
		if val, err := json.Marshal(karg); err != nil {
			return nil, fmt.Errorf("failed to marshal karg %s: %w", karg, err)
		} else {
			args[2*i] = "--karg-append"
			args[2*i+1] = string(val)
		}
	}
	return args, nil
}

// GetDeploymentDirPath return the path to ostree deploy directory e.g:
// /ostree/deploy/<osname>/deploy/<deployment.id>
func GetDeploymentDirPath(osname, deployment string) string {
	return filepath.Join(common.GetStaterootPath(osname), fmt.Sprintf("deploy/%s", deployment))
}

// GetDeploymentOriginPath return the path to .orign file e.g:
// /ostree/deploy/<osname>/deploy/<deployment.id>.origin
func GetDeploymentOriginPath(osname, deployment string) string {
	originName := fmt.Sprintf("%s.origin", deployment)
	return filepath.Join(common.GetStaterootPath(osname), fmt.Sprintf("deploy/%s", originName))
}

// RemoveETCDeletions remove the files that are listed in etc.deletions
func RemoveETCDeletions(mountpoint, osname, deployment string) error {
	file, err := os.Open(filepath.Join(common.PathOutsideChroot(mountpoint), "etc.deletions"))
	if err != nil {
		return fmt.Errorf("failed to open etc.deletions: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fileToRemove := strings.Trim(scanner.Text(), " ")
		filePath := common.PathOutsideChroot(filepath.Join(GetDeploymentDirPath(osname, deployment), fileToRemove))
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
func GetDeploymentFromDeploymentID(deploymentID string) (string, error) {
	splitted := strings.Split(deploymentID, "-")
	if len(splitted) < 2 {
		return "", fmt.Errorf(
			"failed to get deployment from deploymentID, there should be a '-' in deploymentID %s",
			deploymentID)
	}
	return splitted[len(splitted)-1], nil
}
