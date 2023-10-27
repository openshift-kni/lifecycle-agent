package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPrepHandler_initialCall(t *testing.T) {
	type ExpectedCondition struct {
		ConditionType   utils.ConditionType
		ConditionReason utils.ConditionReason
		ConditionStatus v1.ConditionStatus
		Message         string
	}
	testcases := []struct {
		name               string
		progress           string
		chrootCommands     []string
		result             ctrl.Result
		expectedConditions []ExpectedCondition
	}{
		{
			name:           "initital call",
			progress:       "",
			chrootCommands: []string{"/var/ibu/prepGetSeedImage.sh --seed-image  --progress-file /var/ibu/prep-progress"},
			result:         requeueWithShortInterval(),
		},
		{
			name:           "setup state root",
			progress:       "completed-seed-image-pull",
			chrootCommands: []string{"/var/ibu/prepSetupStateroot.sh --seed-image  --progress-file /var/ibu/prep-progress"},
			result:         requeueWithShortInterval(),
		},
		{
			name:           "pull images",
			progress:       "completed-stateroot",
			chrootCommands: []string{"/var/ibu/prepPullImages.sh --seed-image  --progress-file /var/ibu/prep-progress"},
			result:         requeueWithShortInterval(),
		},
		{
			name:           "clean up",
			progress:       "completed-precache",
			chrootCommands: []string{"/var/ibu/prepCleanup.sh --seed-image "},
			result:         doNotRequeue(),
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.PrepCompleted,
					utils.ConditionReasons.Completed,
					v1.ConditionTrue,
					"Prep completed",
				}, {
					utils.ConditionTypes.PrepInProgress,
					utils.ConditionReasons.Completed,
					v1.ConditionFalse,
					"Prep completed",
				},
			},
		},
		{
			name:           "failed",
			progress:       "Failed",
			chrootCommands: []string{},
			result:         doNotRequeue(),
			expectedConditions: []ExpectedCondition{
				{
					utils.ConditionTypes.PrepCompleted,
					utils.ConditionReasons.Completed,
					v1.ConditionFalse,
					"Prep failed",
				}, {
					utils.ConditionTypes.PrepInProgress,
					utils.ConditionReasons.Completed,
					v1.ConditionFalse,
					"Prep failed",
				},
			},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{}
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}
			fakeCmd := &utils.FakeCmd{ChrootCommands: make(chan string, 10), Commands: make(chan string, 10)}
			fakeFs := &utils.FakeFS{FilesExist: make(map[string]bool)}

			r := &ImageBasedUpgradeReconciler{
				Client: fakeClient,
				Log:    logr.Discard(),
				Scheme: fakeClient.Scheme(),
				FS:     fakeFs,
				Cmd:    fakeCmd,
			}
			fakeFs.FilesExist["/host"] = true
			if tc.progress != "" {
				fakeFs.FilesExist["/host/var/ibu/prep-progress"] = true
			}
			fakeFs.ReadContent = tc.progress

			ibu := &ranv1alpha1.ImageBasedUpgrade{}
			result, err := r.handlePrep(context.TODO(), ibu)
			assert.NoError(t, err)
			assert.Equal(t, tc.result, result)
			for _, expectedCondition := range tc.expectedConditions {
				con := meta.FindStatusCondition(ibu.Status.Conditions, string(expectedCondition.ConditionType))
				assert.Equal(t, con == nil, false)
				if con != nil {
					assert.Equal(t, expectedCondition, ExpectedCondition{
						utils.ConditionType(con.Type),
						utils.ConditionReason(con.Reason),
						con.Status,
						con.Message})
				}
			}
			for _, c := range tc.chrootCommands {
				select {
				case res := <-fakeCmd.ChrootCommands:
					assert.Equal(t, c, res)
				case <-time.After(100 * time.Millisecond):
					panic("timeout 100")
				}
			}
			close(fakeCmd.ChrootCommands)
			close(fakeCmd.Commands)
		})
	}
}
