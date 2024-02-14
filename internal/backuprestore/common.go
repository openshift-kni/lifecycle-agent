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
	"errors"
	"fmt"
	"math"

	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/internal/common"

	lcav1alpha1 "github.com/openshift-kni/lifecycle-agent/api/v1alpha1"

	configv1 "github.com/openshift/api/config/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;update;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=backups,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=velero.io,resources=restores,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=velero.io,resources=backupstoragelocations,verbs=get;list;watch
// +kubebuilder:rbac:groups=velero.io,resources=deletebackuprequests,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=subscriptions,verbs=get;list;delete;watch
// +kubebuilder:rbac:groups=operators.coreos.com,resources=clusterserviceversions,verbs=get;list;delete;watch
// +kubebuilder:rbac:groups=oadp.openshift.io,resources=dataprotectionapplications,verbs=get;list;create;update;watch

const (
	applyWaveAnn     = "lca.openshift.io/apply-wave"
	applyLabelAnn    = "lca.openshift.io/apply-label"
	backupLabel      = "lca.openshift.io/backup"
	clusterIDLabel   = "config.openshift.io/clusterID" // label for backups applied by lifecycle agent
	defaultApplyWave = math.MaxInt32                   // 2147483647, an enough large number

	OadpPath        = "/opt/OADP"
	OadpRestorePath = OadpPath + "/veleroRestore"
	oadpDpaPath     = OadpPath + "/dpa"
	oadpSecretPath  = OadpPath + "/secret"

	// OadpNs is the namespace used for everything related OADP e.g configsMaps, DataProtectionApplicationm, Restore, etc
	OadpNs = "openshift-adp"
)

var (
	hostPath = common.Host

	dpaGvk     = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplication", Version: "v1alpha1"}
	dpaGvkList = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplicationList", Version: "v1alpha1"}
	backupGvk  = schema.GroupVersionKind{Group: "velero.io", Kind: "Backup", Version: "v1"}
	restoreGvk = schema.GroupVersionKind{Group: "velero.io", Kind: "Restore", Version: "v1"}
)

// BackuperRestorer interface also used for mocks
type BackuperRestorer interface {
	CleanupBackups(ctx context.Context) (bool, error)
	CheckOadpOperatorAvailability(ctx context.Context) error
	ExportOadpConfigurationToDir(ctx context.Context, toDir, oadpNamespace string) error
	ExportRestoresToDir(ctx context.Context, configMaps []lcav1alpha1.ConfigMapRef, toDir string) error
	GetSortedBackupsFromConfigmap(ctx context.Context, content []lcav1alpha1.ConfigMapRef) ([][]*velerov1.Backup, error)
	LoadRestoresFromOadpRestorePath() ([][]*velerov1.Restore, error)
	RestoreOadpConfigurations(ctx context.Context) error
	StartOrTrackBackup(ctx context.Context, backups []*velerov1.Backup) (*BackupTracker, error)
	StartOrTrackRestore(ctx context.Context, restores []*velerov1.Restore) (*RestoreTracker, error)
	ValidateOadpConfigmap(ctx context.Context, content []lcav1alpha1.ConfigMapRef) error
}

// BRHandler handles the backup and restore
type BRHandler struct {
	client.Client
	DynamicClient dynamic.Interface
	Log           logr.Logger
}

// BRStatusError type
type BRStatusError struct {
	Type       string
	Reason     string
	ErrMessage string
}

type ObjMetadata struct {
	Group     string
	Version   string
	Resource  string
	Namespace string
	Name      string
}

func (e *BRStatusError) Error() string {
	return fmt.Sprintf(e.ErrMessage)
}

func NewBRNotFoundError(msg string) *BRStatusError {
	return &BRStatusError{
		Type:       "configmap",
		Reason:     "NotFound",
		ErrMessage: msg,
	}
}

func NewBRFailedError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "Failed",
		ErrMessage: msg,
	}
}

