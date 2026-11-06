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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	testscheme = scheme.Scheme
)

func getFakeClientFromObjects(objs ...client.Object) (client.WithWatch, error) {
	c := fake.NewClientBuilder().WithScheme(testscheme).WithObjects(objs...).WithStatusSubresource(objs...).Build()
	return c, nil
}

// TestStartBackupFilesystemErrorIsTerminal verifies that a filesystem error
// while preparing the local backup directory is returned as a BRFailedError, so
// the Upgrade stage fails terminally (human intervention) instead of requeuing
// forever: a backup that cannot write to local node storage will not succeed on
// retry.
func TestStartBackupFilesystemErrorIsTerminal(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-cm", Namespace: "openshift-adp"},
		Data: map[string]string{
			"backup.yaml": `apiVersion: velero.io/v1
kind: Backup
metadata:
  name: b1
spec:
  includedClusterScopedResources:
  - clusterroles
`,
			"restore.yaml": `apiVersion: velero.io/v1
kind: Restore
metadata:
  name: r1
spec:
  backupName: b1
`,
		},
	}
	c, err := getFakeClientFromObjects(cm)
	assert.NoError(t, err)

	// Point hostPath at a temp tree where an ancestor of the backup directory is
	// a regular file, so clearing/creating the backup directory fails with
	// ENOTDIR regardless of process privileges (root bypasses permission bits).
	tmp := t.TempDir()
	blockingParent := filepath.Join(tmp, "var", "lib", "containers")
	assert.NoError(t, os.MkdirAll(filepath.Dir(blockingParent), 0o700))
	assert.NoError(t, os.WriteFile(blockingParent, []byte("not a directory"), 0o600))

	origHostPath := hostPath
	hostPath = tmp
	defer func() { hostPath = origHostPath }()

	h := &BRHandler{Client: c, Log: logr.Discard()}
	_, err = h.StartBackup(context.Background(), []ibuv1.ConfigMapRef{
		{Name: "backup-cm", Namespace: "openshift-adp"},
	})
	assert.Error(t, err)
	assert.True(t, IsBRFailedError(err),
		"filesystem error should be a terminal BRFailedError, got: %v", err)
}

