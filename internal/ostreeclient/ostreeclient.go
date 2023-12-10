package ostreeclient

import (
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
)

//go:generate mockgen -source=ostreeclient.go -package=ostreeclient -destination=mock_ostreeclient.go
type IClient interface {
	PullLocal(repoPath string) error
	OSInit(osname string) error
	Deploy(osname, refsepc string, kargs []string) error
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
	_, err := c.executor.Execute("ostree", args...)
	return err
}
