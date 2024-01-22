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
	_, err := c.executor.Execute("ostree", args...)
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
	args := []string{"admin", "--help", "|", "grep", "-q", "set-default"}
	_, err := c.executor.Execute("ostree", args...)
	return err == nil
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

func (c *Client) GetDeployment(osname string) (string, error) {
	args := []string{"admin", "status"}
	if c.ibi {
		args = append(args, "--sysroot", common.OstreeDeployPathPrefix)
	}

	args = append(args, "|", "awk", fmt.Sprintf("/%s/'{print $NF ; exit}'", osname))
	return c.executor.Execute("ostree", strings.Join(args, " "))
}

func (c *Client) GetDeploymentDir(osname string) (string, error) {
	deployment, err := c.GetDeployment(osname)
	if err != nil {
		return "", err
	}

	deploymentDir := filepath.Join(common.GetStaterootPath(osname), "deploy", deployment)
	return deploymentDir, nil
}
