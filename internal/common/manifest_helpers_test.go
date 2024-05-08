package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func fakeBackupCr(name, applyWave, backupResource string) *velerov1.Backup {
	backup := &velerov1.Backup{
		TypeMeta: metav1.TypeMeta{
			Kind:       BackupGvk.Kind,
			APIVersion: BackupGvk.Group + "/" + BackupGvk.Version,
		},
	}
	backup.SetName(name)
	backup.SetNamespace("ns")
	backup.SetAnnotations(map[string]string{ApplyWaveAnn: applyWave})

	backup.Spec = velerov1.BackupSpec{
		IncludedNamespaces:               []string{"openshift-test"},
		IncludedNamespaceScopedResources: []string{backupResource},
	}
	return backup
}

func fakeRestoreCr(name, applyWave, backupName string) *velerov1.Restore {
	restore := &velerov1.Restore{
		TypeMeta: metav1.TypeMeta{
			Kind:       RestoreGvk.Kind,
			APIVersion: RestoreGvk.Group + "/" + RestoreGvk.Version,
		},
	}
	restore.SetName(name)
	restore.SetNamespace("ns")
	restore.SetAnnotations(map[string]string{ApplyWaveAnn: applyWave})

	restore.Spec = velerov1.RestoreSpec{
		BackupName: backupName,
	}
	return restore
}

func TestSortBackupCrs(t *testing.T) {
	testcases := []struct {
		name           string
		resources      []*velerov1.Backup
		expectedResult [][]*velerov1.Backup
	}{
		{
			name: "Multiple resources contain the same wave number",
			resources: []*velerov1.Backup{
				fakeBackupCr("c_backup", "3", "fakeResource"),
				fakeBackupCr("d_backup", "10", "fakeResource"),
				fakeBackupCr("a_backup", "3", "fakeResource"),
				fakeBackupCr("b_backup", "1", "fakeResource"),
				fakeBackupCr("f_backup", "100", "fakeResource"),
				fakeBackupCr("e_backup", "100", "fakeResource"),
			},
			expectedResult: [][]*velerov1.Backup{{
				fakeBackupCr("b_backup", "1", "fakeResource"),
			}, {
				fakeBackupCr("a_backup", "3", "fakeResource"),
				fakeBackupCr("c_backup", "3", "fakeResource"),
			}, {
				fakeBackupCr("d_backup", "10", "fakeResource"),
			}, {
				fakeBackupCr("e_backup", "100", "fakeResource"),
				fakeBackupCr("f_backup", "100", "fakeResource"),
			},
			},
		},
		{
			name: "Multiple resources have no wave number",
			resources: []*velerov1.Backup{
				fakeBackupCr("c_backup", "", "fakeResource"),
				fakeBackupCr("d_backup", "10", "fakeResource"),
				fakeBackupCr("a_backup", "3", "fakeResource"),
				fakeBackupCr("b_backup", "1", "fakeResource"),
				fakeBackupCr("f_backup", "100", "fakeResource"),
				fakeBackupCr("e_backup", "100", "fakeResource"),
				fakeBackupCr("g_backup", "", "fakeResource"),
			},
			expectedResult: [][]*velerov1.Backup{{
				fakeBackupCr("b_backup", "1", "fakeResource"),
			}, {
				fakeBackupCr("a_backup", "3", "fakeResource"),
			}, {
				fakeBackupCr("d_backup", "10", "fakeResource"),
			}, {
				fakeBackupCr("e_backup", "100", "fakeResource"),
				fakeBackupCr("f_backup", "100", "fakeResource"),
			}, {
				fakeBackupCr("c_backup", "", "fakeResource"),
				fakeBackupCr("g_backup", "", "fakeResource"),
			},
			},
		},
		{
			name: "All resources have no wave number",
			resources: []*velerov1.Backup{
				fakeBackupCr("c_backup", "", "fakeResource"),
				fakeBackupCr("d_backup", "", "fakeResource"),
				fakeBackupCr("a_backup", "", "fakeResource"),
				fakeBackupCr("b_backup", "", "fakeResource"),
				fakeBackupCr("f_backup", "", "fakeResource"),
				fakeBackupCr("e_backup", "", "fakeResource"),
			},
			expectedResult: [][]*velerov1.Backup{{
				fakeBackupCr("a_backup", "", "fakeResource"),
				fakeBackupCr("b_backup", "", "fakeResource"),
				fakeBackupCr("c_backup", "", "fakeResource"),
				fakeBackupCr("d_backup", "", "fakeResource"),
				fakeBackupCr("e_backup", "", "fakeResource"),
				fakeBackupCr("f_backup", "", "fakeResource"),
			},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := SortAndGroupByApplyWave[*velerov1.Backup](tc.resources)
			assert.Equal(t, tc.expectedResult, result)
		})
	}
}

