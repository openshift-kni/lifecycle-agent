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
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"

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

// Client is a handle for interacting with a rpm-ostree based system.
type Client struct {
	clientid string
	ops      ops.Ops
}

// NewClient creates a new rpm-ostree client.  The client identifier should be a short, unique and ideally machine-readable string.
// This could be as simple as `examplecorp-management-agent`.
// If you want to be more verbose, you could use a URL, e.g. `https://gitlab.com/examplecorp/management-agent`.
func NewClient(id string, ops ops.Ops) *Client {
	return &Client{
		clientid: id,
		ops:      ops,
	}
}

func (c *Client) newCmd(args ...string) []byte {
	rawOutput, _ := c.ops.RunInHostNamespace("rpm-ostree", args...)
	return []byte(rawOutput)
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

// RpmOstreeVersion returns the running rpm-ostree version number
func (c *Client) RpmOstreeVersion() (*VersionData, error) {
	buf := c.newCmd("--version")

	var q rpmOstreeVersionData

	if err := yaml.Unmarshal(buf, &q); err != nil {
		return nil, fmt.Errorf("failed to parse `rpm-ostree --version` output: %w", err)
	}

	return &q.Root, nil
}

// QueryStatus loads the current system state.
func (c *Client) QueryStatus() (*Status, error) {
	var q Status
	buf := c.newCmd("status", "--json")

	if err := json.Unmarshal(buf, &q); err != nil {
		return nil, fmt.Errorf("failed to parse `rpm-ostree status --json` output: %w", err)
	}

	return &q, nil
}

// TODO: replace with https://github.com/coreos/rpmostree-client-go
//       when https://github.com/coreos/rpmostree-client-go/issues/23 resolves

// QueryStatusChroot uses chroot to loads the current system state
func QueryStatusChroot(rootPath string) (*Status, error) {
	cmd := exec.Command("/usr/bin/env", "--", "bash", "-c", "rpm-ostree status --json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: rootPath}
	var stdoutBytes, stderrBytes bytes.Buffer
	cmd.Stdout = &stdoutBytes
	cmd.Stderr = &stderrBytes
	cmd.Dir = "/"
	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", stderrBytes.String(), err)
	}
	var q Status
	if err := json.Unmarshal(stdoutBytes.Bytes(), &q); err != nil {
		return nil, fmt.Errorf("failed to parse `rpm-ostree status --json` output: %w", err)
	}
	return &q, nil
}

// SetDefaultChroot uses chroot to run the set-default command.
func SetDefaultChroot(rootPath string, deploymentID int) error {
	// TEMPORARY: Until the set-default feature is available in the platform, access a newer ostree cli from a container image:
	// This test-ostree-cli image is the rhel-coreos image with the ostree rpms updated to a newer version with the "admin set-default" feature.
	// Once the new feature is available, this function can be updated/replaced
	remountCmd := "mount -o remount,rw /sysroot && mount /boot -o remount,rw"
	ostreeCliContainerCmd := "podman run --rm --privileged -v /sysroot:/sysroot -v /boot:/boot -v /ostree:/ostree quay.io/openshift-kni/telco-ran-tools:test-ostree-cli"
	setDefaultCmd := fmt.Sprintf("%s && %s ostree admin set-default %d", remountCmd, ostreeCliContainerCmd, deploymentID)

	cmd := exec.Command("/usr/bin/env", "--", "bash", "-c", setDefaultCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: rootPath}
	var stdoutBytes, stderrBytes bytes.Buffer
	cmd.Stdout = &stdoutBytes
	cmd.Stderr = &stderrBytes
	cmd.Dir = "/"
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%s: %w", stderrBytes.String(), err)
	}
	return nil
}

// GetDeploymentIDForStaterootChroot uses chroot to run the ostree status check and find the deployment id for the given stateroot.
func GetDeploymentIDForStaterootChroot(rootPath, stateroot string) (deploymentID int, err error) {
	status, err := QueryStatusChroot(rootPath)
	if err != nil {
		return
	}

	for index, deployment := range status.Deployments {
		if deployment.OSName == stateroot {
			deploymentID = index
			return
		}
	}

	err = fmt.Errorf("Unable to find deployment for stateroot: %s", stateroot)
	return
}

// PivotToStaterootChroot uses chroot to set a new default ostree deployment.
func PivotToStaterootChroot(rootPath, stateroot string) (err error) {
	deploymentID, err := GetDeploymentIDForStaterootChroot(rootPath, stateroot)
	if err != nil {
		return
	}

	err = SetDefaultChroot(rootPath, deploymentID)
	return
}
