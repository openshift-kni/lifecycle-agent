package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigpools,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=list;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list;watch

var (
	pollInterval = 30 * time.Second
	pollTimeout  = 15 * time.Minute
)

const (
	NodeRoleControlPlane = "node-role.kubernetes.io/control-plane"
	NodeRoleMaster       = "node-role.kubernetes.io/master"
	NodeRoleWorker       = "node-role.kubernetes.io/worker"
)

func HealthChecks(c client.Reader, l logr.Logger) error {
	defer common.FuncTimer(time.Now(), "healthCheck", l)

	// using channel store go routine return val and WaitGroup to sync.
	chanBufferSize := 5
	errChn := make(chan error, chanBufferSize)
	var wg sync.WaitGroup

	// prep and launch routines
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer common.FuncTimer(time.Now(), "clusterOperatorsReady", l)
		errChn <- clusterOperatorsReady(c, l)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer common.FuncTimer(time.Now(), "machineConfigPoolReady", l)
		errChn <- machineConfigPoolReady(c, l)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer common.FuncTimer(time.Now(), "clusterServiceVersionReady", l)
		errChn <- clusterServiceVersionReady(c, l)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer common.FuncTimer(time.Now(), "clusterVersionReady", l)
		errChn <- clusterVersionReady(c, l)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer common.FuncTimer(time.Now(), "nodesReady", l)
		errChn <- nodesReady(c, l)
	}()

	// wait
	go func() {
		l.Info("Wait until WaitGroup is done")
		defer close(errChn)
		wg.Wait()
	}()

	var finalErrs error
	for err := range errChn {
		if err != nil {
			finalErrs = errors.Join(finalErrs, err)
		}
	}

	l.Info("Health checks done")
	return finalErrs
}

func clusterServiceVersionReady(c client.Reader, l logr.Logger) error {
	l.Info("Waiting for all ClusterServiceVersion (csv) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isClusterServiceVersionReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isClusterServiceVersionReady(c client.Reader, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		clusterServiceVersionList := operatorsv1alpha1.ClusterServiceVersionList{}
		err := c.List(context.Background(), &clusterServiceVersionList)
		if err != nil {
			l.Error(err, "failed to get csv list")
			return false, nil
		}

		for _, csv := range clusterServiceVersionList.Items {
			if strings.Contains(csv.Name, "lifecycle-agent") {
				l.Info(fmt.Sprintf("Skipping check of %s/%s", csv.Kind, csv.Name))
				continue
			}
			if !(csv.Status.Phase == operatorsv1alpha1.CSVPhaseSucceeded && csv.Status.Reason == operatorsv1alpha1.CSVReasonInstallSuccessful) {
				l.Info(fmt.Sprintf("%s not ready yet", csv.Name), "kind", csv.Kind)
				return false, nil
			}
		}

		l.Info("All CSVs are ready")
		return true, nil
	}
}

func clusterVersionReady(c client.Reader, l logr.Logger) error {
	l.Info("Waiting for ClusterVersion to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isClusterVersionReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isClusterVersionReady(c client.Reader, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		clusterVersionList := configv1.ClusterVersionList{}
		err := c.List(context.Background(), &clusterVersionList)
		if err != nil {
			l.Error(err, "failed to get cv list")
			return false, nil
		}

		for _, co := range clusterVersionList.Items {
			if !getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorAvailable) {
				l.Info(fmt.Sprintf("%s not ready yet", co.Name), "kind", co.Kind)
				return false, nil
			}
		}

		l.Info("Cluster version is ready")
		return true, nil
	}

}

func machineConfigPoolReady(c client.Reader, l logr.Logger) error {
	l.Info("Waiting for MachineConfigPool (mcp) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isMachineConfigPoolReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isMachineConfigPoolReady(c client.Reader, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		machineConfigPoolList := mcv1.MachineConfigPoolList{}
		err := c.List(context.Background(), &machineConfigPoolList)
		if err != nil {
			l.Error(err, "failed to get mcp list")
			return false, nil
		}

		for _, mcp := range machineConfigPoolList.Items {
			if mcp.Status.MachineCount != mcp.Status.ReadyMachineCount {
				l.Info(fmt.Sprintf("%s not ready yet", mcp.Name), "kind", mcp.Kind)
				return false, nil
			}
		}

		l.Info("MachineConfigPool ready")
		return true, nil
	}
}

func clusterOperatorsReady(c client.Reader, l logr.Logger) error {
	l.Info("Waiting for all ClusterOperator (co) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, areClusterOperatorsReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func areClusterOperatorsReady(c client.Reader, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		clusterOperatorList := configv1.ClusterOperatorList{}
		err := c.List(context.Background(), &clusterOperatorList)
		if err != nil {
			l.Error(err, "failed to get co list")
			return false, nil
		}

		for _, co := range clusterOperatorList.Items {
			if !getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorAvailable) {
				l.Info(fmt.Sprintf("%s not ready yet", co.Name), "kind", co.Kind)
				return false, nil
			}

			if getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorProgressing) {
				l.Info(fmt.Sprintf("%s is in progressing state", co.Name), "kind", co.Kind)
				return false, nil
			}

			if getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorDegraded) {
				l.Info(fmt.Sprintf("%s is in degraded state", co.Name), "kind", co.Kind)
				return false, nil
			}
		}

		l.Info("All cluster operators are now ready")
		return true, nil
	}
}

func getClusterOperatorStatusCondition(conditions []configv1.ClusterOperatorStatusCondition, conditionType configv1.ClusterStatusConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == configv1.ConditionTrue
		}
	}

	return false
}

func nodesReady(c client.Reader, l logr.Logger) error {
	l.Info("Waiting for Node to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isNodeReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isNodeReady(c client.Reader, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		infra := &configv1.Infrastructure{}
		if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
			l.Error(err, "failed to get infrastucture CR")
			return false, nil
		}

		if infra.Status.InfrastructureTopology != configv1.SingleReplicaTopologyMode {
			// This is likely a test environment, so skip the health check.
			l.Info(fmt.Sprintf("Skipping Node check. InfrastructureTopology is %s. Expected %s", infra.Status.InfrastructureTopology, configv1.SingleReplicaTopologyMode))
			return true, nil
		}

		nodeList := corev1.NodeList{}
		err := c.List(context.Background(), &nodeList)
		if err != nil {
			l.Error(err, "failed to get node list")
			return false, nil
		}

		for _, node := range nodeList.Items {
			if !getNodeStatusCondition(node.Status.Conditions, corev1.NodeReady) {
				l.Info(fmt.Sprintf("%s not ready yet", node.Name), "kind", node.Kind)
				return false, nil
			}

			if getNodeStatusCondition(node.Status.Conditions, corev1.NodeNetworkUnavailable) {
				l.Info(fmt.Sprintf("%s NodeNetworkUnavailable", node.Name), "kind", node.Kind)
				return false, nil
			}

			// Verify the node has the expected node-role labels for SNO
			labels := node.ObjectMeta.GetLabels()
			requiredLabels := []string{NodeRoleControlPlane, NodeRoleMaster, NodeRoleWorker}
			for _, label := range requiredLabels {
				if _, found := labels[label]; !found {
					l.Info(fmt.Sprintf("%s does not have %s label", node.Name, label), "kind", node.Kind)
					return false, nil
				}
			}
		}

		l.Info("Node is ready")
		return true, nil
	}
}

func getNodeStatusCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
