package healthcheck

import (
	"context"
	"fmt"
	"strings"

	k8sv1 "k8s.io/api/certificates/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sriovv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	configv1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigpools,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=list;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=sriovnetwork.openshift.io,resources=sriovnetworknodestates,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch

const (
	NodeRoleControlPlane = "node-role.kubernetes.io/control-plane"
	NodeRoleMaster       = "node-role.kubernetes.io/master"
	NodeRoleWorker       = "node-role.kubernetes.io/worker"

	SriovNetworkNodeStateNotPresentMsg = "no SriovNetworkNodeStates present"
)

func HealthChecks(ctx context.Context, c client.Reader, l logr.Logger) error {
	var failures []string

	clusterOperatorsReady := false
	clusterServiceVersionsReady := false

	if err := AreClusterOperatorsReady(ctx, c, l); err != nil {
		l.Info("co health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	} else {
		clusterOperatorsReady = true
	}

	if err := AreMachineConfigPoolsReady(ctx, c, l); err != nil {
		l.Info("mcp health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	}

	if err := IsNodeReady(ctx, c, l); err != nil {
		l.Info("node health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	}

	if err := AreClusterServiceVersionsReady(ctx, c, l); err != nil {
		l.Info("csv health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	} else {
		clusterServiceVersionsReady = true
	}

	if err := IsClusterVersionReady(ctx, c, l); err != nil {
		l.Info("clusterVersion health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	}

	if err := AreCertificateSigningRequestsReady(ctx, c, l); err != nil {
		l.Info("certificateSigningRequest (csr) health check failure", "error", err.Error())
		failures = append(failures, err.Error())
	}

	if clusterOperatorsReady && clusterServiceVersionsReady {
		// Only check SriovNetworkNodeState once cluster operators and CSVs are stable
		if err := IsSriovNetworkNodeReady(ctx, c, l); err != nil {
			l.Info("sriovNetworkNodeState health check failure", "error", err.Error())
			failures = append(failures, err.Error())
		}

		// Check oadp storage backend connection when DPA exists and is reconciled
		if ok, err := IsDataProtectionApplicationReconciled(ctx, c, l); err != nil {
			l.Info("dataProtentionApplication health check failure", "error", err.Error())
			failures = append(failures, err.Error())
		} else if ok {
			if err := AreBackupStorageLocationsAvailable(ctx, c, l); err != nil {
				l.Info("backupStorageLocation health check failure", "error", err.Error())
				failures = append(failures, err.Error())
			}
		}
	}

	if len(failures) > 0 {
		l.Info("One or more health checks failed")
		// nolint: staticcheck
		return fmt.Errorf("one or more health checks failed: %s", strings.Join(failures, "\n  - "))
	}

	l.Info("Health checks done")
	return nil
}

func IsDataProtectionApplicationReconciled(ctx context.Context, c client.Reader, l logr.Logger) (bool, error) {
	dpaCRD := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: "dataprotectionapplications.oadp.openshift.io"}, dpaCRD); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check DataProtectionApplication CRD: %w", err)
	}

	dpaList := &unstructured.UnstructuredList{}
	dpaList.SetGroupVersionKind(backuprestore.DpaGvkList)
	opts := []client.ListOption{
		client.InNamespace(backuprestore.OadpNs),
	}
	if err := c.List(ctx, dpaList, opts...); err != nil {
		return false, fmt.Errorf("failed to list OADP DataProtectionApplication: %w", err)
	}

	if len(dpaList.Items) == 0 {
		return false, nil
	}

	for _, dpa := range dpaList.Items {
		if backuprestore.IsDPAReconciled(&dpa) { //nolint:gosec
			l.Info(fmt.Sprintf("DataProtectionApplication %s is reconciled", dpa.GetName()))
			return true, nil
		}
	}

	msg := "dataProtectionApplication not reconciled"
	l.Info(msg)
	// nolint: staticcheck
	return false, fmt.Errorf("dataProtectionApplication not reconciled")
}

func AreBackupStorageLocationsAvailable(ctx context.Context, c client.Reader, l logr.Logger) error {
	backupStorageLocationList := &velerov1.BackupStorageLocationList{}
	err := c.List(ctx, backupStorageLocationList, client.InNamespace(backuprestore.OadpNs))
	if err != nil {
		return fmt.Errorf("failed to list BackupStorageLocations: %w", err)
	}
	if len(backupStorageLocationList.Items) == 0 {
		msg := "backupsStorageLocations not yet created"
		l.Info(msg)
		// nolint: staticcheck
		return fmt.Errorf("backupsStorageLocations not yet created")
	}

	for _, bsl := range backupStorageLocationList.Items {
		if bsl.Status.Phase != velerov1.BackupStorageLocationPhaseAvailable {
			msg := fmt.Sprintf("backupStorageLocation %s not available", bsl.Name)
			l.Info(msg)
			// nolint: staticcheck
			return fmt.Errorf("backupStorageLocation %s not available", bsl.Name)
		}
	}

	l.Info("All BackupStorageLocations are available")
	return nil
}

func AreClusterServiceVersionsReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	clusterServiceVersionList := operatorsv1alpha1.ClusterServiceVersionList{}
	err := c.List(ctx, &clusterServiceVersionList)
	if err != nil {
		return fmt.Errorf("failed to get csv list: %w", err)
	}

	var notready []string
	for _, csv := range clusterServiceVersionList.Items {
		if strings.Contains(csv.Name, "lifecycle-agent") {
			l.Info(fmt.Sprintf("Skipping check of %s/%s", csv.Kind, csv.Name))
			continue
		}
		if csv.Status.Phase != operatorsv1alpha1.CSVPhaseSucceeded {
			notready = append(notready, csv.Name)
			l.Info(fmt.Sprintf("csv not ready: %s", csv.Name))
		}
	}

	if len(notready) != 0 {
		return fmt.Errorf("one or more ClusterServiceVersions not yet ready: %s", strings.Join(notready, ", "))
	}

	l.Info("All CSVs are ready")
	return nil
}

func IsClusterVersionReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	clusterVersionList := configv1.ClusterVersionList{}
	err := c.List(ctx, &clusterVersionList)
	if err != nil {
		return fmt.Errorf("failed to get cv list: %w", err)
	}

	// As we would only have one ClusterVersion currently, we don't need to build a list of not-ready CVs.
	// Instead, we can return on first error.
	for _, co := range clusterVersionList.Items {
		if !getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorAvailable) {
			msg := fmt.Sprintf("clusterVersion %s not ready", co.Name)
			l.Info(msg)
			// nolint: staticcheck
			return fmt.Errorf("clusterVersion %s not ready", co.Name)
		}
	}

	l.Info("Cluster version is ready")
	return nil
}

func AreMachineConfigPoolsReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	machineConfigPoolList := mcv1.MachineConfigPoolList{}
	err := c.List(ctx, &machineConfigPoolList)
	if err != nil {
		return fmt.Errorf("failed to get mcp list: %w", err)
	}

	var notready []string
	for _, mcp := range machineConfigPoolList.Items {
		if mcp.Status.MachineCount != mcp.Status.ReadyMachineCount {
			notready = append(notready, mcp.Name)
			l.Info(fmt.Sprintf("mcp not ready: %s", mcp.Name))
		}
	}

	if len(notready) != 0 {
		return fmt.Errorf("one or more MachineConfigPools not yet ready: %s", strings.Join(notready, ", "))
	}

	l.Info("MachineConfigPool ready")
	return nil
}

func AreClusterOperatorsReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	clusterOperatorList := configv1.ClusterOperatorList{}
	err := c.List(ctx, &clusterOperatorList)
	if err != nil {
		return fmt.Errorf("failed to get co list: %w", err)
	}

	var notready []string
	for _, co := range clusterOperatorList.Items {
		// nolint: gocritic
		if !getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorAvailable) {
			notready = append(notready, co.Name)
			l.Info(fmt.Sprintf("co not ready: %s", co.Name))
		} else if getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorProgressing) {
			notready = append(notready, co.Name)
			l.Info(fmt.Sprintf("co is in progressing state: %s", co.Name))
		} else if getClusterOperatorStatusCondition(co.Status.Conditions, configv1.OperatorDegraded) {
			notready = append(notready, co.Name)
			l.Info(fmt.Sprintf("co is in degraded state: %s", co.Name))
		}
	}

	if len(notready) != 0 {
		return fmt.Errorf("one or more ClusterOperators not yet ready: %s", strings.Join(notready, ", "))
	}

	l.Info("All cluster operators are now ready")
	return nil
}

