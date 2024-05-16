package healthcheck

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	sriovv1 "github.com/k8snetworkplumbingwg/sriov-network-operator/api/v1"
	backuprestore "github.com/openshift-kni/lifecycle-agent/internal/backuprestore"
	configv1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	k8sv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var s = scheme.Scheme

// let fakeclient know about all the possible types of CRs used by HCs
func init() {
	s.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterVersion{})
	s.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterVersionList{})
	s.AddKnownTypes(mcv1.GroupVersion, &mcv1.MachineConfigPoolList{})
	s.AddKnownTypes(mcv1.GroupVersion, &mcv1.MachineConfigPool{})
	s.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterOperatorList{})
	s.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterOperator{})
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.SubscriptionList{})
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.Subscription{})
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.ClusterServiceVersionList{})
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.ClusterServiceVersion{})
	s.AddKnownTypes(configv1.GroupVersion, &configv1.Infrastructure{})
	s.AddKnownTypes(v1.SchemeGroupVersion, &v1.Node{})
	s.AddKnownTypes(apiextensionsv1.SchemeGroupVersion, &apiextensionsv1.CustomResourceDefinitionList{})
	s.AddKnownTypes(apiextensionsv1.SchemeGroupVersion, &apiextensionsv1.CustomResourceDefinition{})
	s.AddKnownTypes(sriovv1.SchemeGroupVersion, &sriovv1.SriovNetworkNodeStateList{})
	s.AddKnownTypes(sriovv1.SchemeGroupVersion, &sriovv1.SriovNetworkNodeState{})
	s.AddKnownTypes(k8sv1.SchemeGroupVersion, &k8sv1.CertificateSigningRequest{})
	s.AddKnownTypes(k8sv1.SchemeGroupVersion, &k8sv1.CertificateSigningRequestList{})
	s.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.BackupStorageLocationList{})
	s.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.BackupStorageLocation{})
}

