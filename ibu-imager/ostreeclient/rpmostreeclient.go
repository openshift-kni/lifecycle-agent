/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This client lifts code from: https://github.com/coreos/rpmostree-client-go/blob/main/pkg/client/client.go

package rpmostreeclient

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
)

// Status summarizes the current worldview of the rpm-ostree daemon.
// The deployment list is the primary data.
type Status struct {
	// Deployments is the list of bootable filesystem trees.
	Deployments []Deployment
	// Transaction is the active transaction, if any.
	Transaction *[]string
}

// Deployment represents a bootable filesystem tree
type Deployment struct {
	ID                      string   `json:"id"`
	OSName                  string   `json:"osname"`
	Serial                  int32    `json:"serial"`
	BaseChecksum            *string  `json:"base-checksum"`
	Checksum                string   `json:"checksum"`
	Version                 string   `json:"version"`
	Timestamp               uint64   `json:"timestamp"`
	Booted                  bool     `json:"booted"`
	Staged                  bool     `json:"staged"`
	LiveReplaced            string   `json:"live-replaced,omitempty"`
	Origin                  string   `json:"origin"`
	CustomOrigin            []string `json:"custom-origin"`
	ContainerImageReference string   `json:"container-image-reference"`
	RequestedPackages       []string `json:"requested-packages"`
	RequestedBaseRemovals   []string `json:"requested-base-removals"`
	Unlocked                *string  `json:"unlocked"`
}

//go:generate mockgen -source=rpmostreeclient.go -package=rpmostreeclient -destination=mock_rpmostreeclient.go
type IClient interface {
	newCmd(args ...string) ([]byte, error)
	RpmOstreeVersion() (*VersionData, error)
	QueryStatus() (*Status, error)
	IsStaterootBooted(stateroot string) (bool, error)
	GetCurrentStaterootName() (string, error)
	GetUnbootedStaterootName() (string, error)
	GetDeploymentID(osname string) (string, error)
	GetDeploymentIndex(osname string) (int, error)
	GetUnbootedDeploymentIndex() (int, error)
	RpmOstreeCleanup() error
}

// Client is a handle for interacting with a rpm-ostree based system.
type Client struct {
	clientid string
	executor ops.Execute
}

// NewClient creates a new rpm-ostree client.  The client identifier should be a short, unique and ideally machine-readable string.
// This could be as simple as `examplecorp-management-agent`.
// If you want to be more verbose, you could use a URL, e.g. `https://gitlab.com/examplecorp/management-agent`.
func NewClient(id string, executor ops.Execute) *Client {
	return &Client{
		clientid: id,
		executor: executor,
	}
}

func (c *Client) newCmd(args ...string) ([]byte, error) {
	rawOutput, err := c.executor.Execute("rpm-ostree", args...)
	return []byte(rawOutput), err
}

// VersionData represents the static information about rpm-ostree.
type VersionData struct {
	Version  string   `yaml:"Version"`
	Features []string `yaml:"Features"`
	Git      string   `yaml:"Git"`
}

type rpmOstreeVersionData struct {
	Root VersionData `yaml:"rpm-ostree"`
}

func getBootedStaterootName(deployments []Deployment) (string, error) {
	for _, deployment := range deployments {
		if deployment.Booted {
			return deployment.OSName, nil
		}
	}
	return "", fmt.Errorf("unable to determine booted stateroot")
}

func getUnbootedStaterootName(deployments []Deployment) (string, error) {
	// Determine the booted stateroot
	bootedStateroot, err := getBootedStaterootName(deployments)
	if err != nil {
		return "", err
	}

	// Find the first deployment in the unbooted stateroot
	for _, deployment := range deployments {
		if deployment.OSName != bootedStateroot {
			return deployment.OSName, nil
		}
	}

	return "", fmt.Errorf("unable to find deployment in unbooted stateroot")
}

func getDeploymentIndex(deployments []Deployment, stateroot string) (int, error) {
	for index, deployment := range deployments {
		if deployment.OSName == stateroot {
			return index, nil
		}
	}
	return -1, fmt.Errorf("unable to find deployment in stateroot: %s", stateroot)
}

// RpmOstreeVersion returns the running rpm-ostree version number
func (c *Client) RpmOstreeVersion() (*VersionData, error) {
	buf, err := c.newCmd("--version")
	if err != nil {
		return nil, err
	}

	var q rpmOstreeVersionData

	if err = yaml.Unmarshal(buf, &q); err != nil {
		return nil, fmt.Errorf("failed to parse `rpm-ostree --version` output: %w", err)
	}

	return &q.Root, nil
}

// QueryStatus loads the current system state.
func (c *Client) QueryStatus() (*Status, error) {
	var q Status
	buf, err := c.newCmd("status", "--json")
	if err != nil {
		return nil, err
	}

	if err = json.Unmarshal(buf, &q); err != nil {
		return nil, fmt.Errorf("failed to parse `rpm-ostree status --json` output: %w", err)
	}

	return &q, nil
}

func (c *Client) GetDeploymentID(stateroot string) (string, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return "", err
	}

	for _, deployment := range status.Deployments {
		if deployment.OSName == stateroot {
			return deployment.ID, nil
		}
	}
	return "", fmt.Errorf("failed to find deployment with osname %s", stateroot)
}

func (c *Client) GetDeploymentIndex(stateroot string) (int, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return -1, err
	}

	return getDeploymentIndex(status.Deployments, stateroot)
}

func (c *Client) GetUnbootedDeploymentIndex() (int, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return -1, err
	}

	// Determine the booted stateroot
	unbootedStateroot, err := getUnbootedStaterootName(status.Deployments)
	if err != nil {
		return -1, err
	}

	// Find the first deployment in the unbooted stateroot
	return getDeploymentIndex(status.Deployments, unbootedStateroot)
}

// IsStaterootBooted returns whether the specified stateroot is booted
func (c *Client) IsStaterootBooted(stateroot string) (bool, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return false, err
	}

	for _, deployment := range status.Deployments {
		if deployment.Booted && deployment.OSName == stateroot {
			return true, nil
		}
	}
	return false, nil
}

// GetCurrentStaterootName returns current stateroot name (a.k.a OSName)
func (c *Client) GetCurrentStaterootName() (string, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return "", err
	}

	return getBootedStaterootName(status.Deployments)
}

// GetUnbootedStaterootName returns unbooted stateroot name (a.k.a OSName)
func (c *Client) GetUnbootedStaterootName() (string, error) {
	status, err := c.QueryStatus()
	if err != nil {
		return "", err
	}

	// Determine the booted stateroot
	return getUnbootedStaterootName(status.Deployments)
}

func (c *Client) RpmOstreeCleanup() error {
	_, err := c.newCmd("cleanup", "-b")
	return err
}