func getClusterOperatorStatusCondition(conditions []configv1.ClusterOperatorStatusCondition, conditionType configv1.ClusterStatusConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == configv1.ConditionTrue
		}
	}

	return false
}

func IsNodeReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	infra := &configv1.Infrastructure{}
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
		return fmt.Errorf("failed to get infrastucture CR: %w", err)
	}

	if infra.Status.InfrastructureTopology != configv1.SingleReplicaTopologyMode {
		// This is likely a test environment, so skip the health check.
		l.Info(fmt.Sprintf("Skipping Node check. InfrastructureTopology is %s. Expected %s", infra.Status.InfrastructureTopology, configv1.SingleReplicaTopologyMode))
		return nil
	}

	nodeList := corev1.NodeList{}
	err := c.List(ctx, &nodeList)
	if err != nil {
		return fmt.Errorf("failed to get node list: %w", err)
	}

	// As we would only have one node currently, we don't need to build a list of not-ready nodes.
	// Instead, we can return on first error.
	for _, node := range nodeList.Items {
		if !getNodeStatusCondition(node.Status.Conditions, corev1.NodeReady) {
			msg := fmt.Sprintf("node is not yet ready: %s", node.Name)
			l.Info(msg)
			// nolint: staticcheck
			return fmt.Errorf("node is not yet ready: %s", node.Name)
		}

		if getNodeStatusCondition(node.Status.Conditions, corev1.NodeNetworkUnavailable) {
			msg := fmt.Sprintf("node network unavailable: %s", node.Name)
			l.Info(msg)
			// nolint: staticcheck
			return fmt.Errorf("node network unavailable: %s", node.Name)
		}

		// Verify the node has the expected node-role labels for SNO
		labels := node.ObjectMeta.GetLabels()
		requiredLabels := []string{NodeRoleControlPlane, NodeRoleMaster, NodeRoleWorker}
		for _, label := range requiredLabels {
			if _, found := labels[label]; !found {
				msg := fmt.Sprintf("node missing %s label: %s", label, node.Name)
				l.Info(msg)
				// nolint: staticcheck
				return fmt.Errorf("node missing %s label: %s", label, node.Name)
			}
		}
	}

	l.Info("Node is ready")
	return nil
}