// TestStartBackupFullFidelityAndSkips verifies that StartBackup backs up
// resources in full fidelity (cluster-assigned metadata is kept on disk and
// stripped only at restore), skips resources being deleted and completed/failed
// Jobs, and persists the per-spec restore configuration.
func TestStartBackupFullFidelityAndSkips(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "backup-cm", Namespace: "openshift-adp"},
		Data: map[string]string{
			"backup.yaml": `apiVersion: velero.io/v1
kind: Backup
metadata:
  name: b1
spec:
  includedNamespaces:
  - app-ns
  includedNamespaceScopedResources:
  - configmaps
  - jobs
`,
			"restore.yaml": `apiVersion: velero.io/v1
kind: Restore
metadata:
  name: r1
spec:
  backupName: b1
  restorePVs: true
  restoreStatus:
    includedResources:
    - configmaps
`,
		},
	}
	c, err := getFakeClientFromObjects(cm)
	assert.NoError(t, err)

	dynScheme := runtime.NewScheme()
	register := func(gvk schema.GroupVersionKind, isList bool) {
		if isList {
			dynScheme.AddKnownTypeWithName(gvk, &unstructured.UnstructuredList{})
		} else {
			dynScheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		}
	}
	register(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, false)
	register(schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"}, true)
	register(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, false)
	register(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMapList"}, true)
	register(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}, false)
	register(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "JobList"}, true)

	appNs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]interface{}{"name": "app-ns", "resourceVersion": "1"},
	}}
	keepCM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{
			"name": "keep-cm", "namespace": "app-ns",
			"uid": "cm-uid-123", "resourceVersion": "42",
		},
		"data": map[string]interface{}{"k": "v"},
	}}
	deletingCM := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]interface{}{
			"name": "deleting-cm", "namespace": "app-ns",
			"deletionTimestamp": "2020-01-01T00:00:00Z",
			"finalizers":        []interface{}{"example.com/keep"},
		},
	}}
	doneJob := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]interface{}{"name": "done-job", "namespace": "app-ns"},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Complete", "status": "True"},
			},
		},
	}}
	activeJob := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]interface{}{"name": "active-job", "namespace": "app-ns"},
		"status":   map[string]interface{}{"active": int64(1)},
	}}

	dc := dynamicfake.NewSimpleDynamicClient(dynScheme, appNs, keepCM, deletingCM, doneJob, activeJob)
	disc := &fakeDiscovery{byGV: map[string]*metav1.APIResourceList{
		"v1": {GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "namespaces", SingularName: "namespace", Namespaced: false, Kind: "Namespace"},
			{Name: "configmaps", SingularName: "configmap", Namespaced: true, Kind: "ConfigMap"},
		}},
		"batch/v1": {GroupVersion: "batch/v1", APIResources: []metav1.APIResource{
			{Name: "jobs", SingularName: "job", Namespaced: true, Kind: "Job"},
		}},
	}}

	tmp := t.TempDir()
	origHostPath := hostPath
	hostPath = tmp
	defer func() { hostPath = origHostPath }()

	h := &BRHandler{Client: c, DynamicClient: dc, DiscoveryClient: disc, Log: logr.Discard()}
	bt, err := h.StartBackup(context.Background(), []ibuv1.ConfigMapRef{
		{Name: "backup-cm", Namespace: "openshift-adp"},
	})
	assert.NoError(t, err)
	assert.Contains(t, bt.SucceededBackups, "b1")

	// No apply-wave on either CR, so b1 forms the single (final) restore group,
	// written as the first ordinal directory.
	specDir := filepath.Join(tmp, LocalBackupPath, "restore-1")
	entries, err := os.ReadDir(specDir)
	assert.NoError(t, err)

	var yamlNames []string
	foundConfig := false
	for _, e := range entries {
		if e.Name() == restoreConfigFileName {
			foundConfig = true
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") {
			yamlNames = append(yamlNames, e.Name())
		}
	}
	joined := strings.Join(yamlNames, "\n")

	assert.True(t, foundConfig, "restore-config.json must be persisted next to the resources")
	assert.Contains(t, joined, "keep-cm", "a live ConfigMap must be backed up")
	assert.Contains(t, joined, "active-job", "a running Job must be backed up")
	assert.Contains(t, joined, "namespace", "the included namespace must be backed up")
	assert.NotContains(t, joined, "deleting-cm", "a resource being deleted must be skipped")
	assert.NotContains(t, joined, "done-job", "a completed Job must be skipped")

	// Full fidelity: cluster-assigned metadata is preserved on disk (it is stripped
	// only at restore time).
	var keepCMFile string
	for _, n := range yamlNames {
		if strings.Contains(n, "keep-cm") {
			keepCMFile = n
		}
	}
	assert.NotEmpty(t, keepCMFile)
	data, err := os.ReadFile(filepath.Join(specDir, keepCMFile))
	assert.NoError(t, err)
	assert.Contains(t, string(data), "cm-uid-123", "uid must be preserved in the backup file")
	assert.Contains(t, string(data), "resourceVersion", "resourceVersion must be preserved in the backup file")

	// The persisted group restore config reflects the paired Restore CR, and its
	// entry references the backed-up resource files.
	cfgData, err := os.ReadFile(filepath.Join(specDir, restoreConfigFileName))
	assert.NoError(t, err)
	cfg := &groupRestoreConfig{}
	assert.NoError(t, json.Unmarshal(cfgData, cfg))
	assert.Len(t, cfg.Entries, 1)
	assert.True(t, cfg.Entries[0].RestorePVs)
	assert.Equal(t, []string{"configmaps"}, cfg.Entries[0].RestoreStatusResources)
	assert.ElementsMatch(t, yamlNames, cfg.Entries[0].Files,
		"the group config entry must reference exactly the backed-up resource files")

	// The root-level version manifest is written beside checksums.sha256, covered
	// by the checksum manifest, and records the backup metadata. Only one restore
	// group exists here, so yamlNames is the full resource set.
	backupRoot := filepath.Join(tmp, LocalBackupPath)
	manifestData, err := os.ReadFile(filepath.Join(backupRoot, backupLayoutFileName))
	assert.NoError(t, err)
	manifest := &backupLayoutManifest{}
	assert.NoError(t, json.Unmarshal(manifestData, manifest))
	assert.Equal(t, currentLayoutVersion, manifest.LayoutVersion)
	assert.Equal(t, []string{"b1"}, manifest.BackupSpecNames)
	assert.Equal(t, len(yamlNames), manifest.ResourceCount,
		"resourceCount must equal the number of backed-up resource files")
	_, perr := time.Parse(time.RFC3339, manifest.BackupCreationTimestamp)
	assert.NoError(t, perr, "timestamp must be RFC3339 parseable")

	checksumData, err := os.ReadFile(filepath.Join(backupRoot, "checksums.sha256"))
	assert.NoError(t, err)
	assert.Contains(t, string(checksumData), backupLayoutFileName,
		"the version manifest must be covered by checksums.sha256")

	// The produced backup validates end to end: the checksums verify and the layout
	// manifest is accepted by the restore side.
	validated, err := validateChecksums(backupRoot)
	assert.NoError(t, err)
	_, err = loadAndValidateBackupLayout(backupRoot, validated, logr.Discard())
	assert.NoError(t, err)
}

