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
	"strconv"
	"testing"
	"time"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

const oadpNs = "openshift-adp"

var (
	testscheme = scheme.Scheme
)

func init() {
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.Backup{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.BackupList{})
	testscheme.AddKnownTypes(configv1.GroupVersion, &configv1.ClusterVersion{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.DeleteBackupRequest{})
	testscheme.AddKnownTypes(velerov1.SchemeGroupVersion, &velerov1.DeleteBackupRequestList{})
}

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

func fakeBackupCr(name, applyWave, backupResource string) *velerov1.Backup {
	backup := &velerov1.Backup{
		TypeMeta: metav1.TypeMeta{
			Kind:       backupGvk.Kind,
			APIVersion: backupGvk.Group + "/" + backupGvk.Version,
		},
	}
	backup.SetName(name)
	backup.SetNamespace(oadpNs)
	backup.SetAnnotations(map[string]string{applyWaveAnn: applyWave})

	backup.Spec = velerov1.BackupSpec{
		IncludedNamespaces:               []string{"openshift-test"},
		IncludedNamespaceScopedResources: []string{backupResource},
	}
	return backup
}

func fakeBackupCrWithStatus(name, applyWave, backupResource string, phase velerov1.BackupPhase) *velerov1.Backup {
	backup := fakeBackupCr(name, applyWave, backupResource)
	backup.Status = velerov1.BackupStatus{
		Phase: phase,
	}

	return backup
}

func fakeConfigmap(name, applyWave string, number, start int, multiyamls bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: oadpNs,
		},
		Data: map[string]string{},
	}

	for i := start; i < number+start; i++ {
		backup := fakeBackupCr("backup"+strconv.Itoa(i), applyWave, "fakeResource")
		backupBytes, _ := yaml.Marshal(backup)
		restore := fakeRestoreCr("restore"+strconv.Itoa(i), applyWave, backup.Name)
		restoreBytes, _ := yaml.Marshal(restore)

		if multiyamls {
			name := "backup_restore" + strconv.Itoa(i)
			cm.Data[name] = string(backupBytes) + "---\n" + string(restoreBytes)
		} else {
			cm.Data[backup.Name] = string(backupBytes)
			cm.Data[restore.Name] = string(restoreBytes)
		}
	}

	return cm
}

func fakeSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: oadpNs,
		},
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}
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
			result, _ := sortByApplyWaveBackupCrs(tc.resources)
			assert.Equal(t, tc.expectedResult, result)
		})
	}
}