func Test_nodesReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
							NodeRoleWorker:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "node missing a label",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "missing Infrastructure",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "node not ready",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
							NodeRoleWorker:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionFalse,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "node network not ready",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
							NodeRoleWorker:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionTrue,
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := IsNodeReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("IsNodeReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_subscriptionReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path",
			objects: []runtime.Object{
				&operatorsv1alpha1.Subscription{
					Status: operatorsv1alpha1.SubscriptionStatus{
						Conditions: []operatorsv1alpha1.SubscriptionCondition{
							{
								Status: corev1.ConditionFalse,
								Type:   operatorsv1alpha1.SubscriptionCatalogSourcesUnhealthy,
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "fail with catalogsources unhealthy",
			objects: []runtime.Object{
				&operatorsv1alpha1.Subscription{
					Status: operatorsv1alpha1.SubscriptionStatus{
						Conditions: []operatorsv1alpha1.SubscriptionCondition{
							{
								Status: corev1.ConditionTrue,
								Type:   operatorsv1alpha1.SubscriptionCatalogSourcesUnhealthy,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fail with resolution failed",
			objects: []runtime.Object{
				&operatorsv1alpha1.Subscription{
					Status: operatorsv1alpha1.SubscriptionStatus{
						Conditions: []operatorsv1alpha1.SubscriptionCondition{
							{
								Status: corev1.ConditionFalse,
								Type:   operatorsv1alpha1.SubscriptionCatalogSourcesUnhealthy,
							},
							{
								Status: corev1.ConditionTrue,
								Type:   operatorsv1alpha1.SubscriptionResolutionFailed,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fail with no conditions set",
			objects: []runtime.Object{
				&operatorsv1alpha1.Subscription{
					Status: operatorsv1alpha1.SubscriptionStatus{
						Conditions: []operatorsv1alpha1.SubscriptionCondition{},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := AreSubscriptionsReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("AreSubscriptionReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterServiceVersionReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path with reason InstallSucceeded",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonInstallSuccessful,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "happy path with reason Copied",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonCopied,
					},
				},
			},
			wantErr: false,
		},
		{
			name: "fail with phase pending",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase: operatorsv1alpha1.CSVPhasePending,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fail with phase failed",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase: operatorsv1alpha1.CSVPhaseFailed,
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := AreClusterServiceVersionsReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("AreClusterServiceVersionReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterOperatorsReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path",
			objects: []runtime.Object{
				&configv1.ClusterOperator{
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorAvailable,
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "fail when degraded path",
			objects: []runtime.Object{
				&configv1.ClusterOperator{
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorAvailable,
							},
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorDegraded,
							},
						},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fail when progressing but timedotu",
			objects: []runtime.Object{
				&configv1.ClusterOperator{
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorAvailable,
							},
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorProgressing,
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := AreClusterOperatorsReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("AreClusterOperatorsReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_machineConfigPoolReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path",
			objects: []runtime.Object{
				&mcv1.MachineConfigPool{Status: mcv1.MachineConfigPoolStatus{
					MachineCount:      1,
					ReadyMachineCount: 1,
				}},
			},
			wantErr: false,
		},
		{
			name: "not exact count",
			objects: []runtime.Object{
				&mcv1.MachineConfigPool{Status: mcv1.MachineConfigPoolStatus{
					MachineCount:      1,
					ReadyMachineCount: 12,
				}},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := AreMachineConfigPoolsReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("AreMachineConfigPoolsReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterVersionReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}
	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path clusterVersion Ready",
			objects: []runtime.Object{
				&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:   configv1.OperatorAvailable,
								Status: configv1.ConditionTrue,
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "clusterVersion condition false",
			objects: []runtime.Object{
				&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:   configv1.OperatorAvailable,
								Status: configv1.ConditionFalse,
							},
						},
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := IsClusterVersionReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("IsClusterVersionReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_sriovNetworkNodeStateReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sriovnetworknodestates.sriovnetwork.openshift.io",
		},
	}

	successSriovNetworkNodeState := &sriovv1.SriovNetworkNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: sriovv1.SriovNetworkNodeStateStatus{
			SyncStatus: "Succeeded",
		},
	}

	failedSriovNetworkNodeState := &sriovv1.SriovNetworkNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: sriovv1.SriovNetworkNodeStateStatus{
			SyncStatus: "",
		},
	}

	tests := []struct {
		name      string
		args      args
		objects   []runtime.Object
		wantErr   bool
		wantedErr string
	}{
		{
			name: "happy path",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				crd,
				&sriovv1.SriovNetworkNodeStateList{
					Items: []sriovv1.SriovNetworkNodeState{*successSriovNetworkNodeState},
				},
			},
			wantErr: false,
		},
		{
			name: "sriov network node state not present",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				crd,
				&sriovv1.SriovNetworkNodeStateList{
					Items: []sriovv1.SriovNetworkNodeState{},
				},
			},
			wantErr:   true,
			wantedErr: SriovNetworkNodeStateNotPresentMsg,
		},
		{
			name: "sriov network node state not ready",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				crd,
				&sriovv1.SriovNetworkNodeStateList{
					Items: []sriovv1.SriovNetworkNodeState{*failedSriovNetworkNodeState},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := IsSriovNetworkNodeReady(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("IsSriovNetworkNodeReady() error = %v, wantErr %v", err, tt.wantErr)
			} else if tt.wantErr && tt.wantedErr != "" && !strings.Contains(err.Error(), tt.wantedErr) {
				// Check for specific expected error message
				t.Errorf("IsSriovNetworkNodeReady() error = %v, wantedErr %s", err, tt.wantedErr)
			}
		})
	}
}

func Test_DataProtectionApplicationReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dataprotectionapplications.oadp.openshift.io",
		},
	}

	successDpa := &unstructured.Unstructured{
		Object: map[string]any{
			"kind":       backuprestore.DpaGvk.Kind,
			"apiVersion": backuprestore.DpaGvk.Group + "/" + backuprestore.DpaGvk.Version,
			"metadata": map[string]any{
				"name":      "oadp",
				"namespace": backuprestore.OadpNs,
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "Reconciled",
						"status": "True",
						"reason": "Complete",
					},
				},
			},
		},
	}
	failedDpa := &unstructured.Unstructured{
		Object: map[string]any{
			"kind":       backuprestore.DpaGvk.Kind,
			"apiVersion": backuprestore.DpaGvk.Group + "/" + backuprestore.DpaGvk.Version,
			"metadata": map[string]any{
				"name":      "oadp",
				"namespace": backuprestore.OadpNs,
			},
			"status": map[string]any{
				"conditions": []any{
					map[string]any{
						"type":   "Reconciled",
						"status": "False",
						"reason": "Error",
					},
				},
			},
		},
	}

	tests := []struct {
		name           string
		args           args
		objects        []runtime.Object
		wantErr        bool
		expectedResult bool
	}{
		{
			name: "happy path",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				crd,
				&unstructured.UnstructuredList{
					Items: []unstructured.Unstructured{*successDpa},
				},
			},
			wantErr:        false,
			expectedResult: true,
		},
		{
			name:           "no dataProtectionAppliation CRD",
			args:           args{l: logr.Logger{}},
			objects:        []runtime.Object{},
			wantErr:        false,
			expectedResult: false,
		},
		{
			name:           "no dataProtectionAppliation",
			args:           args{l: logr.Logger{}},
			objects:        []runtime.Object{crd},
			wantErr:        false,
			expectedResult: false,
		},
		{
			name: "dataProtectionApplication not reconciled",
			args: args{l: logr.Logger{}},
			objects: []runtime.Object{
				crd,
				&unstructured.UnstructuredList{
					Items: []unstructured.Unstructured{*failedDpa},
				},
			},
			wantErr:        true,
			expectedResult: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if ok, err := IsDataProtectionApplicationReconciled(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("IsDataProtectionApplicationReconciled() error = %v, wantErr %v", err, tt.wantErr)
			} else if ok != tt.expectedResult {
				t.Errorf("IsDataProtectionApplicationReconciled() reconciled = %t, expectedResult %t", ok, tt.expectedResult)
			}
		})
	}
}

func Test_backupStorageLocationReady(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}

	successBsl := &velerov1.BackupStorageLocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dpa-1",
			Namespace: backuprestore.OadpNs,
		},
		Status: velerov1.BackupStorageLocationStatus{
			Phase: velerov1.BackupStorageLocationPhaseAvailable,
		},
	}

	failedBsl := &velerov1.BackupStorageLocation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dpa-1",
			Namespace: backuprestore.OadpNs,
		},
		Status: velerov1.BackupStorageLocationStatus{
			Phase: velerov1.BackupStorageLocationPhaseUnavailable,
		},
	}

	tests := []struct {
		name    string
		args    args
		objects []runtime.Object
		wantErr bool
	}{
		{
			name: "happy path",
			objects: []runtime.Object{
				&velerov1.BackupStorageLocationList{
					Items: []velerov1.BackupStorageLocation{*successBsl},
				},
			},
			wantErr: false,
		},
		{
			name:    "no backupStorageLocation created",
			objects: []runtime.Object{},
			wantErr: true,
		},
		{
			name: "backupStorageLocation unavailable",
			objects: []runtime.Object{
				&velerov1.BackupStorageLocationList{
					Items: []velerov1.BackupStorageLocation{*failedBsl},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := AreBackupStorageLocationsAvailable(context.TODO(), tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("AreBackupStorageLocationsAvailable() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHealthChecks(t *testing.T) {
	type args struct {
		c client.Reader
		l logr.Logger
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sriovnetworknodestates.sriovnetwork.openshift.io",
		},
	}

	successSriovNetworkNodeState := &sriovv1.SriovNetworkNodeState{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Status: sriovv1.SriovNetworkNodeStateStatus{
			SyncStatus: "Succeeded",
		},
	}

	tests := []struct {
		name    string
		args    args
		wantErr bool
		objects []runtime.Object
	}{
		{
			name:    "fail all",
			wantErr: true,
			objects: []runtime.Object{
				crd,
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonAPIServiceResourceIssue,
					},
				},
				&configv1.ClusterOperator{
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorAvailable,
							},
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorProgressing,
							},
						},
					},
				},
				&mcv1.MachineConfigPool{Status: mcv1.MachineConfigPoolStatus{
					MachineCount:      1,
					ReadyMachineCount: 12,
				}},
				&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:   configv1.OperatorAvailable,
								Status: configv1.ConditionFalse,
							},
						},
					},
				},
			},
		},
		{
			name:    "happy path",
			wantErr: false,
			objects: []runtime.Object{
				&configv1.Infrastructure{
					ObjectMeta: metav1.ObjectMeta{
						Name: "cluster",
					},
					Status: configv1.InfrastructureStatus{
						InfrastructureTopology: configv1.SingleReplicaTopologyMode,
					},
				},
				&v1.Node{
					TypeMeta: metav1.TypeMeta{},
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							NodeRoleControlPlane: "",
							NodeRoleMaster:       "",
							NodeRoleWorker:       "",
						},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:   v1.NodeReady,
								Status: v1.ConditionTrue,
							},
							{
								Type:   v1.NodeNetworkUnavailable,
								Status: v1.ConditionFalse,
							},
						},
					},
				},
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonInstallSuccessful,
					},
				},
				&configv1.ClusterOperator{
					Status: configv1.ClusterOperatorStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Status: configv1.ConditionTrue,
								Type:   configv1.OperatorAvailable,
							},
						},
					},
				},
				&mcv1.MachineConfigPool{Status: mcv1.MachineConfigPoolStatus{
					MachineCount:      1,
					ReadyMachineCount: 1,
				}},
				&configv1.ClusterVersion{
					Status: configv1.ClusterVersionStatus{
						Conditions: []configv1.ClusterOperatorStatusCondition{
							{
								Type:   configv1.OperatorAvailable,
								Status: configv1.ConditionTrue,
							},
						},
					},
				},
				crd,
				&sriovv1.SriovNetworkNodeStateList{
					Items: []sriovv1.SriovNetworkNodeState{*successSriovNetworkNodeState},
				},
				&apiextensionsv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "dataprotectionapplications.oadp.openshift.io",
					},
				},
				&unstructured.UnstructuredList{
					Items: []unstructured.Unstructured{
						{
							Object: map[string]any{
								"kind":       backuprestore.DpaGvk.Kind,
								"apiVersion": backuprestore.DpaGvk.Group + "/" + backuprestore.DpaGvk.Version,
								"metadata": map[string]any{
									"name":      "oadp",
									"namespace": backuprestore.OadpNs,
								},
								"status": map[string]any{
									"conditions": []any{
										map[string]any{
											"type":   "Reconciled",
											"status": "True",
											"reason": "Complete",
										},
									},
								},
							},
						},
					},
				},
				&velerov1.BackupStorageLocationList{
					Items: []velerov1.BackupStorageLocation{
						{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "dpa-1",
								Namespace: backuprestore.OadpNs,
							},
							Status: velerov1.BackupStorageLocationStatus{
								Phase: velerov1.BackupStorageLocationPhaseAvailable,
							},
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			err := HealthChecks(context.TODO(), tt.args.c, tt.args.l, HealthCheckOptions{})
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthChecks() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestAreCertificateSigningRequestsReady(t *testing.T) {
	tests := []struct {
		name       string
		objects    []runtime.Object
		wantErr    bool
		wantErrMsg string
	}{
		{
			name: "Successful only if approved and true",
			objects: []runtime.Object{
				&k8sv1.CertificateSigningRequest{
					Status: k8sv1.CertificateSigningRequestStatus{
						Conditions: []k8sv1.CertificateSigningRequestCondition{
							{
								Type:   k8sv1.CertificateApproved,
								Status: v1.ConditionStatus(metav1.ConditionTrue),
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Fail with approved but status is false",
			objects: []runtime.Object{
				&k8sv1.CertificateSigningRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-csr",
					},
					Status: k8sv1.CertificateSigningRequestStatus{
						Conditions: []k8sv1.CertificateSigningRequestCondition{
							{
								Type:   k8sv1.CertificateApproved,
								Status: v1.ConditionStatus(metav1.ConditionFalse),
							},
						},
					},
				},
			},
			wantErr:    true,
			wantErrMsg: "certificateSigningRequest (csr) test-csr not yet approved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			err := AreCertificateSigningRequestsReady(context.Background(), c, logr.Logger{})
			if (err != nil) != tt.wantErr {
				t.Errorf("AreCertificateSigningRequestsReady() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				assert.Equal(t, err.Error(), tt.wantErrMsg)
			}
		})
	}
}
