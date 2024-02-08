package ostreeclient

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

//go:generate mockgen -source=ostreeclient.go -package=ostreeclient -destination=mock_ostreeclient.go
type IClient interface {
	PullLocal(repoPath string) error
	OSInit(osname string) error
	Deploy(osname, refsepc string, kargs []string) error
	Undeploy(ostreeIndex int) error
	SetDefaultDeployment(index int) error
	IsOstreeAdminSetDefaultFeatureEnabled() bool
	GetDeployment(osname string) (string, error)
	GetDeploymentDir(osname string) (string, error)
}

type Client struct {
	executor ops.Execute
	ibi      bool
}

func NewClient(executor ops.Execute, ibi bool) IClient {
	return &Client{
		executor: executor,
		ibi:      ibi,
	}
}

func (c *Client) PullLocal(repoPath string) error {
	args := []string{"pull-local"}
	if c.ibi {
		args = append(args, "--repo", "/mnt/ostree/repo")
	}
	_, err := c.executor.Execute("ostree", append(args, repoPath)...)
	return err
}

func (c *Client) OSInit(osname string) error {
	args := []string{"admin", "os-init"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}

	_, err := c.executor.Execute("ostree", append(args, osname)...)
	return err
}

func (c *Client) Deploy(osname, refsepc string, kargs []string) error {
	args := []string{"admin", "deploy", "--os", osname, "--no-prune"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}
	args = append(args, kargs...)
	args = append(args, refsepc)
	if !c.ibi && c.IsOstreeAdminSetDefaultFeatureEnabled() {
		args = append(args, "--not-as-default")
	}

	// Run the command in bash to preserve the quoted kargs
	args = append([]string{"ostree"}, args...)
	_, err := c.executor.Execute("bash", "-c", strings.Join(args, " "))
	return err
}

func (c *Client) Undeploy(ostreeIndex int) error {
	args := []string{"admin", "undeploy"}
	if c.ibi {
		args = append(args, "--sysroot", "/mnt")
	}
	_, err := c.executor.Execute("ostree", append(args, fmt.Sprint(ostreeIndex))...)
	return err
}

func (c *Client) IsOstreeAdminSetDefaultFeatureEnabled() bool {
	// Quick check to see if the "ostree admin set-default" feature is available
	output, err := c.executor.Execute("ostree", "admin", "--help")
	if err != nil {
		return false
	}

	return strings.Contains(output, "set-default")
}

func (c *Client) SetDefaultDeployment(index int) error {
	if index == 0 {
		// Already set as default deployment
		return nil
	}

	args := []string{"admin", "set-default", strconv.Itoa(index)}
	_, err := c.executor.Execute("ostree", args...)
	return err
}

func (c *Client) GetDeployment(stateroot string) (string, error) {
	args := []string{"admin", "status"}
	if c.ibi {
		args = append(args, "--sysroot", common.OstreeDeployPathPrefix)
	}

	output, err := c.executor.Execute("ostree", args...)
	if err != nil {
		return "", fmt.Errorf("unable to get deployment, ostree command failed: %w", err)
	}

	// Example output:
	//   # ostree admin status
	//   * rhcos 9455b99374197f10c453eb96f1b66cea884b3dc16ce4bc753bdb7263602bb722.0
	//       origin: <unknown origin type>
	//     rhcos_4.15.0_rc.1 8ef186bc6407db2180726e32354c394c189c6e9be2c17839b313cf1fed3d5391.0
	//       origin: <unknown origin type>
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		if fields[0] == "*" {
			// Pop off the *, which indicates the currently booted deployment
			fields = fields[1:]
		}

		if len(fields) < 2 {
			continue
		}

		if fields[0] == stateroot {
			// Return the deployment for the first matching stateroot
			return fields[1], nil
		}
	}

	return "", nil
}

func (c *Client) GetDeploymentDir(stateroot string) (string, error) {
	deployment, err := c.GetDeployment(stateroot)
	if err != nil {
		return "", fmt.Errorf("unable to get determine deployment dir: %w", err)
	}

	deploymentDir := filepath.Join(common.GetStaterootPath(stateroot), "deploy", deployment)
	return deploymentDir, nil
}
