package utils

import (
	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"testing"
)

var (
	testscheme = scheme.Scheme
)

func getFakeClientFromObjects() client.Client {
	testscheme.AddKnownTypes(ibuv1.GroupVersion, &ibuv1.ImageBasedUpgrade{})
	return fake.NewClientBuilder().WithScheme(testscheme).WithObjects(&ibuv1.ImageBasedUpgrade{}).Build()
}

func TestResetHistory(t *testing.T) {
	client := getFakeClientFromObjects()
	log := logr.Logger{}

	type args struct {
		ibu *ibuv1.ImageBasedUpgrade
	}
	tests := []struct {
		name        string
		args        args
		expectation *ibuv1.ImageBasedUpgrade
	}{
		{
			name: "history field resets to empty if desired stage is Idle",
			args: args{
				ibu: &ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						Stage: ibuv1.Stages.Idle,
					},
					Status: ibuv1.ImageBasedUpgradeStatus{
						History: []*ibuv1.History{
							{
								Stage: ibuv1.Stages.Upgrade,
							},
						},
					},
				},
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Idle,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{},
				},
			},
		},
		{
			name: "no change or reset in history field if any stage other than Idle is desired",
			args: args{
				ibu: &ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						Stage: ibuv1.Stages.Upgrade,
					},
					Status: ibuv1.ImageBasedUpgradeStatus{
						History: []*ibuv1.History{
							{
								Stage: ibuv1.Stages.Upgrade,
							},
						},
					},
				},
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage: ibuv1.Stages.Upgrade,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResetHistory(client, log, tt.args.ibu)
			if !equality.Semantic.DeepEqual(tt.args.ibu.Status.History, tt.expectation.Status.History) {
				assert.Fail(t, "expect ibu resources to be equal", "got: ", tt.args.ibu.Status.History, "want :", tt.expectation.Status.History)
			}
		})
	}
}

func TestStartPhase(t *testing.T) {
	client := getFakeClientFromObjects()
	log := logr.Logger{}

	// override
	currentTime := metav1.Now()
	getMetav1Now = func() metav1.Time {
		return currentTime
	}

	type args struct {
		ibu   *ibuv1.ImageBasedUpgrade
		phase string
	}
	tests := []struct {
		name        string
		args        args
		expectation *ibuv1.ImageBasedUpgrade
	}{
		{
			name: "Start a phase for Upgrade before initializing Stage history",
			args: args{
				ibu: &ibuv1.ImageBasedUpgrade{
					Spec: ibuv1.ImageBasedUpgradeSpec{
						Stage: ibuv1.Stages.Upgrade,
					},
				},
				phase: "TEST-PHASE",
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
			},
		},
		{
			name: "Start a phase for Upgrade after initializing Stage",
			args: args{
				ibu: func() *ibuv1.ImageBasedUpgrade {
					curIbu := &ibuv1.ImageBasedUpgrade{
						Spec: ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Upgrade},
					}
					StartStageHistory(client, log, curIbu)
					return curIbu
				}(),
				phase: "TEST-PHASE",
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{History: []*ibuv1.History{
					{
						Stage:     ibuv1.Stages.Upgrade,
						StartTime: currentTime,
						Phases: []*ibuv1.Phase{
							{
								Phase:     "TEST-PHASE",
								StartTime: currentTime,
							},
						},
					},
				}},
			},
		},
		{
			name: "Phase already started but function is called again during requeue",
			args: args{
				ibu: func() *ibuv1.ImageBasedUpgrade {
					curIbu := &ibuv1.ImageBasedUpgrade{
						Spec: ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Upgrade},
					}
					StartStageHistory(client, log, curIbu)
					StartPhase(client, log, curIbu, "TEST-PHASE")
					return curIbu
				}(),
				phase: "TEST-PHASE",
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Status: ibuv1.ImageBasedUpgradeStatus{History: []*ibuv1.History{
					{
						Stage:     ibuv1.Stages.Upgrade,
						StartTime: currentTime,
						Phases: []*ibuv1.Phase{
							{
								Phase:     "TEST-PHASE",
								StartTime: currentTime,
							},
						},
					},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StartPhase(client, log, tt.args.ibu, tt.args.phase)
			if !equality.Semantic.DeepEqual(tt.args.ibu.Status.History, tt.expectation.Status.History) {
				assert.Fail(t, "expect ibu status.history to be equal", "got", tt.args.ibu.Status.History, "want :", &tt.expectation.Status.History)
			}
		})
	}
}