func NewBRFailedValidationError(brType, msg string) *BRStatusError {
	return &BRStatusError{
		Type:       brType,
		Reason:     "FailedValidation",
		ErrMessage: msg,
	}
}

func NewBRStorageBackendUnavailableError(msg string) *BRStatusError {
	return &BRStatusError{
		Type:       "StorageBackend",
		Reason:     "Unavailable",
		ErrMessage: msg,
	}
}

func IsBRNotFoundError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "configmap" {
			return brErr.Reason == "NotFound"
		}
	}
	return false
}

func IsBRFailedError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" || brErr.Type == "OADP" {
			return brErr.Reason == "Failed"
		}
	}
	return false
}

func IsBRFailedValidationError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "Backup" || brErr.Type == "Restore" || brErr.Type == "OADP" {
			return brErr.Reason == "FailedValidation"
		}
	}
	return false
}

func IsBRStorageBackendUnavailableError(err error) bool {
	var brErr *BRStatusError
	if errors.As(err, &brErr) {
		if brErr.Type == "StorageBackend" {
			return brErr.Reason == "Unavailable"
		}
	}
	return false
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
		return nil, fmt.Errorf("failed to get backup %s: %w", backup.GetName(), err)
	}

	return backup, nil
}

func getClusterID(ctx context.Context, c client.Client) (string, error) {

	clusterVersion := &configv1.ClusterVersion{}
	if err := c.Get(ctx, types.NamespacedName{
		Name: "version",
	}, clusterVersion); err != nil {
		return "", fmt.Errorf("failed to get ClusterVersion: %w", err)
	}

	return string(clusterVersion.Spec.ClusterID), nil
}

func setBackupLabelSelector(backup *velerov1.Backup) {
	if backup.Spec.LabelSelector == nil {
		backup.Spec.LabelSelector = &metav1.LabelSelector{}
	}
	metav1.AddLabelToSelector(backup.Spec.LabelSelector, backupLabel, "true")
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

func (h *BRHandler) createObjectWithDryRun(ctx context.Context, object *unstructured.Unstructured, cm string) error {
	// Create the resource in dry-run mode to detect any validation errors in the CR
	// i.e., missing required fields
	err := h.Create(ctx, object, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		if k8serrors.IsInvalid(err) {
			errMsg := fmt.Sprintf("Invalid %s %s detected in configmap %s, error: %s. Please update the invalid CRs in configmap.",
				object.GetKind(), object.GetName(), cm, err.Error())
			h.Log.Error(err, errMsg)
			return NewBRFailedValidationError(object.GetKind(), errMsg)
		}

		if !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("failed to create configMap with dry run: %w", err)
		}
	}
	return nil
}

func isDPAReconciled(dpa *unstructured.Unstructured) bool {
	if dpa.Object["status"] == nil {
		return false
	}

	dpaStatus := dpa.Object["status"].(map[string]any)
	if dpaStatus["conditions"] == nil {
		return false
	}

	dpaStatusConditions := dpaStatus["conditions"].([]any)
	for _, condition := range dpaStatusConditions {
		conditionMap := condition.(map[string]any)
		if conditionMap["type"] == "Reconciled" {
			return conditionMap["status"] == "True"
		}
	}
	return false
}

func patchObj(ctx context.Context, client dynamic.Interface, obj *ObjMetadata, isDryRun bool, payload []byte) error {
	patchOptions := metav1.PatchOptions{}
	if isDryRun {
		patchOptions = metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	}
	resourceClient := client.Resource(schema.GroupVersionResource{
		Group: obj.Group, Version: obj.Version, Resource: obj.Resource},
	)
	var err error
	if obj.Namespace != "" {
		_, err = resourceClient.Namespace(obj.Namespace).Patch(
			ctx, obj.Name, types.JSONPatchType, payload, patchOptions,
		)
	} else {
		_, err = resourceClient.Patch(ctx, obj.Name, types.JSONPatchType, payload, patchOptions)
	}
	if err != nil {
		return fmt.Errorf("failed to patch object: %w", err)
	}

	return nil
}