func TestGetObjsFromApplyLabel(t *testing.T) {
	testcases := []struct {
		name       string
		annotation string
		expected   []ObjMetadata
	}{
		{
			name:       "one name spaced resource",
			annotation: "apps/v1/deployments/default/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "deployments",
					Namespace: "default",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "one cluster resource, one namespaced",
			annotation: "apps/v1/clusterroles/klusterlet,apps/v1/deployment/ns/klusterlet-deploy",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "deployment",
					Namespace: "ns",
					Name:      "klusterlet-deploy",
				},
			},
		},
		{
			name:       "two cluster resources",
			annotation: "apps/v1/clusterroles/klusterlet,apps/v1/clusterroles/klusterlet2",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet2",
				},
			},
		},
		{
			name:       "one cluster resources",
			annotation: "apps/v1/clusterroles/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "apps",
					Version:   "v1",
					Resource:  "clusterroles",
					Namespace: "",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "object without group and namespace",
			annotation: "v1/namespace/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "",
					Version:   "v1",
					Resource:  "namespace",
					Namespace: "",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "object without group",
			annotation: "v1/secrets/default/klusterlet",
			expected: []ObjMetadata{
				{
					Group:     "",
					Version:   "v1",
					Resource:  "secrets",
					Namespace: "default",
					Name:      "klusterlet",
				},
			},
		},
		{
			name:       "empty",
			annotation: "",
			expected:   []ObjMetadata(nil),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := getObjsFromApplyLabel(tc.annotation)
			assert.Equal(t, tc.expected, result)
			assert.NoError(t, err)
		})
	}
}

