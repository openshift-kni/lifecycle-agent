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

package backuprestore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func init() {
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.Restore{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.RestoreList{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.BackupStorageLocation{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.BackupStorageLocationList{})
}

func fakeRestoreCr(name, applyWave, backupName string) *velerov1.Restore {
	restoreGvk := common.RestoreGvk
	restore := &velerov1.Restore{
		TypeMeta: v1.TypeMeta{
			Kind:       restoreGvk.Kind,
			APIVersion: restoreGvk.Group + "/" + restoreGvk.Version,
		},
	}
	restore.SetName(name)
	restore.SetNamespace(OadpNs)
	restore.SetAnnotations(map[string]string{common.ApplyWaveAnn: applyWave})

	restore.Spec = velerov1.RestoreSpec{
		BackupName: backupName,
	}
	return restore
}

func fakeRestoreCrWithStatus(name, applyWave, backupName string, phase velerov1.RestorePhase) *velerov1.Restore {
	restore := fakeRestoreCr(name, applyWave, backupName)
	restore.Status = velerov1.RestoreStatus{
		Phase: phase,
	}

	return restore
}

func TestTriggerRestore(t *testing.T) {
	testcases := []struct {
		name                   string
		existingBackups        []client.Object
		existingRestores       []client.Object
		expectedRestoreTracker RestoreTracker
	}{
		{
			name:             "No restores applied and required backups not found",
			existingBackups:  []client.Object{},
			existingRestores: []client.Object{},
			expectedRestoreTracker: RestoreTracker{
				MissingBackups: []string{"restore1", "restore2", "restore3"},
			},
		},
		{
			name: "No restores applied and required backups found",
			existingBackups: []client.Object{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
			},
			existingRestores: []client.Object{},
			expectedRestoreTracker: RestoreTracker{
				ProgressingRestores: []string{"restore1", "restore2", "restore3"},
			},
		},
		{
			name: "Restores applied but have no status",
			existingBackups: []client.Object{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
			},
			existingRestores: []client.Object{
				fakeRestoreCrWithStatus("restore1", "1", "backup1", ""),
				fakeRestoreCrWithStatus("restore2", "1", "backup2", ""),
				fakeRestoreCrWithStatus("restore3", "1", "backup3", ""),
			},
			expectedRestoreTracker: RestoreTracker{
				PendingRestores: []string{"restore1", "restore2", "restore3"},
			},
		},
		{
			name: "Restores applied but have failed status",
			existingBackups: []client.Object{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
			},
			existingRestores: []client.Object{
				fakeRestoreCrWithStatus("restore1", "1", "backup1", velerov1.RestorePhaseFailed),
				fakeRestoreCrWithStatus("restore2", "1", "backup2", velerov1.RestorePhaseInProgress),
				fakeRestoreCrWithStatus("restore3", "1", "backup3", velerov1.RestorePhaseCompleted),
			},
			expectedRestoreTracker: RestoreTracker{
				FailedRestores:      []string{"restore1"},
				ProgressingRestores: []string{"restore2"},
				SucceededRestores:   []string{"restore3"},
			},
		},
		{
			name: "All restores completed",
			existingBackups: []client.Object{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
			},
			existingRestores: []client.Object{
				fakeRestoreCrWithStatus("restore1", "1", "backup1", velerov1.RestorePhaseCompleted),
				fakeRestoreCrWithStatus("restore2", "1", "backup2", velerov1.RestorePhaseCompleted),
				fakeRestoreCrWithStatus("restore3", "1", "backup3", velerov1.RestorePhaseCompleted),
			},
			expectedRestoreTracker: RestoreTracker{
				SucceededRestores: []string{"restore1", "restore2", "restore3"},
			},
		},
	}

	ns := &corev1.Namespace{
		ObjectMeta: v1.ObjectMeta{
			Name: OadpNs,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{ns}
			objs = append(objs, tc.existingBackups...)
			objs = append(objs, tc.existingRestores...)
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			// restore CRs to apply
			restores := []*velerov1.Restore{
				fakeRestoreCr("restore1", "1", "backup1"),
				fakeRestoreCr("restore2", "1", "backup2"),
				fakeRestoreCr("restore3", "1", "backup3"),
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			restoreTracker, err := handler.StartOrTrackRestore(context.Background(), restores)
			if err != nil {
				t.Errorf("unexpected error: %v", err.Error())
			}

			// Verify the result
			assert.Equal(t, len(tc.expectedRestoreTracker.SucceededRestores), len(restoreTracker.SucceededRestores))
			assert.Equal(t, len(tc.expectedRestoreTracker.FailedRestores), len(restoreTracker.FailedRestores))
			assert.Equal(t, len(tc.expectedRestoreTracker.MissingBackups), len(restoreTracker.MissingBackups))
			assert.Equal(t, len(tc.expectedRestoreTracker.PendingRestores), len(restoreTracker.PendingRestores))
			assert.Equal(t, len(tc.expectedRestoreTracker.ProgressingRestores), len(restoreTracker.ProgressingRestores))
		})
	}
}

func TestLoadRestoresFromDir(t *testing.T) {
	// Create temporary directory
	tmpDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create restores directory
	restoreDir := filepath.Join(tmpDir, OadpRestorePath)
	if err := os.MkdirAll(restoreDir, 0755); err != nil {
		t.Fatalf("Failed to create restore directory: %v", err)
	}

	// Create two subdirectories for restores
	restoreSubDir1 := filepath.Join(restoreDir, "restore1")
	if err := os.Mkdir(restoreSubDir1, 0755); err != nil {
		t.Fatalf("Failed to create restore subdirectory: %v", err)
	}
	restoreSubDir2 := filepath.Join(restoreDir, "restore2")
	if err := os.Mkdir(restoreSubDir2, 0755); err != nil {
		t.Fatalf("Failed to create restore subdirectory: %v", err)
	}

	restore1File := filepath.Join(restoreSubDir1, "1_default-restore1.yaml")
	if err := os.WriteFile(restore1File, []byte("apiVersion: velero.io/v1\n"+
		"kind: Restore\n"+
		"metadata:\n"+
		"  name: restore1\n"+
		"spec:\n"+
		"  backupName: backup1\n"), 0644); err != nil {
		t.Fatalf("Failed to create restore file: %v", err)
	}
	restore2File := filepath.Join(restoreSubDir1, "2_default-restore2.yaml")
	if err := os.WriteFile(restore2File, []byte("apiVersion: velero.io/v1\n"+
		"kind: Restore\n"+
		"metadata:\n"+
		"  name: restore2\n"+
		"spec:\n"+
		"  backupName: backup2\n"), 0644); err != nil {
		t.Fatalf("Failed to create restore file: %v", err)
	}
	restore3File := filepath.Join(restoreSubDir2, "1_default-restore3.yaml")
	if err := os.WriteFile(restore3File, []byte("apiVersion: velero.io/v1\n"+
		"kind: Restore\n"+
		"metadata:\n"+
		"  name: restore3\n"+
		"spec:\n"+
		"  backupName: backup3\n"), 0644); err != nil {
		t.Fatalf("Failed to create restore file: %v", err)
	}

	handler := &BRHandler{
		Client: nil,
		Log:    ctrl.Log.WithName("BackupRestore"),
	}

	// Override the default host path
	hostPath = tmpDir
	// Load restores from the temporary directory
	restores, err := handler.LoadRestoresFromOadpRestorePath()
	if err != nil {
		t.Fatalf("Failed to load restores: %v", err)
	}

	// Check that the restores were loaded in expected order correctly
	expectedRestores := [][]*velerov1.Restore{
		{
			{
				TypeMeta: v1.TypeMeta{
					Kind:       "Restore",
					APIVersion: "velero.io/v1",
				},
				ObjectMeta: v1.ObjectMeta{
					Name: "restore1",
				},
				Spec: velerov1.RestoreSpec{
					BackupName: "backup1",
				},
			},
			{
				TypeMeta: v1.TypeMeta{
					Kind:       "Restore",
					APIVersion: "velero.io/v1",
				},
				ObjectMeta: v1.ObjectMeta{
					Name: "restore2",
				},
				Spec: velerov1.RestoreSpec{
					BackupName: "backup2",
				},
			},
		},
		{
			{
				TypeMeta: v1.TypeMeta{
					Kind:       "Restore",
					APIVersion: "velero.io/v1",
				},
				ObjectMeta: v1.ObjectMeta{
					Name: "restore3",
				},
				Spec: velerov1.RestoreSpec{
					BackupName: "backup3",
				},
			},
		},
	}
	if !reflect.DeepEqual(restores, expectedRestores) {
		t.Errorf("Unexpected restores: got %v, expected %v", restores, expectedRestores)
	}
}

func TestEnsureOadpConfigurations(t *testing.T) {
	// Create temporary directory
	fromDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	defer os.RemoveAll(fromDir)

	// Create oadp directory
	dpaDir := filepath.Join(fromDir, OadpDpaPath)
	if err := os.MkdirAll(dpaDir, 0755); err != nil {
		t.Fatalf("Failed to create oadp directory: %v", err)
	}

	// Create oadp DPA file
	dpa := &unstructured.Unstructured{
		Object: map[string]any{
			"kind":       dpaGvk.Kind,
			"apiVersion": dpaGvk.Group + "/" + dpaGvk.Version,
			"metadata": map[string]any{
				"name":      "oadp",
				"namespace": OadpNs,
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
	dpaFilePath := filepath.Join(dpaDir, dpa.GetName()+".yaml")
	if err := utils.MarshalToYamlFile(dpa, dpaFilePath); err != nil {
		t.Errorf("error in writing dpa to file")
	}

	fakeClient, err := getFakeClientFromObjects(dpa)
	if err != nil {
		t.Errorf("error in creating fake client")
	}

	handler := &BRHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("BackupRestore"),
	}

	errorChan := make(chan error)
	// Verify DPA is reconciled and storage backends are availiable
	go func() {
		// Override the default host path
		hostPath = fromDir
		err := handler.EnsureOadpConfiguration(context.Background())
		errorChan <- err
	}()

	// Mock the backup storage
	time.Sleep(1 * time.Second)
	bsl := fakeBackupStorageBackendWithStatus("oadp-1", velerov1.BackupStorageLocationPhaseAvailable)
	if err := fakeClient.Create(context.Background(), bsl); err != nil {
		t.Errorf("error in creating backup storage location")
	}
}

func fakeBackupStorageBackendWithStatus(name string, phase velerov1.BackupStorageLocationPhase) *velerov1.BackupStorageLocation {
	return &velerov1.BackupStorageLocation{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: OadpNs,
		},
		Status: velerov1.BackupStorageLocationStatus{
			Phase: phase,
		},
	}
}

func TestEnsureStorageBackendAvailable(t *testing.T) {
	testcases := []struct {
		name        string
		bsl         []client.Object
		expectedErr error
	}{
		{
			name: "Backup storage location is unavailable",
			bsl: []client.Object{
				fakeBackupStorageBackendWithStatus("oadp1", velerov1.BackupStorageLocationPhaseUnavailable),
				fakeBackupStorageBackendWithStatus("oadp2", velerov1.BackupStorageLocationPhaseAvailable),
			},
			expectedErr: NewBRStorageBackendUnavailableError("BackupStorageLocation is unavailable. Name: oadp1, Error: "),
		},
		{
			name: "Backup storage locations are available",
			bsl: []client.Object{
				fakeBackupStorageBackendWithStatus("oadp1", velerov1.BackupStorageLocationPhaseAvailable),
				fakeBackupStorageBackendWithStatus("oadp2", velerov1.BackupStorageLocationPhaseAvailable),
			},
			expectedErr: nil,
		},
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: OadpNs,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{ns}
			objs = append(objs, tc.bsl...)
			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			err = handler.ensureStorageBackendAvailable(context.Background(), OadpNs)
			assert.Equal(t, err, tc.expectedErr)
		})
	}
}
