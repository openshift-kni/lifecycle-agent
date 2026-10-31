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

package ipconfigcmd

import (
	"fmt"
	"os"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	runtimeClient "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	dnsIPFamily string

	// primaryIPFile is the file inferPrimaryStack reads the node's current primary
	// IP from. It is a variable so the read can be redirected without a node.
	primaryIPFile = PrimaryIPPath

	// kubeconfigPath is the node kubeconfig used to build the Kubernetes client.
	// The chroot prefix is resolved once here so the call site stays a plain read.
	kubeconfigPath = common.PathOutsideChroot(common.KubeconfigFile)
)

const (
	sysrootPath     = "/sysroot"
	PrimaryIPPath   = "/run/nodeip-configuration/primary-ip"
	dnsIPFamilyFlag = "dns-ip-family"
)

// buildOpsAndK8sClient creates the host executor, ops interface, and Kubernetes client
// using the provided scheme. This logic is shared between the ip-config pre-pivot and post-pivot flows.
func buildOpsAndK8sClient(
	scheme *runtime.Scheme,
) (ops.Ops, ops.Execute, runtimeClient.Client, error) {
	var hostCommandsExecutor ops.Execute
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(pkgLog, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(pkgLog, true)
	}
	opsInterface := ops.NewOps(pkgLog, hostCommandsExecutor)

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create k8s config: %w", err)
	}

	client, err := runtimeClient.New(k8sConfig, runtimeClient.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create runtime client: %w", err)
	}

	return opsInterface, hostCommandsExecutor, client, nil
}
