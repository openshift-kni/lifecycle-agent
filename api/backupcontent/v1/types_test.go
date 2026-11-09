/*
Copyright 2026.

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

package v1

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBackupContentSpecExported is a compile-time assertion that BackupContentSpec
// and its field types are importable and embeddable from the public API package,
// as the hub requires. If any of these types stopped being exported, this file
// would not compile.
func TestBackupContentSpecExported(t *testing.T) {
	wave := int32(3)
	var spec BackupContentSpec = BackupContentSpec{
		BackupWave:          &wave,
		RestoreWave:         &wave,
		Namespaces:          []string{"open-cluster-management-agent"},
		NamespacedResources: []ResourceSelector{{Kind: "secrets", Names: []string{"a"}}},
		ClusterScopedResources: []ResourceSelector{
			{Kind: "clusterroles.rbac.authorization.k8s.io"},
		},
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "acm"}},
	}
	if spec.BackupWave == nil || *spec.BackupWave != 3 {
		t.Fatalf("BackupContentSpec not constructed as expected: %+v", spec)
	}
}

// TestBackupContentSpecDeepCopy exercises the generated DeepCopy for
// BackupContentSpec across its pointer, slice and LabelSelector fields, and
// confirms the copy is deep (mutating the copy does not affect the original).
func TestBackupContentSpecDeepCopy(t *testing.T) {
	backupWave := int32(1)
	restoreWave := int32(2)
	original := &BackupContentSpec{
		BackupWave:  &backupWave,
		RestoreWave: &restoreWave,
		Namespaces:  []string{"ns1", "ns2"},
		NamespacedResources: []ResourceSelector{
			{Kind: "deployments.apps", Names: []string{"klusterlet"}},
			{Kind: "configmaps"},
		},
		ExcludedNamespacedResources: []ResourceSelector{
			{Kind: "secrets", Names: []string{"do-not-restore"}},
		},
		ClusterScopedResources: []ResourceSelector{
			{Kind: "clusterroles.rbac.authorization.k8s.io", Names: []string{"klusterlet"}},
		},
		ExcludedClusterScopedResources: []ResourceSelector{
			{Kind: "clusterroles.rbac.authorization.k8s.io", Names: []string{"do-not-restore"}},
		},
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "acm"}},
	}

	cp := original.DeepCopy()
	if !reflect.DeepEqual(original, cp) {
		t.Fatalf("DeepCopy produced a different value:\n got: %+v\nwant: %+v", cp, original)
	}

	// Mutate the copy and confirm the original is untouched (true deep copy).
	*cp.BackupWave = 99
	cp.Namespaces[0] = "mutated"
	cp.NamespacedResources[0].Names[0] = "mutated"
	cp.LabelSelector.MatchLabels["app"] = "mutated"

	if *original.BackupWave != 1 {
		t.Errorf("original BackupWave mutated via copy: got %d, want 1", *original.BackupWave)
	}
	if original.Namespaces[0] != "ns1" {
		t.Errorf("original Namespaces mutated via copy: got %q, want %q", original.Namespaces[0], "ns1")
	}
	if original.NamespacedResources[0].Names[0] != "klusterlet" {
		t.Errorf("original NamespacedResources mutated via copy: got %q, want %q",
			original.NamespacedResources[0].Names[0], "klusterlet")
	}
	if original.LabelSelector.MatchLabels["app"] != "acm" {
		t.Errorf("original LabelSelector mutated via copy: got %q, want %q",
			original.LabelSelector.MatchLabels["app"], "acm")
	}
}
