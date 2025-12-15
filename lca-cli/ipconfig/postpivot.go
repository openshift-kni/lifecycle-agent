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

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/recert"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

// IPConfigPostPivotHandler handles IP changes during the post-pivot phase.
type IPConfigPostPivotHandler struct {
	log          *logrus.Logger
	ops          ops.Ops
	recertImage  string
	client       runtimeclient.Client
	dnsIPFamily  string
	scheme       *runtime.Scheme
	kubeconfig   string
	workspaceDir string
}

// NewIPConfigPostPivotHandler creates a new IPConfigPostPivotHandler instance.
func NewIPConfigPostPivotHandler(
	log *logrus.Logger,
	ops ops.Ops,
	client runtimeclient.Client,
	recertImage string,
	dnsIPFamily string,
	scheme *runtime.Scheme,
	kubeconfig string,
	workspaceDir string,
) *IPConfigPostPivotHandler {
	return &IPConfigPostPivotHandler{
		log:          log,
		ops:          ops,
		recertImage:  recertImage,
		client:       client,
		dnsIPFamily:  dnsIPFamily,
		scheme:       scheme,
		kubeconfig:   kubeconfig,
		workspaceDir: workspaceDir,
	}
}

func (p *IPConfigPostPivotHandler) Run(ctx context.Context) error {
	p.log.Info("IP config post-pivot started")

	p.log.Info("Applying network configuration")
	if err := utils.RunOnce("apply-network-configuration", p.workspaceDir, p.log, p.applyNetworkConfiguration); err != nil {
		return fmt.Errorf("failed to run once apply-network-configuration: %w", err)
	}

	p.log.Info("Running recert")
	if err := utils.RunOnce("run-recert", p.workspaceDir, p.log, p.runRecert); err != nil {
		return fmt.Errorf("failed to run once run-recert: %w", err)
	}

	if err := p.enableAndStartKubelet(); err != nil {
		return fmt.Errorf("failed to enable and start kubelet: %w", err)
	}

	kubeconfigData, err := p.ops.ReadFile(p.kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to read kubeconfig %s: %w", p.kubeconfig, err)
	}

	client, err := utils.CreateKubeClientFromBytes(p.scheme, kubeconfigData)
	if err != nil {
		return fmt.Errorf("failed to create k8s client, err: %w", err)
	}

	utils.WaitForApi(ctx, client, p.log)

	if p.dnsIPFamily != "" {
		p.log.Info("Configuring dnsmasq override")
		if err := utils.RunOnce("set-dns-masq-filter", p.workspaceDir, p.log, common.SetDNSMasqFilterInMachineConfig,
			ctx,
			client,
			p.dnsIPFamily,
		); err != nil {
			return fmt.Errorf("failed to update dnsmasq filter in machine config: %w", err)
		}
	}

	if _, err = p.ops.SystemctlAction("disable", "ip-configuration.service"); err != nil {
		return fmt.Errorf("failed to disable ip-configuration.service, err: %w", err)
	}

	p.log.Info("IP config post-pivot completed successfully")

	return nil
}

// runRecert writes a recert configuration and executes the full recert flow
// to regenerate certificates and manifests for the new IP configuration.
func (p *IPConfigPostPivotHandler) runRecert() error {
	pullSecretPath := filepath.Join(p.workspaceDir, filepath.Base(common.IPConfigPullSecretFile))
	if err := p.ops.RecertFullFlow(
		p.recertImage,
		pullSecretPath,
		filepath.Join(p.workspaceDir, recert.RecertConfigFile),
		nil,
		nil,
		"-v", fmt.Sprintf("%s:%s", p.workspaceDir, p.workspaceDir),
	); err != nil {
		return fmt.Errorf("failed recert full flow: %w", err)
	}

	return nil
}

func (p *IPConfigPostPivotHandler) applyNetworkConfiguration() error {
	nmstateConfigFile := filepath.Join(p.workspaceDir, common.NmstateConfigFileName)
	if _, err := p.ops.RunInHostNamespace("nmstatectl", "apply", nmstateConfigFile); err != nil {
		return fmt.Errorf("failed to apply network configuration: %w", err)
	}

	return nil
}

func (p *IPConfigPostPivotHandler) enableAndStartKubelet() error {
	if _, err := p.ops.SystemctlAction("enable", "kubelet", "--now"); err != nil {
		return fmt.Errorf("failed to enable kubelet: %w", err)
	}
	return nil
}
