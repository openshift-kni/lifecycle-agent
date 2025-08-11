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
	"os"
	"path/filepath"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"

	configv1 "github.com/openshift/api/config/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
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
	applyLabelAnn  = "lca.openshift.io/apply-label"
	backupLabel    = "lca.openshift.io/backup"
	clusterIDLabel = "config.openshift.io/clusterID" // label for backups applied by lifecycle agent

	OadpPath        = "/opt/OADP"
	OadpRestorePath = OadpPath + "/veleroRestore"
	OadpDpaPath     = OadpPath + "/dpa"
	OadpSecretPath  = OadpPath + "/secret"

	// OadpNs is the namespace used for everything related OADP e.g configsMaps, DataProtectionApplicationm, Restore, etc
	OadpNs                  = "openshift-adp"
	OadpMinSupportedVersion = "1.3.1"

	topolvmValue                   = "topolvm.io"
	topolvmAnnotation              = "pv.kubernetes.io/provisioned-by"
	updatedReclaimPolicyAnnotation = "lca.openshift.io/updated-reclaim-policy" // used to identify LVMS PVs updated by LCA
)

var (
	hostPath = common.Host

	DpaGvk     = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplication", Version: "v1alpha1"}
	DpaGvkList = schema.GroupVersionKind{Group: "oadp.openshift.io", Kind: "DataProtectionApplicationList", Version: "v1alpha1"}
)

// BackuperRestorer interface also used for mocks
type BackuperRestorer interface {
	CleanupBackups(ctx context.Context) error
	CleanupStaleBackups(ctx context.Context, backups []*velerov1.Backup) error
	CleanupDeleteBackupRequests(ctx context.Context) error
	CheckOadpOperatorAvailability(ctx context.Context) error
	PatchPVsReclaimPolicy(ctx context.Context) error
	RestorePVsReclaimPolicy(ctx context.Context) error
	EnsureOadpConfiguration(ctx context.Context) error
	ExportOadpConfigurationToDir(ctx context.Context, toDir, oadpNamespace string) error
	ExportRestoresToDir(ctx context.Context, configMaps []ibuv1.ConfigMapRef, toDir string) error
	GetSortedBackupsFromConfigmap(ctx context.Context, content []ibuv1.ConfigMapRef) ([][]*velerov1.Backup, error)
	LoadRestoresFromOadpRestorePath() ([][]*velerov1.Restore, error)
	StartOrTrackBackup(ctx context.Context, backups []*velerov1.Backup) (*BackupTracker, error)
	StartOrTrackRestore(ctx context.Context, restores []*velerov1.Restore) (*RestoreTracker, error)
	ValidateOadpConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef) error
	IsOadpInstalled(ctx context.Context) bool
	GetDataProtectionApplicationList(ctx context.Context) (*unstructured.UnstructuredList, error)
	CheckOadpMinimumVersion(ctx context.Context) (bool, error)
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
	return e.ErrMessage
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

// getValidBackup retrieves a backup by name and namespace, ensuring it belongs to the correct cluster.
// If the backup doesn't exist or doesn't belong to the cluster, it returns nil.
func getValidBackup(ctx context.Context, c client.Client, name, namespace string) (*velerov1.Backup, error) {
	clusterID, err := getClusterID(ctx, c)
	if err != nil {
		return nil, err
	}

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

	// Check if the backup belongs to the correct cluster
	labels := backup.GetLabels()
	if labels[clusterIDLabel] != clusterID {
		return nil, nil
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

func CreateOrUpdateSecret(ctx context.Context, secret *corev1.Secret, c client.Client) error {
	existingSecret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{
		Name:      secret.Name,
		Namespace: secret.Namespace,
	}, existingSecret)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("failed to get secret: %w", err)
		}
		// Create the secret if it does not exist
		if err := c.Create(ctx, secret); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create secret: %w", err)
			}
		}
	} else {
		secret.SetResourceVersion(existingSecret.GetResourceVersion())
		if err := c.Update(ctx, secret); err != nil {
			return fmt.Errorf("failed to update secret: %w", err)
		}
	}
	return nil
}

func CreateOrUpdateDataProtectionAppliation(ctx context.Context, dpa *unstructured.Unstructured, c client.Client) error {
	dpa.SetGroupVersionKind(DpaGvk)
	unstructured.RemoveNestedField(dpa.Object, "status")

	existingDpa := &unstructured.Unstructured{}
	existingDpa.SetGroupVersionKind(DpaGvk)
	err := c.Get(ctx, types.NamespacedName{
		Name:      dpa.GetName(),
		Namespace: dpa.GetNamespace(),
	}, existingDpa)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return fmt.Errorf("could not get DataProtectionApplication: %w", err)
		}
		// Create the DPA if it does not exist
		if err := c.Create(ctx, dpa); err != nil {
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create DataProtectionApplication: %w", err)
			}
		}
	} else {
		dpa.SetResourceVersion(existingDpa.GetResourceVersion())
		if err := c.Update(ctx, dpa); err != nil {
			return fmt.Errorf("failed to update DataProtectionApplication: %w", err)
		}
	}
	return nil
}

