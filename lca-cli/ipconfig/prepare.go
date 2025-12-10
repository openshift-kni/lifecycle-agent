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

package ipconfig

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	intOstree "github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmOstree "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// OstreeData groups metadata about the old and new stateroots involved in the
// IP configuration prepare flow.
type OstreeData struct {
	OldStateroot *StaterootData
	NewStateroot *StaterootData
}

// StaterootData describes a stateroot's identity and deployment metadata.
type StaterootData struct {
	Name           string
	Path           string
	DeploymentDir  string
	DeploymentName string
}

// PrepareHandler coordinates preparing a new stateroot for IP configuration
// changes by deploying, copying state, and setting default deployment.
type PrepareHandler struct {
	log        *logrus.Logger
	ops        ops.Ops
	ostreeData *OstreeData
	ostree     intOstree.IClient
	rpm        rpmOstree.IClient
	reboot     reboot.RebootIntf
	k8s        runtimeclient.Client
}

// NewPrepareHandler creates a new PrepareHandler instance.
func NewPrepareHandler(
	log *logrus.Logger,
	ops ops.Ops,
	ostreeData *OstreeData,
	ostree intOstree.IClient,
	rpm rpmOstree.IClient,
	reboot reboot.RebootIntf,
	k8s runtimeclient.Client,
) *PrepareHandler {
	return &PrepareHandler{
		log:        log,
		ops:        ops,
		ostreeData: ostreeData,
		ostree:     ostree,
		rpm:        rpm,
		reboot:     reboot,
		k8s:        k8s,
	}
}

// Run executes the prepare sequence: stop services, deploy/prepare the new
// stateroot, and re-enable services in the appropriate roots.
func (p *PrepareHandler) Run(ctx context.Context) (err error) {
	p.log.Info("IP config prepare started")

	p.log.Info("Fetching current kernel args")
	kargs, err := utils.BuildKernelArgumentsFromMCOFile(common.MCDCurrentConfig)
	if err != nil {
		err = fmt.Errorf("failed to get current kernel args: %w", err)
		return
	}

	p.log.Info("Stopping cluster services")
	if err = p.ops.StopClusterServices(); err != nil {
		err = fmt.Errorf("failed to stop cluster services: %w", err)
		return
	}

	defer func() {
		p.log.Info("Enabling cluster services in old stateroot")
		if internalErr := p.ops.EnableClusterServices(""); internalErr != nil {
			if err == nil {
				err = internalErr
			} else {
				err = fmt.Errorf("%s; also failed to enable cluster services: %w", err.Error(), internalErr)
			}
		}
	}()

	p.log.Info("Preparing new stateroot")
	if err = p.prepareNewStateroot(p.ostreeData, kargs); err != nil {
		return fmt.Errorf("failed to prepare new stateroot: %w", err)
	}

	p.log.Info("Enabling cluster services in new stateroot")
	if err = p.ops.EnableClusterServices(
		p.ostreeData.NewStateroot.DeploymentDir,
	); err != nil {
		err = fmt.Errorf("failed to enable cluster services: %w", err)
		return
	}

	p.log.Info("IP config prepare done successfully")

	return nil
}

// prepareNewStateroot deploys the new stateroot if needed, copies data, and
// sets it as default (when supported).
func (p *PrepareHandler) prepareNewStateroot(
	ostreeData *OstreeData,
	kernelArgs []string,
) error {
	if err := p.ensureSysrootWritable(); err != nil {
		return fmt.Errorf("failed to ensure sysroot writable: %w", err)
	}

	if ostreeData.NewStateroot.DeploymentName == "" {
		p.log.Infof("New stateroot %s is not deployed, deploying it", ostreeData.NewStateroot.Name)

		bootedCommit, err := p.getBootedCommit()
		if err != nil {
			return fmt.Errorf("failed to get booted commit: %w", err)
		}

		if err = p.deployNewStateroot(
			ostreeData,
			lo.FromPtr(bootedCommit),
			kernelArgs,
		); err != nil {
			return fmt.Errorf("failed to deploy new stateroot: %w", err)
		}
	} else {
		p.log.Infof("New stateroot %s is already deployed, skipping deploy", ostreeData.NewStateroot.Name)
	}

	if err := p.copyStateRootData(ostreeData); err != nil {
		return fmt.Errorf("failed to copy state root data: %w", err)
	}

	if err := p.setDefaultDeploymentIfEnabled(ostreeData.NewStateroot.Name); err != nil {
		return fmt.Errorf("failed to set default deployment: %w", err)
	}

	return nil
}

func (p *PrepareHandler) ensureSysrootWritable() error {
	if err := p.ops.RemountSysroot(); err != nil {
		return fmt.Errorf("failed to remount /sysroot rw: %w", err)
	}
	return nil
}