func (h *BRHandler) ValidateOadpConfigmap(ctx context.Context, content []lcav1alpha1.ConfigMapRef) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("OADP configmap not found, error: %s. Please create the configmap.", err.Error())
			h.Log.Error(nil, errMsg)
			return NewBRFailedValidationError("OADP", errMsg)
		}
		return fmt.Errorf("failed to oadp configMaps: %w", err)
	}

	backups, err := h.extractBackupFromConfigmaps(ctx, configmaps)
	if err != nil {
		return err
	}
	restores, err := h.extractRestoreFromConfigmaps(ctx, configmaps)
	if err != nil {
		return err
	}

	if len(backups) == 0 || len(restores) == 0 || len(backups) != len(restores) {
		errMsg := "Both backup and restore CRs should be specified in OADP configmaps and each backup CR should be paired with a corresponding restore CR."
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	// Check if we can apply backup label to objects included in apply-backup annotation
	payload := []byte(fmt.Sprintf(`[{"op":"add","path":"/metadata/labels","value":{"%s":"true"}}]`, backupLabel))
	for _, backup := range backups {
		objs, err := getObjsFromAnnotations(backup)
		if err != nil {
			return NewBRFailedValidationError("OADP", err.Error())
		}
		for _, obj := range objs {
			err := patchObj(ctx, h.DynamicClient, &obj, true, payload) //nolint:gosec
			if err != nil {
				return NewBRFailedValidationError("OADP", fmt.Sprintf("failed apply backup label to objects included in apply-backup annotation: %s", err.Error()))
			}
		}
	}

	// Check if the backup CRs defined in restore CRs exist in OADP configmaps
	for _, restore := range restores {
		found := false
		for _, backup := range backups {
			if restore.Spec.BackupName == backup.Name {
				found = true
				break
			}
		}
		if !found {
			errMsg := fmt.Sprintf("The backup CR %s defined in restore CR %s not found in OADP configmaps", restore.Spec.BackupName, restore.Name)
			h.Log.Error(nil, errMsg)
			return NewBRFailedValidationError("OADP", errMsg)
		}
	}
	return nil
}

func (h *BRHandler) CheckOadpOperatorAvailability(ctx context.Context) error {
	// Check if OADP is running
	oadpCsv := &operatorsv1alpha1.ClusterServiceVersionList{}
	if err := h.List(ctx, oadpCsv, &client.ListOptions{Namespace: OadpNs}); err != nil {
		return fmt.Errorf("could not list ClusterServiceVersion: %w", err)
	}

	if len(oadpCsv.Items) == 0 ||
		!(oadpCsv.Items[0].Status.Phase == operatorsv1alpha1.CSVPhaseSucceeded && oadpCsv.Items[0].Status.Reason == operatorsv1alpha1.CSVReasonInstallSuccessful) {
		errMsg := fmt.Sprintf("Please ensure OADP operator is running successfully in the %s", OadpNs)
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	// Check if OADP DPA is reconciled
	dpaList := &unstructured.UnstructuredList{}
	dpaList.SetGroupVersionKind(dpaGvkList)
	opts := []client.ListOption{
		client.InNamespace(OadpNs),
	}
	if err := h.List(ctx, dpaList, opts...); err != nil {
		return fmt.Errorf("failed to list dpa: %w", err)
	}

	if len(dpaList.Items) == 0 {
		errMsg := fmt.Sprintf("No DataProtectionApplication CR found in the %s", OadpNs)
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	if len(dpaList.Items) != 1 {
		errMsg := fmt.Sprintf("Only one DataProtectionApplication CR is allowed in the %s,", OadpNs)
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	if !isDPAReconciled(&dpaList.Items[0]) {
		errMsg := fmt.Sprintf("DataProtectionApplication CR %s is not reconciled", dpaList.Items[0].GetName())
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	// Check if the storage backend is ready
	err := h.ensureStorageBackendAvailable(ctx, OadpNs)
	if err != nil {
		return NewBRFailedValidationError("OADP", err.Error())
	}
	return nil
}
