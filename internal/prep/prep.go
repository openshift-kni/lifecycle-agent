package prep

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/ibu-imager/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// need this for unit tests
var osReadFile = os.ReadFile

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

func GetVersionFromClusterInfoFile(path string) (string, error) {
	data, err := osReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed reading %s: %w", path, err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("failed to unmarshal %s: %w", path, err)
	}
	version, ok := v["version"].(string)
	if !ok {
		return "", fmt.Errorf("failed to get version from %s: %w", path, err)
	}
	return version, nil
}

func BuildKernelArgumentsFromMCOFile(path string) ([]string, error) {
	mcJSON, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer mcJSON.Close()
	mc := &mcfgv1.MachineConfig{}
	if err := json.NewDecoder(bufio.NewReader(mcJSON)).Decode(mc); err != nil {
		return nil, fmt.Errorf("failed to read and decode machine config json file: %w", err)
	}

	args := make([]string, len(mc.Spec.KernelArguments)*2)
	for i, karg := range mc.Spec.KernelArguments {
		args[2*i] = "--karg-append"
		args[2*i+1] = karg
	}
	return args, nil
}

func GetDeploymentDirPath(osname, deploymentID string) string {
	deployDirName := strings.Split(deploymentID, "-")[1]
	return filepath.Join(common.GetStaterootPath(osname), fmt.Sprintf("deploy/%s", deployDirName))
}

func GetDeploymentOriginPath(osname, deploymentID string) string {
	originName := fmt.Sprintf("%s.origin", strings.Split(deploymentID, "-")[1])
	return filepath.Join(common.GetStaterootPath(osname), fmt.Sprintf("deploy/%s", originName))
}

func RemoveETCDeletions(mountpoint, osname, deploymentID string, log logr.Logger) error {
	file, err := os.Open(filepath.Join(common.PathOutsideChroot(mountpoint), "etc.deletions"))
	if err != nil {
		return fmt.Errorf("failed to open etc.deletions: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fileToRemove := strings.Trim(scanner.Text(), " ")
		filePath := common.PathOutsideChroot(filepath.Join(GetDeploymentDirPath(osname, deploymentID), fileToRemove))
		log.Info("removing file: " + filePath)
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

func BackupCertificates(ctx context.Context, osname string, client *clusterinfo.InfoClient) error {
	certsDir := filepath.Join(common.GetStaterootPath(osname), "/var/opt/openshift/certs")
	if err := os.MkdirAll(common.PathOutsideChroot(certsDir), os.ModePerm); err != nil {
		return fmt.Errorf("failed to create cert directory %s: %w", certsDir, err)
	}

	adminKubeConfigClientCA, err := client.GetConfigMapData(ctx, "admin-kubeconfig-client-ca", "openshift-config", "ca-bundle.crt")
	if err != nil {
		return err
	}
	if err := os.WriteFile(common.PathOutsideChroot(filepath.Join(certsDir, "admin-kubeconfig-client-ca.crt")), []byte(adminKubeConfigClientCA), 0o644); err != nil {
		return err
	}

	for _, cert := range common.CertPrefixes {
		servingSignerKey, err := client.GetSecretData(ctx, cert, "openshift-kube-apiserver-operator", "tls.key")
		if err != nil {
			return err
		}
		if err := os.WriteFile(common.PathOutsideChroot(path.Join(certsDir, cert+".key")), []byte(servingSignerKey), 0o644); err != nil {
			return err
		}
	}

	ingressOperatorKey, err := client.GetSecretData(ctx, "router-ca", "openshift-ingress-operator", "tls.key")
	if err != nil {
		return err
	}
	if err := os.WriteFile(common.PathOutsideChroot(filepath.Join(certsDir, "ingresskey-ingress-operator.key")), []byte(ingressOperatorKey), 0o644); err != nil {
		return err
	}

	return nil
}
