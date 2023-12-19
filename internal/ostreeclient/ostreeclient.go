package ostreeclient

import (
	"fmt"
	"strconv"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
)

//go:generate mockgen -source=ostreeclient.go -package=ostreeclient -destination=mock_ostreeclient.go
type IClient interface {
	PullLocal(repoPath string) error
	OSInit(osname string) error
	Deploy(osname, refsepc string, kargs []string) error
	Undeploy(ostreeIndex int) error
	SetDefaultDeployment(index int) error
	IsOstreeAdminSetDefaultFeatureEnabled() bool
}

type Client struct {
	executor ops.Execute
}

func NewClient(executor ops.Execute) IClient {
	return &Client{
		executor: executor,
	}
}

func (c *Client) PullLocal(repoPath string) error {
	_, err := c.executor.Execute("ostree", "pull-local", repoPath)
	return err
}

func (c *Client) OSInit(osname string) error {
	_, err := c.executor.Execute("ostree", "admin", "os-init", osname)
	return err
}

func (c *Client) Deploy(osname, refsepc string, kargs []string) error {
	args := []string{"admin", "deploy", "--os", osname}
	args = append(args, kargs...)
	args = append(args, refsepc)
	if c.IsOstreeAdminSetDefaultFeatureEnabled() {
		args = append(args, "--not-as-default")
	}
	_, err := c.executor.Execute("ostree", args...)
	return err
}

func (c *Client) Undeploy(ostreeIndex int) error {
	_, err := c.executor.Execute("ostree", "admin", "undeploy", fmt.Sprint(ostreeIndex))
	return err
}

func (c *Client) IsOstreeAdminSetDefaultFeatureEnabled() bool {
	// Quick check to see if the "ostree admin set-default" feature is available
	args := []string{"admin --help | grep -q set-default"}
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
