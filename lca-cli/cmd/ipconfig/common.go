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

// buildOpsAndClient creates the host executor, ops interface, and Kubernetes client
// using the provided scheme. This logic is shared between the ip-config prepare and run flows.
func buildOpsAndClient(
	scheme *runtime.Scheme,
) (ops.Ops, ops.Execute, runtimeClient.Client, error) {
	var hostCommandsExecutor ops.Execute
	if _, err := os.Stat(common.Host); err == nil {
		hostCommandsExecutor = ops.NewChrootExecutor(pkgLog, true, common.Host)
	} else {
		hostCommandsExecutor = ops.NewRegularExecutor(pkgLog, true)
	}
	opsInterface := ops.NewOps(pkgLog, hostCommandsExecutor)

	k8sConfig, err := clientcmd.BuildConfigFromFlags("", common.PathOutsideChroot(common.KubeconfigFile))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create k8s config: %w", err)
	}

	client, err := runtimeClient.New(k8sConfig, runtimeClient.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create runtime client: %w", err)
	}

	return opsInterface, hostCommandsExecutor, client, nil
}