func TestTriggerBackup(t *testing.T) {
	testcases := []struct {
		name                  string
		existingBackups       []client.Object
		expectedBackupTracker BackupTracker
	}{
		{
			name:            "No backups applied",
			existingBackups: []client.Object{},
			expectedBackupTracker: BackupTracker{
				ProgressingBackups: []string{"backup1", "backup2", "backup3", "backup4"},
			},
		},
		{
			name: "Backups applied but have no status",
			existingBackups: []client.Object{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
				fakeBackupCr("backup4", "1", "fakeResource4"),
			},
			expectedBackupTracker: BackupTracker{
				PendingBackups: []string{"backup1", "backup2", "backup3", "backup4"},
			},
		},
		{
			name: "Backups applied but have failed status",
			existingBackups: []client.Object{
				fakeBackupCrWithStatus("backup1", "1", "fakeResource1", velerov1.BackupPhaseFailed),
				fakeBackupCrWithStatus("backup2", "1", "fakeResource2", velerov1.BackupPhaseInProgress),
				fakeBackupCrWithStatus("backup3", "1", "fakeResource3", velerov1.BackupPhaseCompleted),
				fakeBackupCrWithStatus("backup4", "1", "fakeResource4", velerov1.BackupPhaseFailedValidation),
			},
			expectedBackupTracker: BackupTracker{
				ProgressingBackups: []string{"backup2"},
				FailedBackups:      []string{"backup1", "backup4"},
				SucceededBackups:   []string{"backup3"},
			},
		},
		{
			name: "All backups have completed",
			existingBackups: []client.Object{
				fakeBackupCrWithStatus("backup1", "1", "fakeResource1", velerov1.BackupPhaseCompleted),
				fakeBackupCrWithStatus("backup2", "1", "fakeResource2", velerov1.BackupPhaseCompleted),
				fakeBackupCrWithStatus("backup3", "1", "fakeResource3", velerov1.BackupPhaseCompleted),
				fakeBackupCrWithStatus("backup4", "1", "fakeResource4", velerov1.BackupPhaseCompleted),
			},
			expectedBackupTracker: BackupTracker{
				SucceededBackups: []string{"backup1", "backup2", "backup3", "backup4"},
			},
		},
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: oadpNs,
		},
	}

	clusterVersion := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Spec: configv1.ClusterVersionSpec{
			ClusterID: "42fd3c76-4a1b-4e8b-8397-1c7210fd3e36",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			objs := []client.Object{ns, clusterVersion}
			objs = append(objs, tc.existingBackups...)

			fakeClient, err := getFakeClientFromObjects(objs...)
			if err != nil {
				t.Errorf("error in creating fake client")
			}

			backups := []*velerov1.Backup{
				fakeBackupCr("backup1", "1", "fakeResource1"),
				fakeBackupCr("backup2", "1", "fakeResource2"),
				fakeBackupCr("backup3", "1", "fakeResource3"),
				fakeBackupCr("backup4", "1", "fakeResource4"),
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			backupTracker, err := handler.StartOrTrackBackup(context.Background(), backups)
			if err != nil {
				t.Errorf("unexpected error: %v", err.Error())
			}

			// assert tracker values
			assert.Equal(t, len(tc.expectedBackupTracker.PendingBackups), len(backupTracker.PendingBackups))
			assert.Equal(t, len(tc.expectedBackupTracker.ProgressingBackups), len(backupTracker.ProgressingBackups))
			assert.Equal(t, len(tc.expectedBackupTracker.FailedBackups), len(backupTracker.FailedBackups))
			assert.Equal(t, len(tc.expectedBackupTracker.SucceededBackups), len(backupTracker.SucceededBackups))
		})
	}
}

func TestExportRestoresToDir(t *testing.T) {
	configMaps := []lcav1alpha1.ConfigMapRef{
		{
			Name:      "configmap1",
			Namespace: oadpNs,
		},
		{
			Name:      "configmap2",
			Namespace: oadpNs,
		},
		{
			Name:      "configmap3",
			Namespace: oadpNs,
		},
	}

	toDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(toDir)

	// Create fake configmaps
	cm1 := fakeConfigmap("configmap1", "1", 2, 1, false)
	cm2 := fakeConfigmap("configmap2", "10", 2, 3, false)
	// Configmap3 has a multi-document yaml format data
	cm3 := fakeConfigmap("configmap3", "11", 2, 5, true)

	fakeClient, err := getFakeClientFromObjects(cm1, cm2, cm3)
	if err != nil {
		t.Errorf("error in creating fake client")
	}

	handler := &BRHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("BackupRestore"),
	}

	err = handler.ExportRestoresToDir(context.Background(), configMaps, toDir)
	if err != nil {
		t.Fatalf("ExportRestoresToDir failed: %v", err)
	}

	// Check the output
	expectedDir1 := filepath.Join(toDir, OadpRestorePath, "restore1")
	expectedDir2 := filepath.Join(toDir, OadpRestorePath, "restore2")
	expectedDir3 := filepath.Join(toDir, OadpRestorePath, "restore3")
	expectedDirs := []string{expectedDir1, expectedDir2, expectedDir3}

	expectedFiles := []string{
		filepath.Join(expectedDir1, "1_restore1_openshift-adp.yaml"),
		filepath.Join(expectedDir1, "2_restore2_openshift-adp.yaml"),
		filepath.Join(expectedDir2, "1_restore3_openshift-adp.yaml"),
		filepath.Join(expectedDir2, "2_restore4_openshift-adp.yaml"),
		filepath.Join(expectedDir3, "1_restore5_openshift-adp.yaml"),
		filepath.Join(expectedDir3, "2_restore6_openshift-adp.yaml"),
	}
	for _, dir := range expectedDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			t.Errorf("ExportRestoresToDir failed to create directory %s: %v", dir, err)
		}
	}
	for _, file := range expectedFiles {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			t.Errorf("ExportRestoresToDir failed to create file %s: %v", file, err)
		}
	}
}