func setBackupLabelSelector(backup *velerov1.Backup) {
	if backup.Spec.LabelSelector == nil {
		backup.Spec.LabelSelector = &metav1.LabelSelector{}
	}
	metav1.AddLabelToSelector(backup.Spec.LabelSelector, backupLabel, backup.GetName())
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

func IsDPAReconciled(dpa *unstructured.Unstructured) bool {
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

func ReadOadpDataProtectionApplication(dpaYamlDir string) (*unstructured.Unstructured, error) {
	dpaYamls, err := os.ReadDir(dpaYamlDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read DataProtectionApplication dir %s: %w", dpaYamlDir, err)
	}
	if len(dpaYamls) == 0 {
		return nil, nil
	}

	if len(dpaYamls) > 1 {
		// Unexpected error
		return nil, fmt.Errorf("found more than one DataProtectionApplication yamls in %s", dpaYamlDir)
	}

	dpaYamlPath := filepath.Join(dpaYamlDir, dpaYamls[0].Name())
	if dpaYamls[0].IsDir() {
		// Unexpected error
		return nil, fmt.Errorf("%s is a directory instead of file", dpaYamlPath)
	}

	dpa := &unstructured.Unstructured{}
	dpa.SetGroupVersionKind(DpaGvk)
	if err := utils.ReadYamlOrJSONFile(dpaYamlPath, dpa); err != nil {
		return nil, fmt.Errorf("failed to read DataProtectionApplication from %s: %w", dpaYamlPath, err)
	}
	return dpa, nil
}

// patchObj patches the objects / resources defined in the Backup CRs of OADP.
// Also, it has a isDryRun flag that allows to simulate patching the specified resources, which is handy when
// validating the objects defined in Backup CRs within the OADP ConfigMap.
func patchObj(ctx context.Context, client dynamic.Interface, obj *ObjMetadata,
	isDryRun bool, payload []byte, patchType types.PatchType) error {
	var err error
	resourceClient := client.Resource(schema.GroupVersionResource{
		Group:    obj.Group,
		Version:  obj.Version,
		Resource: obj.Resource,
	})

	patchOptions := metav1.PatchOptions{}
	if isDryRun {
		patchOptions = metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	}

	if obj.Namespace != "" {
		_, err = resourceClient.Namespace(obj.Namespace).Patch(
			ctx, obj.Name, patchType, payload, patchOptions,
		)
	} else {
		_, err = resourceClient.Patch(ctx, obj.Name, patchType, payload, patchOptions)
	}
	if err != nil {
		return fmt.Errorf("failed to patch object: %w", err)
	}

	return nil
}

func (h *BRHandler) ValidateOadpConfigmaps(ctx context.Context, content []ibuv1.ConfigMapRef) error {
	configmaps, err := common.GetConfigMaps(ctx, h.Client, content)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			errMsg := fmt.Sprintf("OADP configmap not found, error: %s. Please create the configmap.", err.Error())
			h.Log.Error(nil, errMsg)
			return NewBRFailedValidationError("OADP", errMsg)
		}
		return fmt.Errorf("failed to oadp configMaps: %w", err)
	}

	backups, err := common.ExtractResourcesFromConfigmaps[*velerov1.Backup](configmaps, common.BackupGvk)
	if err != nil {
		return err
	}
	for _, backup := range backups {
		// Dry-run the Backup CR to detect early issues
		err := h.Create(ctx, backup, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			if k8serrors.IsInvalid(err) {
				errMsg := fmt.Sprintf("Invalid backup %s detected in configmap, error: %s. Please update the invalid Backup in configmap.",
					backup.GetName(), err.Error())
				h.Log.Error(err, errMsg)
				return NewBRFailedValidationError("backup", errMsg)
			}
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create backup with dry run: %w", err)
			}
		}

		// Check if we can apply backup label to objects included in apply-backup annotation
		payload := []byte(fmt.Sprintf(`{"metadata": {"labels": {"%s": "%s"}}}`, backupLabel, backup.GetName()))
		objs, err := getObjsFromAnnotations(backup)
		if err != nil {
			return NewBRFailedValidationError("OADP", err.Error())
		}
		for _, obj := range objs {
			err := patchObj(ctx, h.DynamicClient, &obj, true, payload, types.MergePatchType) //nolint:gosec
			if err != nil {
				return NewBRFailedValidationError("OADP", fmt.Sprintf("failed apply backup label to objects included in apply-backup annotation: %s", err.Error()))
			}
		}
	}

	restores, err := common.ExtractResourcesFromConfigmaps[*velerov1.Restore](configmaps, common.RestoreGvk)
	if err != nil {
		return err
	}
	for _, restore := range restores {
		// Dry-run the Restore CR to detect early issues
		err := h.Create(ctx, restore, &client.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			if k8serrors.IsInvalid(err) {
				errMsg := fmt.Sprintf("Invalid Restore %s detected in configmap, error: %s. Please update the invalid Restore in configmap.",
					restore.GetName(), err.Error())
				h.Log.Error(err, errMsg)
				return NewBRFailedValidationError("restore", errMsg)
			}
			if !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create Restore with dry run: %w", err)
			}
		}

		// Check if the backup CRs defined in restore CRs exist in OADP configmaps
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

	if len(backups) == 0 || len(restores) == 0 || len(backups) != len(restores) {
		errMsg := "Both backup and restore CRs should be specified in OADP configmaps and each backup CR should be paired with a corresponding restore CR."
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	// Check for any stale backup CRs in this cluster
	if err := h.CleanupStaleBackups(ctx, backups); err != nil {
		errMsg := fmt.Sprintf("Failed to cleanup stale Backups: %s", err)
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	h.Log.Info("OADP configMaps are validated", "configMaps", content)
	return nil
}

func (h *BRHandler) CheckOadpOperatorAvailability(ctx context.Context) error {
	// Check if OADP is installed
	if !h.IsOadpInstalled(ctx) {
		errMsg := fmt.Sprintf("Please ensure OADP operator is installed in the %s", OadpNs)
		h.Log.Error(nil, errMsg)
		return NewBRFailedValidationError("OADP", errMsg)
	}

	// Check if OADP DPA is reconciled
	dpaList, err := h.GetDataProtectionApplicationList(ctx)
	if err != nil {
		return err
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

	h.Log.Info("OADP operator is installed and DataProtectionApplication is validated")
	return nil
}

// IsOadpInstalled a simple function to determine if OADP is installed by checking the presence
// of its CSV and at least one OADP defined CRD
func (h *BRHandler) IsOadpInstalled(ctx context.Context) bool {
	// Check if OADP CSV is installed
	oadpCsvList := &operatorsv1alpha1.ClusterServiceVersionList{}
	if err := h.List(ctx, oadpCsvList, &client.ListOptions{Namespace: OadpNs}); err != nil {
		h.Log.Error(err, "could not list ClusterServiceVersion")
		return false
	}

	oadpCsvExist := false
	for _, csv := range oadpCsvList.Items {
		if strings.Contains(csv.Name, "oadp-operator") {
			oadpCsvExist = true
			break
		}
	}

	if !oadpCsvExist {
		return false
	}

	// Check if OADP CRDs are installed
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := h.Client.List(ctx, crds); err != nil {
		h.Log.Error(err, "could not list CRDs to verify if OADP is installed")
		return false
	}

	/*
		oadp installs more than one CRD (listed below from a dev cluster)...we are looking for at least one match
		cloudstorages.oadp.openshift.io
		dataprotectionapplications.oadp.openshift.io
		volumesnapshotbackups.datamover.oadp.openshift.io
		volumesnapshotrestores.datamover.oadp.openshift.io
	*/
	var oadpCrds []string
	for _, crd := range crds.Items {
		if strings.HasSuffix(crd.GetName(), "oadp.openshift.io") {
			oadpCrds = append(oadpCrds, crd.ObjectMeta.Name)
		}
	}

	return len(oadpCrds) > 0
}

func (h *BRHandler) GetDataProtectionApplicationList(ctx context.Context) (*unstructured.UnstructuredList, error) {
	dpaList := &unstructured.UnstructuredList{}
	dpaList.SetGroupVersionKind(DpaGvkList)

	opts := []client.ListOption{
		client.InNamespace(OadpNs),
	}

	if err := h.List(ctx, dpaList, opts...); err != nil {
		return dpaList, fmt.Errorf("failed to list dpa: %w", err)
	}

	return dpaList, nil
}

// CheckOadpMinimumVersion checks the minimum supported version for the OADP operator version from the installed CSV.
func (h *BRHandler) CheckOadpMinimumVersion(ctx context.Context) (bool, error) {
	oadpCsvList := &operatorsv1alpha1.ClusterServiceVersionList{}
	if err := h.Client.List(ctx, oadpCsvList, &client.ListOptions{Namespace: OadpNs}); err != nil {
		return false, fmt.Errorf("failed to list ClusterServiceVersions in namespace %s: %w", OadpNs, err)
	}

	minSupportedVersion, err := semver.Parse(OadpMinSupportedVersion)
	if err != nil {
		return false, fmt.Errorf("failed to parse minimum supported version: %w", err)
	}

	for _, csv := range oadpCsvList.Items {
		if !strings.Contains(csv.Name, "oadp-operator") {
			continue
		}

		currentInstalledVersion, err := semver.Parse(csv.Spec.Version.String())
		if err != nil {
			return false, fmt.Errorf("failed to parse OADP operator version: %w", err)
		}
		if currentInstalledVersion.GTE(minSupportedVersion) {
			return true, nil
		}

		h.Log.Info(fmt.Sprintf("oadp operator version %s is below the minimum supported version %s", currentInstalledVersion, OadpMinSupportedVersion))
		return false, nil
	}

	return false, fmt.Errorf("failed to find CSV for OADP operator")
}