// getBootedCommit returns the checksum of the currently booted rpm-ostree deployment.
func (p *PrepareHandler) getBootedCommit() (*string, error) {
	status, err := p.rpm.QueryStatus()
	if err != nil {
		return nil, fmt.Errorf("failed to query rpm-ostree status: %w", err)
	}

	var bootedCommit string
	for _, d := range status.Deployments {
		if d.Booted {
			bootedCommit = d.Checksum
			break
		}
	}

	if bootedCommit == "" {
		return nil, fmt.Errorf("failed to determine booted deployment commit")
	}

	return &bootedCommit, nil
}

// deployNewStateroot initializes OSTree for the new stateroot and deploys the
// booted commit with the provided kernel args, updating the ostreeData fields.
func (p *PrepareHandler) deployNewStateroot(
	ostreeData *OstreeData,
	bootedCommit string,
	kargs []string,
) error {
	common.OstreeDeployPathPrefix = "/sysroot"
	if err := p.ostree.OSInit(ostreeData.NewStateroot.Name); err != nil {
		return fmt.Errorf("failed to initialize ostree: %w", err)
	}

	if err := p.ostree.Deploy(ostreeData.NewStateroot.Name, bootedCommit, kargs, p.rpm, false); err != nil {
		return fmt.Errorf("failed ostree admin deploy: %w", err)
	}

	deploymentName, err := p.ostree.GetDeployment(ostreeData.NewStateroot.Name)
	if err != nil {
		return fmt.Errorf("failed to get deployment for %s: %w", ostreeData.NewStateroot.Name, err)
	}
	ostreeData.NewStateroot.DeploymentName = deploymentName

	deploymentDir, err := p.ostree.GetDeploymentDir(ostreeData.NewStateroot.Name)
	if err != nil {
		return fmt.Errorf("failed to get deployment dir for %s: %w", ostreeData.NewStateroot.Name, err)
	}
	ostreeData.NewStateroot.DeploymentDir = deploymentDir

	return nil
}

// copyStateRootData copies important state from the old stateroot to the new one.
func (p *PrepareHandler) copyStateRootData(ostreeData *OstreeData) error {
	if err := p.copyVar(
		ostreeData.OldStateroot.Path,
		ostreeData.NewStateroot.Path,
	); err != nil {
		return err
	}

	if err := p.copyEtc(
		ostreeData.OldStateroot.DeploymentDir,
		ostreeData.NewStateroot.DeploymentDir,
	); err != nil {
		return err
	}

	if err := p.copyDeploymentOrigin(
		ostreeData.OldStateroot.Path,
		ostreeData.NewStateroot.Path,
		ostreeData.OldStateroot.DeploymentName,
		ostreeData.NewStateroot.DeploymentName,
	); err != nil {
		return fmt.Errorf("failed to copy deployment origin: %w", err)
	}

	return nil
}

// setDefaultDeploymentIfEnabled sets the given stateroot as default when supported.
func (p *PrepareHandler) setDefaultDeploymentIfEnabled(newStateroot string) error {
	if !p.ostree.IsOstreeAdminSetDefaultFeatureEnabled() {
		return fmt.Errorf("ostree admin set default feature is not enabled")
	}

	idx, err := p.rpm.GetDeploymentIndex(newStateroot)
	if err != nil {
		return fmt.Errorf("failed to get deployment index for %s: %w", newStateroot, err)
	}

	if err := p.ostree.SetDefaultDeployment(idx); err != nil {
		return fmt.Errorf("failed to set default deployment: %w", err)
	}

	return nil
}

// copyVar copies the var directory preserving SELinux contexts and attributes
func (p *PrepareHandler) copyVar(oldSRPath, newSRPath string) error {
	// Copy var directory preserving SELinux contexts and attributes
	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -ar --preserve=context '%s/' '%s/'",
			filepath.Join(oldSRPath, "var"),
			newSRPath,
		),
	); err != nil {
		return fmt.Errorf("failed to copy var: %w", err)
	}
	return nil
}

// copyEtc copies the deployment's etc directory preserving SELinux contexts
func (p *PrepareHandler) copyEtc(oldDeploymentDir, newDeploymentDir string) error {
	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -ar --preserve=context '%s/' '%s/'",
			filepath.Join(oldDeploymentDir, "etc"),
			newDeploymentDir,
		),
	); err != nil {
		return fmt.Errorf("failed to copy etc: %w", err)
	}
	return nil
}

// copyOrigin copies the .origin file between deployments preserving SELinux context
func (p *PrepareHandler) copyDeploymentOrigin(oldSRPath, newSRPath, oldDeploymentName, newDeploymentName string) error {
	oldOriginPath := filepath.Join(oldSRPath, "deploy", fmt.Sprintf("%s.origin", oldDeploymentName))
	newOriginPath := filepath.Join(newSRPath, "deploy", fmt.Sprintf("%s.origin", newDeploymentName))

	if _, err := p.ops.RunInHostNamespace(
		"bash", "-c",
		fmt.Sprintf(
			"cp -a --preserve=context '%s' '%s'",
			oldOriginPath,
			newOriginPath,
		),
	); err != nil {
		return fmt.Errorf("failed to copy origin file: %w", err)
	}
	return nil
}