func TestExportOadpConfigurationToDir(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	toDir, err := os.MkdirTemp("", "staterootB")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(toDir)

	handler := &BRHandler{
		Client: c,
		Log:    ctrl.Log.WithName("BackupRestore"),
	}

	// Test case 1: storage secret not found
	err = handler.ExportOadpConfigurationToDir(context.Background(), toDir, oadpNs)
	assert.NoError(t, err)

	// Test case 2: DPA with velero credentials found
	dpa := &unstructured.Unstructured{
		Object: map[string]any{
			"kind":       dpaGvk.Kind,
			"apiVersion": dpaGvk.Group + "/" + dpaGvk.Version,
			"metadata": map[string]any{
				"name":      "dpa-name",
				"namespace": oadpNs,
			},
			"spec": map[string]any{
				"backupLocations": []any{
					map[string]any{
						"velero": map[string]any{
							"credential": map[string]any{
								"name": "velero-cred",
							},
						},
					},
					map[string]any{
						"velero": map[string]any{
							"credential": map[string]any{
								"name": "cloud-credentials",
							},
						},
					},
				},
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
	err = c.Create(context.Background(), dpa)
	assert.NoError(t, err)

	veleroCreds := fakeSecret("velero-cred")
	err = c.Create(context.Background(), veleroCreds)
	assert.NoError(t, err)

	storageSecret := fakeSecret("cloud-credentials")
	err = c.Create(context.Background(), storageSecret)
	assert.NoError(t, err)

	// Test oadp configurations are exported to files
	err = handler.ExportOadpConfigurationToDir(context.Background(), toDir, oadpNs)
	assert.NoError(t, err)

	// Check that the DPA was written to file
	dpaFilePath := filepath.Join(toDir, oadpDpaPath, dpa.GetName()+".yaml")
	_, err = os.Stat(dpaFilePath)
	assert.NoError(t, err)

	// Check that the secrets was written to file
	veleroCredSecretFilePath := filepath.Join(toDir, oadpSecretPath, "velero-cred.yaml")
	_, err = os.Stat(veleroCredSecretFilePath)
	assert.NoError(t, err)

	storageSecretFilePath := filepath.Join(toDir, oadpSecretPath, "cloud-credentials.yaml")
	_, err = os.Stat(storageSecretFilePath)
	assert.NoError(t, err)
}

func TestCleanupBackups(t *testing.T) {
	currentCluster := &configv1.ClusterVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: "version",
		},
		Spec: configv1.ClusterVersionSpec{
			ClusterID: "cluster1",
		},
	}

	// Create backups for different clusters
	backups := []client.Object{
		&velerov1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backupCluster1",
				Namespace: oadpNs,
				Labels: map[string]string{
					clusterIDLabel: "cluster1",
				},
			},
		},
		&velerov1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backupCluster2",
				Namespace: oadpNs,
				Labels: map[string]string{
					clusterIDLabel: "cluster2",
				},
			},
		},
		&velerov1.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backupCluster3",
				Namespace: oadpNs,
				Labels: map[string]string{
					clusterIDLabel: "cluster2",
				},
			},
		},
	}

	objs := []client.Object{currentCluster}
	objs = append(objs, backups...)
	fakeClient, err := getFakeClientFromObjects(objs...)
	if err != nil {
		t.Errorf("error in creating fake client")
	}
	assert.Equal(t, 3, len(backups))

	handler := &BRHandler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("BackupRestore"),
	}

	resultChan := make(chan bool)
	errorChan := make(chan error)

	// Test backup cleanup for cluster1
	go func() {
		result, err := handler.CleanupBackups(context.Background())
		resultChan <- result
		errorChan <- err
	}()

	// Mock the deletion of backup for cluster1
	time.Sleep(1 * time.Second)
	if err := fakeClient.Delete(context.Background(), backups[0]); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	result := <-resultChan
	err = <-errorChan
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Verify that the backup for cluster1 was deleted
	assert.Equal(t, true, result)

	// Verify backupDeletionRequest was created for cluster1 only
	deletionRequests := &velerov1.DeleteBackupRequestList{}
	if err := fakeClient.List(context.Background(), deletionRequests); err != nil {
		t.Errorf("failed to list deleteBackupRequest: %v", err)
	}
	assert.Equal(t, 1, len(deletionRequests.Items))
	assert.Equal(t, "backupCluster1", deletionRequests.Items[0].Name)
}