func TestSortRestoreCrs(t *testing.T) {
	testcases := []struct {
		name           string
		resources      []*velerov1.Restore
		expectedResult [][]*velerov1.Restore
	}{
		{
			name: "Multiple resources contain the same wave number",
			resources: []*velerov1.Restore{
				fakeRestoreCr("c_Restore", "3", "backup1"),
				fakeRestoreCr("d_Restore", "10", "backup1"),
				fakeRestoreCr("a_Restore", "3", "backup1"),
				fakeRestoreCr("b_Restore", "1", "backup1"),
				fakeRestoreCr("f_Restore", "100", "backup1"),
				fakeRestoreCr("e_Restore", "100", "backup1"),
			},
			expectedResult: [][]*velerov1.Restore{{
				fakeRestoreCr("b_Restore", "1", "backup1"),
			}, {
				fakeRestoreCr("a_Restore", "3", "backup1"),
				fakeRestoreCr("c_Restore", "3", "backup1"),
			}, {
				fakeRestoreCr("d_Restore", "10", "backup1"),
			}, {
				fakeRestoreCr("e_Restore", "100", "backup1"),
				fakeRestoreCr("f_Restore", "100", "backup1"),
			},
			},
		},
		{
			name: "Multiple resources have no wave number",
			resources: []*velerov1.Restore{
				fakeRestoreCr("c_Restore", "", "backup1"),
				fakeRestoreCr("d_Restore", "10", "backup1"),
				fakeRestoreCr("a_Restore", "3", "backup1"),
				fakeRestoreCr("b_Restore", "1", "backup1"),
				fakeRestoreCr("f_Restore", "100", "backup1"),
				fakeRestoreCr("e_Restore", "100", "backup1"),
				fakeRestoreCr("g_Restore", "", "backup1"),
			},
			expectedResult: [][]*velerov1.Restore{{
				fakeRestoreCr("b_Restore", "1", "backup1"),
			}, {
				fakeRestoreCr("a_Restore", "3", "backup1"),
			}, {
				fakeRestoreCr("d_Restore", "10", "backup1"),
			}, {
				fakeRestoreCr("e_Restore", "100", "backup1"),
				fakeRestoreCr("f_Restore", "100", "backup1"),
			}, {
				fakeRestoreCr("c_Restore", "", "backup1"),
				fakeRestoreCr("g_Restore", "", "backup1"),
			},
			},
		},
		{
			name: "All resources have no wave number",
			resources: []*velerov1.Restore{
				fakeRestoreCr("c_Restore", "", "backup1"),
				fakeRestoreCr("d_Restore", "", "backup1"),
				fakeRestoreCr("a_Restore", "", "backup1"),
				fakeRestoreCr("b_Restore", "", "backup1"),
				fakeRestoreCr("f_Restore", "", "backup1"),
				fakeRestoreCr("e_Restore", "", "backup1"),
			},
			expectedResult: [][]*velerov1.Restore{{
				fakeRestoreCr("a_Restore", "", "backup1"),
				fakeRestoreCr("b_Restore", "", "backup1"),
				fakeRestoreCr("c_Restore", "", "backup1"),
				fakeRestoreCr("d_Restore", "", "backup1"),
				fakeRestoreCr("e_Restore", "", "backup1"),
				fakeRestoreCr("f_Restore", "", "backup1"),
			},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := SortAndGroupByApplyWave[*velerov1.Restore](tc.resources)
			assert.Equal(t, tc.expectedResult, result)
		})
	}
}