func getNodeStatusCondition(conditions []corev1.NodeCondition, conditionType corev1.NodeConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func IsSriovNetworkNodeReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	// Check the CRD first. If it doesn't exist, pass the health check
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: "sriovnetworknodestates.sriovnetwork.openshift.io"}, crd); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to verify sriov CRD exists: %w", err)
	}

	nodeStates := &sriovv1.SriovNetworkNodeStateList{}

	if err := c.List(ctx, nodeStates); err != nil {
		return fmt.Errorf("failed to get SriovNetworkNodeStates: %w", err)
	}

	if len(nodeStates.Items) == 0 {
		// nolint: staticcheck
		return fmt.Errorf(SriovNetworkNodeStateNotPresentMsg)
	}

	for _, nodeState := range nodeStates.Items {
		if nodeState.Status.SyncStatus != "Succeeded" {
			return fmt.Errorf("sriovNetworkNodeState not ready")
		}
	}

	l.Info("SriovNetworkNodeState is ready")
	return nil
}

func AreCertificateSigningRequestsReady(ctx context.Context, c client.Reader, l logr.Logger) error {
	csrList := k8sv1.CertificateSigningRequestList{}
	if err := c.List(ctx, &csrList); err != nil {
		return fmt.Errorf("failed to get csr list: %w", err)
	}

	for _, csr := range csrList.Items {
		if !isApproved(csr) {
			return fmt.Errorf("certificateSigningRequest (csr) %s not yet approved", csr.GetName())
		}
	}

	l.Info("All CSRs are approved")
	return nil
}

func isApproved(csr k8sv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == k8sv1.CertificateApproved {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
