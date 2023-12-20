package healthcheck

import (
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
	"time"
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
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.ClusterServiceVersionList{})
	s.AddKnownTypes(operatorsv1alpha1.SchemeGroupVersion, &operatorsv1alpha1.ClusterServiceVersion{})
	s.AddKnownTypes(configv1.GroupVersion, &configv1.Infrastructure{})
	s.AddKnownTypes(v1.SchemeGroupVersion, &v1.Node{})
}

func Test_nodesReady(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

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
			if err := nodesReady(tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("nodesReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterServiceVersionReady(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

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
			name: "fail for when not CSVReasonInstallSuccessful",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonAPIServiceResourceIssue,
					},
				},
			},
			wantErr: true,
		},
		{
			name: "fail for when only lifecycle-agent",
			objects: []runtime.Object{
				&operatorsv1alpha1.ClusterServiceVersion{
					ObjectMeta: metav1.ObjectMeta{
						Name: "lifecycle-agent",
					},
					Status: operatorsv1alpha1.ClusterServiceVersionStatus{
						Phase:  operatorsv1alpha1.CSVPhaseSucceeded,
						Reason: operatorsv1alpha1.CSVReasonAPIServiceResourceIssue,
					},
				},
			},
			wantErr: false, // todo: this should fail
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			if err := clusterServiceVersionReady(tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("clusterOperatorsReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterOperatorsReady(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

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
			if err := clusterOperatorsReady(tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("clusterOperatorsReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_machineConfigPoolReady(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

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
			if err := machineConfigPoolReady(tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("machineConfigPoolReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func Test_clusterVersionReady(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

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
			if err := clusterVersionReady(tt.args.c, tt.args.l); (err != nil) != tt.wantErr {
				t.Errorf("clusterVersionReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHealthChecks(t *testing.T) {
	oldPoll := pollTimeout
	defer func() {
		pollTimeout = oldPoll
	}()
	pollTimeout = 1 * time.Microsecond

	type args struct {
		c client.Reader
		l logr.Logger
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
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.args.c = fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(tt.objects...).Build()
			err := HealthChecks(tt.args.c, tt.args.l)
			if (err != nil) != tt.wantErr {
				t.Errorf("HealthChecks() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
