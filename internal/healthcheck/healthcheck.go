package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	"github.com/go-logr/logr"
	v12 "github.com/openshift/api/config/v1"
	v1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigpools,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=list;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=list;watch

var (
	pollInterval = 3 * time.Second
	pollTimeout  = 15 * time.Minute
)

func HealthChecks(c client.Client, l logr.Logger) error {
	defer common.FuncTimer(time.Now(), "healthCheck", l)

	// using channel store go routine return val and WaitGroup to sync.
	chanBufferSize := 3
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

func clusterServiceVersionReady(c client.Client, l logr.Logger) error {
	l.Info("Waiting for all ClusterServiceVersion (csv) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isClusterServiceVersionReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isClusterServiceVersionReady(c client.Client, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		clusterServiceVersionList := v1alpha1.ClusterServiceVersionList{}
		err := c.List(context.Background(), &clusterServiceVersionList)
		if err != nil {
			return false, err
		}

		for _, csv := range clusterServiceVersionList.Items {
			if !(csv.Status.Phase == v1alpha1.CSVPhaseSucceeded && csv.Status.Reason == v1alpha1.CSVReasonInstallSuccessful) {
				l.Info(fmt.Sprintf("%s not ready yet", csv.Name), "kind", csv.Kind)
				return false, nil
			}
		}

		l.Info("All CSVs are ready")
		return true, nil
	}
}

func machineConfigPoolReady(c client.Client, l logr.Logger) error {
	l.Info("Waiting for MachineConfigPool (mcp) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, isMachineConfigPoolReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func isMachineConfigPoolReady(c client.Client, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		machineConfigPoolList := v1.MachineConfigPoolList{}
		err := c.List(context.Background(), &machineConfigPoolList)
		if err != nil {
			return false, err
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

func clusterOperatorsReady(c client.Client, l logr.Logger) error {
	l.Info("Waiting for all ClusterOperator (co) to be ready")
	err := wait.PollUntilContextTimeout(context.Background(), pollInterval, pollTimeout, true, areClusterOperatorsReady(c, l))
	if err != nil {
		return err
	}

	return nil
}

func areClusterOperatorsReady(c client.Client, l logr.Logger) wait.ConditionWithContextFunc {
	return func(ctx context.Context) (bool, error) {
		clusterOperatorList := v12.ClusterOperatorList{}
		err := c.List(context.Background(), &clusterOperatorList)
		if err != nil {
			return false, err
		}

		for _, co := range clusterOperatorList.Items {
			if !findCOStatusCondition(co.Status.Conditions, v12.OperatorAvailable) {
				l.Info(fmt.Sprintf("%s not ready yet", co.Name), "kind", co.Kind)
				return false, nil
			}
		}

		l.Info("All cluster operators are now ready")
		return true, nil
	}
}

func findCOStatusCondition(conditions []v12.ClusterOperatorStatusCondition, conditionType v12.ClusterStatusConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return true
		}
	}

	return false
}