func TestRemovePVClaimRef(t *testing.T) {
	t.Run("removes claimRef from a PersistentVolume", func(t *testing.T) {
		pv := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "PersistentVolume",
			"metadata":   map[string]interface{}{"name": "pv1"},
			"spec": map[string]interface{}{
				"claimRef": map[string]interface{}{
					"name":      "my-pvc",
					"namespace": "my-ns",
				},
				"capacity": map[string]interface{}{"storage": "1Gi"},
			},
		}}
		removePVClaimRef(pv)
		_, found, _ := unstructured.NestedMap(pv.Object, "spec", "claimRef")
		assert.False(t, found, "claimRef should be removed")
		// Other spec fields are preserved.
		_, found, _ = unstructured.NestedMap(pv.Object, "spec", "capacity")
		assert.True(t, found, "capacity should be preserved")
	})

	t.Run("no-op for non-PersistentVolume resources", func(t *testing.T) {
		svc := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Service",
			"metadata":   map[string]interface{}{"name": "svc1"},
			"spec": map[string]interface{}{
				"claimRef": map[string]interface{}{"name": "should-stay"},
			},
		}}
		removePVClaimRef(svc)
		_, found, _ := unstructured.NestedMap(svc.Object, "spec", "claimRef")
		assert.True(t, found, "non-PV resources must be left untouched")
	})
}

func TestPatchPVsReclaimPolicy(t *testing.T) {
	testcases := []struct {
		name          string
		pvs           []client.Object
		expectedPatch map[string]corev1.PersistentVolumeReclaimPolicy
	}{
		{
			name: "No PVs",
			pvs:  []client.Object{},
		},
		{
			name: "PV with topolvm annotation and Delete policy gets patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv1",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv1": corev1.PersistentVolumeReclaimRetain,
			},
		},
		{
			name: "PV without topolvm annotation is not patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "pv2",
						Annotations: map[string]string{},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv2": corev1.PersistentVolumeReclaimDelete,
			},
		},
		{
			name: "PV with topolvm annotation but Retain policy is not patched",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv3",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv3": corev1.PersistentVolumeReclaimRetain,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(tc.pvs...)
			if err != nil {
				t.Fatalf("error creating fake client: %v", err)
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			err = handler.PatchPVsReclaimPolicy(context.Background())
			assert.NoError(t, err)

			// Verify results
			pvList := &corev1.PersistentVolumeList{}
			err = fakeClient.List(context.Background(), pvList)
			assert.NoError(t, err)

			for _, pv := range pvList.Items {
				if expected, ok := tc.expectedPatch[pv.Name]; ok {
					assert.Equal(t, expected, pv.Spec.PersistentVolumeReclaimPolicy, "PV %s", pv.Name)
				}
			}
		})
	}
}

func TestRestorePVsReclaimPolicy(t *testing.T) {
	testcases := []struct {
		name          string
		pvs           []client.Object
		expectedPatch map[string]corev1.PersistentVolumeReclaimPolicy
	}{
		{
			name: "No PVs",
			pvs:  []client.Object{},
		},
		{
			name: "PV with updated-reclaim-policy annotation gets restored",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv1",
						Annotations: map[string]string{
							updatedReclaimPolicyAnnotation: "true",
							topolvmAnnotation:              topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv1": corev1.PersistentVolumeReclaimDelete,
			},
		},
		{
			name: "PV without updated-reclaim-policy annotation is not changed",
			pvs: []client.Object{
				&corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv2",
						Annotations: map[string]string{
							topolvmAnnotation: topolvmValue,
						},
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					},
				},
			},
			expectedPatch: map[string]corev1.PersistentVolumeReclaimPolicy{
				"pv2": corev1.PersistentVolumeReclaimRetain,
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient, err := getFakeClientFromObjects(tc.pvs...)
			if err != nil {
				t.Fatalf("error creating fake client: %v", err)
			}

			handler := &BRHandler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("BackupRestore"),
			}

			err = handler.RestorePVsReclaimPolicy(context.Background())
			assert.NoError(t, err)

			// Verify results
			pvList := &corev1.PersistentVolumeList{}
			err = fakeClient.List(context.Background(), pvList)
			assert.NoError(t, err)

			for _, pv := range pvList.Items {
				if expected, ok := tc.expectedPatch[pv.Name]; ok {
					assert.Equal(t, expected, pv.Spec.PersistentVolumeReclaimPolicy, "PV %s", pv.Name)
				}
			}
		})
	}
}
