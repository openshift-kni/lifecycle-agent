/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/lcenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backuprestore

import (
	"context"
	"math"
	"os"

	ranv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"
	configv1 "github.com/openshift/api/config/v1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list
// +kubebuilder:rbac:groups=velero.io/v1,resources=backups,verbs=get;list;delete;create;update
// +kubebuilder:rbac:groups=velero.io/v1,resources=restores,verbs=get;list;delete;create;update

var log = ctrl.Log.WithName("backuprestore")

const (
	applyWaveAnn     = "lca.openshift.io/apply-wave"
	clusterIDLabel   = "config.openshift.io/clusterID" // label for backups applied by lifecycle agent
	defaultApplyWave = math.MaxInt32                   // 2147483647, an enough large number

	defaultStorageSecret = "cloud-credentials"

	oadpRestoreDir = "/OADP/veleroRestore"
	oadpDpaDir     = "/OADP/dpa"
	oadpSecretDir  = "/OADP/secret"
	oadpPackage    = "redhat-oadp-operator"
)

// BackupPhase defines the phase of backup
type BackupPhase string

// Constants for backup phase
const (
	BackupPending          BackupPhase = "BackupPending"
	BackupFailedValidation BackupPhase = "BackupFailedValidation"
	BackupFailed           BackupPhase = "BackupFailed"
	BackupCompleted        BackupPhase = "BackupCompleted"
	BackupInProgress       BackupPhase = "BackupInProgress"
)

// RestorePhase defines the phase of restore
type RestorePhase string

// Constants for restore phase
const (
	RestorePending    RestorePhase = "RestorePending"
	RestoreFailed     RestorePhase = "RestoreFailed"
	RestoreCompleted  RestorePhase = "RestoreCompleted"
	RestoreInProgress RestorePhase = "RestoreInProgress"
)

var (
	dpaGvk     = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplication", Version: "v1alpha1"}
	dpaGvkList = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplicationList", Version: "v1alpha1"}
	backupGvk  = schema.GroupVersionKind{Group: "velero.io", Kind: "Backup", Version: "v1"}
	restoreGvk = schema.GroupVersionKind{Group: "velero.io", Kind: "Restore", Version: "v1"}
)

// BackupStatus defines the status of backup
type BackupStatus struct {
	Status  BackupPhase
	Message string
}

// RestoreStatus defines the status of restore
type RestoreStatus struct {
	Status  RestorePhase
	Message string
}

// getConfigMaps restrieves the configmaps from cluster
func getConfigMaps(ctx context.Context, c client.Client, configMaps []ranv1alpha1.ConfigMapRef) ([]corev1.ConfigMap, error) {
	var cms []corev1.ConfigMap

	for _, cm := range configMaps {
		existingCm := &corev1.ConfigMap{}
		err := c.Get(ctx, types.NamespacedName{
			Name:      cm.Name,
			Namespace: cm.Namespace,
		}, existingCm)

		if err != nil {
			return nil, err
		}
		cms = append(cms, *existingCm)
	}

	return cms, nil
}

func writeSecretToFile(secret *corev1.Secret, filePath string) error {
	secretBytes, err := yaml.Marshal(secret)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, secretBytes, 0o644); err != nil {
		return err
	}
	return nil
}

func writeDpaToFile(dpa *unstructured.Unstructured, filePath string) error {
	dpaBytes, err := yaml.Marshal(dpa)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, dpaBytes, 0o644); err != nil {
		return err
	}
	return nil
}

func writeRestoreToFile(restore *velerov1.Restore, filePath string) error {
	data, err := yaml.Marshal(restore)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return err
	}
	return nil
}

func getBackup(ctx context.Context, c client.Client, name, namespace string) (*velerov1.Backup, error) {
	backup := &velerov1.Backup{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, backup); err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return backup, nil
}

func getClusterID(ctx context.Context, c client.Client) (string, error) {
	clusterVersion := &configv1.ClusterVersion{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: "version",
	}, clusterVersion); err != nil {
		return "", err
	}

	return string(clusterVersion.Spec.ClusterID), nil
}

func setBackupLabel(backup *velerov1.Backup, newLabels map[string]string) {
	labels := backup.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	for k, v := range newLabels {
		labels[k] = v
	}
	backup.SetLabels(labels)
}

// nolint:unused
// TODO: remove oadp operator
func deleteOperator() {
}

// nolint:unused
// TODO: delete backups
func deleteBackups() {
}