func TestStartStageHistory(t *testing.T) {
	client := getFakeClientFromObjects()
	log := logr.Logger{}

	// override time
	currentTime := metav1.Now()
	getMetav1Now = func() metav1.Time {
		return currentTime
	}

	type args struct {
		ibu *ibuv1.ImageBasedUpgrade
	}
	tests := []struct {
		name        string
		args        args
		expectation *ibuv1.ImageBasedUpgrade
	}{
		{
			name: "A new stage is desired that's not of type Idle",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
			}},
			expectation: &ibuv1.ImageBasedUpgrade{
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage:     ibuv1.Stages.Upgrade,
							StartTime: currentTime,
						},
					},
				},
			},
		},
		{
			name: "A stage start time already recorded but called again during requeue",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage:     ibuv1.Stages.Upgrade,
							StartTime: currentTime,
						},
					},
				},
			}},
			expectation: &ibuv1.ImageBasedUpgrade{
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage:     ibuv1.Stages.Upgrade,
							StartTime: currentTime,
						},
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StartStageHistory(client, log, tt.args.ibu)
			if !equality.Semantic.DeepEqual(tt.args.ibu.Status.History, tt.expectation.Status.History) {
				assert.Fail(t, "expect ibu resources to be equal", "got", tt.args.ibu)
			}
		})
	}
}

func TestStopPhase(t *testing.T) {
	client := getFakeClientFromObjects()
	log := logr.Logger{}

	// override
	currentTime := metav1.Now()
	getMetav1Now = func() metav1.Time {
		return currentTime
	}

	type args struct {
		ibu   *ibuv1.ImageBasedUpgrade
		phase string
	}
	tests := []struct {
		name        string
		args        args
		expectation *ibuv1.ImageBasedUpgrade
	}{
		{
			name: "Stop a phase of a stage",
			args: args{
				ibu: func() *ibuv1.ImageBasedUpgrade {
					curIbu := &ibuv1.ImageBasedUpgrade{
						Spec: ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Upgrade},
					}
					StartStageHistory(client, log, curIbu)
					StartPhase(client, log, curIbu, "TEST-PHASE")
					return curIbu
				}(),
				phase: "TEST-PHASE",
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{History: []*ibuv1.History{
					{
						Stage:     ibuv1.Stages.Upgrade,
						StartTime: currentTime,
						Phases: []*ibuv1.Phase{
							{
								Phase:          "TEST-PHASE",
								StartTime:      currentTime,
								CompletionTime: currentTime,
							},
						},
					},
				}},
			},
		},
		{
			name: "Stop a phase that doesnt exist",
			args: args{
				ibu: func() *ibuv1.ImageBasedUpgrade {
					curIbu := &ibuv1.ImageBasedUpgrade{
						Spec: ibuv1.ImageBasedUpgradeSpec{Stage: ibuv1.Stages.Upgrade},
					}
					StartStageHistory(client, log, curIbu)
					return curIbu
				}(),
				phase: "TEST-PHASE",
			},
			expectation: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{History: []*ibuv1.History{
					{
						Stage:     ibuv1.Stages.Upgrade,
						StartTime: currentTime,
					},
				}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StopPhase(client, log, tt.args.ibu, tt.args.phase)
			if !equality.Semantic.DeepEqual(tt.args.ibu.Status.History, tt.expectation.Status.History) {
				assert.Fail(t, "expect ibu status.history to be equal", "got", tt.args.ibu.Status.History, "want :", &tt.expectation.Status.History)
			}
		})
	}
}

func TestStopStageHistory(t *testing.T) {
	client := getFakeClientFromObjects()
	log := logr.Logger{}

	// override time
	currentTime := metav1.Now()
	getMetav1Now = func() metav1.Time {
		return currentTime
	}

	type args struct {
		ibu *ibuv1.ImageBasedUpgrade
	}
	tests := []struct {
		name        string
		args        args
		expectation *ibuv1.ImageBasedUpgrade
	}{
		{
			name: "A stop a known stage timer with completionTime",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage:     ibuv1.Stages.Upgrade,
							StartTime: currentTime,
						},
					},
				},
			}},
			expectation: &ibuv1.ImageBasedUpgrade{
				Status: ibuv1.ImageBasedUpgradeStatus{
					History: []*ibuv1.History{
						{
							Stage:          ibuv1.Stages.Upgrade,
							StartTime:      currentTime,
							CompletionTime: currentTime,
						},
					},
				},
			},
		},
		{
			name: "Stop stage timer that doesnt exist",
			args: args{ibu: &ibuv1.ImageBasedUpgrade{
				Spec: ibuv1.ImageBasedUpgradeSpec{
					Stage: ibuv1.Stages.Upgrade,
				},
			}},
			expectation: &ibuv1.ImageBasedUpgrade{
				Status: ibuv1.ImageBasedUpgradeStatus{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			StopStageHistory(client, log, tt.args.ibu)
			if !equality.Semantic.DeepEqual(tt.args.ibu.Status.History, tt.expectation.Status.History) {
				assert.Fail(t, "expect ibu resources to be equal", "got", tt.args.ibu)
			}
		})
	}
}
