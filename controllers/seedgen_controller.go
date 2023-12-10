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

package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	seedgenv1alpha1 "github.com/openshift-kni/lifecycle-agent/api/seedgenerator/v1alpha1"
	"github.com/openshift-kni/lifecycle-agent/controllers/utils"

	"github.com/openshift-kni/lifecycle-agent/ibu-imager/clusterinfo"
	"github.com/openshift-kni/lifecycle-agent/ibu-imager/ops"
	"github.com/openshift-kni/lifecycle-agent/internal/common"

	commonUtils "github.com/openshift-kni/lifecycle-agent/utils"
)

// SeedGeneratorReconciler reconciles a SeedGenerator object
type SeedGeneratorReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Executor ops.Execute
}

var (
	clusterName            string
	lcaImage               string
	seedgenWorkingDir      = "/tmp/ibu-seedgen-orch"
	seedgenAuthFile        = filepath.Join(seedgenWorkingDir, "auth.json")
	storedSeedgenCR        = filepath.Join(seedgenWorkingDir, "seedgen-cr.json")
	storedSeedgenSecret    = filepath.Join(seedgenWorkingDir, "seedgen-secret.json")
	storedManagedClusterCR = filepath.Join(seedgenWorkingDir, "managedcluster.json")
)

//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=delete
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=managedclusters,verbs=get;list;watch;delete

// Create an API client for hub requests (ACM)
func (r *SeedGeneratorReconciler) createHubClient(hubKubeconfig []byte) (hubClient client.Client, err error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(hubKubeconfig)
	if err != nil {
		err = fmt.Errorf("failed RESTConfigFromKubeConfig: %w", err)
		return
	}

	hubClient, err = client.New(config, client.Options{Scheme: r.Scheme})
	if err != nil {
		err = fmt.Errorf("failed to create hub client: %w", err)
		return
	}

	return
}

// Collect and save the data needed to restore the ACM registration, then delete the managedcluster from the hub
func (r *SeedGeneratorReconciler) deregisterFromHub(ctx context.Context, hubClient client.Client) error {
	//nolint:gocritic // TODO: Cleanup when we definitely don't need the importsecret
	// // Save ACM import data
	// importSecretName := fmt.Sprintf("%s-import", clusterName)
	// importSecret := &corev1.Secret{}
	// if err := hubClient.Get(ctx, types.NamespacedName{Name: importSecretName, Namespace: clusterName}, importSecret); err != nil {
	// 	// If not found, do nothing.
	// 	return client.IgnoreNotFound(err)
	// }
	//
	// savedfile := filepath.Join(seedgenWorkingDir, "importsecret.json")
	// if err := commonUtils.MarshalToFile(importSecret, common.PathOutsideChroot(savedfile)); err != nil {
	// 	return fmt.Errorf("failed to write importsecret to %s: %w", savedfile, err)
	// }
	//
	// if importYaml, exists := importSecret.Data["import.yaml"]; exists {
	// 	filename := filepath.Join(seedgenWorkingDir, "acm-import.yaml")
	// 	if err := r.stringToFile(filename, string(importYaml)); err != nil {
	// 		return fmt.Errorf("failed to write %s: %w", filename, err)
	// 	}
	// }
	//
	// if crdsYaml, exists := importSecret.Data["crds.yaml"]; exists {
	// 	filename := filepath.Join(seedgenWorkingDir, "acm-crds.yaml")
	// 	if err := r.stringToFile(filename, string(crdsYaml)); err != nil {
	// 		return fmt.Errorf("failed to write %s: %w", filename, err)
	// 	}
	// }

	// Save the managedcluster
	managedcluster := &clusterv1.ManagedCluster{}
	if err := hubClient.Get(ctx, types.NamespacedName{Name: clusterName}, managedcluster); err != nil {
		// If not found, do nothing.
		return client.IgnoreNotFound(err)
	}

	// The hubClient.Get() request isn't setting the GVK, so do it using the scheme data
	// TODO: This may be due to issues with the RESTMapper or Scheme, where this CRD doesn't exist
	// on the SNO, so maybe we need a separate resource discovery mechanism, or distinct scheme?
	typeMeta, err := commonUtils.TypeMetaForObject(r.Scheme, managedcluster)
	if err != nil {
		return err
	}
	managedcluster.TypeMeta = *typeMeta

	if err := commonUtils.MarshalToFile(managedcluster, common.PathOutsideChroot(storedManagedClusterCR)); err != nil {
		return fmt.Errorf("failed to write managedcluster to %s: %w", storedManagedClusterCR, err)
	}

	// Ensure that the dependent resources are deleted
	deleteOpts := []client.DeleteOption{
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	}

	// Deregister from ACM on the hub
	if err := hubClient.Delete(ctx, managedcluster, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete managedcluster from hub: %w", err)
	}

	// TODO: For some reason, the managedcluster deletion is returning immediately, rather than
	// blocking while the deletion occurs in the foreground. Maybe because it's deleting on the hub?
	// As a workaround, we'll poll until the cluster is deleted.
	interval := 10 * time.Second
	maxRetries := 90 // ~15 minutes
	current := 0
	r.Log.Info("Waiting until managedcluster is deleted")
	for r.managedClusterExists(ctx, hubClient) {
		if current < maxRetries {
			time.Sleep(interval)
			current += 1
		} else {
			return fmt.Errorf("timed out waiting for managedcluster deletion")
		}
	}

	return nil
}

// Check whether the managedcluster resource exists on the hub
func (r *SeedGeneratorReconciler) managedClusterExists(ctx context.Context, hubClient client.Client) bool {
	managedcluster := &clusterv1.ManagedCluster{}
	if err := hubClient.Get(ctx, types.NamespacedName{Name: clusterName}, managedcluster); err != nil {
		if client.IgnoreNotFound(err) != nil {
			r.Log.Info(fmt.Sprintf("Error when checking managedcluster existence: %s", err.Error()))
		}
		return false
	}
	return true
}

// Get a list of ACM addon namespaces present on the cluster
func (r *SeedGeneratorReconciler) currentAcmAddonNamespaces(ctx context.Context) (acmNsList []string) {
	namespaces := &corev1.NamespaceList{}
	if err := r.Client.List(ctx, namespaces); err != nil {
		if client.IgnoreNotFound(err) != nil {
			r.Log.Info(fmt.Sprintf("Error when checking namespaces: %s", err.Error()))
		}
		return
	}

	// Find all namespaces that start with "open-cluster-management-addon-" prefix
	re := regexp.MustCompile(`^open-cluster-management-addon-`)
	for _, ns := range namespaces.Items {
		if re.MatchString(ns.ObjectMeta.Name) {
			acmNsList = append(acmNsList, ns.ObjectMeta.Name)
		}
	}
	return
}

// Get a list of existing ACM namespaces on the cluster
func (r *SeedGeneratorReconciler) currentAcmNamespaces(ctx context.Context) (acmNsList []string) {
	namespaces := &corev1.NamespaceList{}
	if err := r.Client.List(ctx, namespaces); err != nil {
		if client.IgnoreNotFound(err) != nil {
			r.Log.Info(fmt.Sprintf("Error when checking namespaces: %s", err.Error()))
		}
		return
	}

	re := regexp.MustCompile(`^open-cluster-management-agent`)
	for _, ns := range namespaces.Items {
		if re.MatchString(ns.ObjectMeta.Name) {
			acmNsList = append(acmNsList, ns.ObjectMeta.Name)
		}
	}
	return
}

// Get a list of existing ACM CRDs on the cluster
func (r *SeedGeneratorReconciler) currentAcmCrds(ctx context.Context) (acmCrdList []string) {
	crds := &apiextensionsv1.CustomResourceDefinitionList{}
	if err := r.Client.List(ctx, crds); err != nil {
		if client.IgnoreNotFound(err) != nil {
			r.Log.Info(fmt.Sprintf("Error when checking namespaces: %s", err.Error()))
		}
		return
	}

	re := regexp.MustCompile(`\.open-cluster-management\.io$`)
	for _, crd := range crds.Items {
		if re.MatchString(crd.ObjectMeta.Name) {
			acmCrdList = append(acmCrdList, crd.ObjectMeta.Name)
		}
	}
	return
}

// Clean up ACM and other resources on the cluster
func (r *SeedGeneratorReconciler) cleanupClusterResources(ctx context.Context) error {
	// Ensure that the dependent resources are deleted
	deleteOpts := []client.DeleteOption{
		client.PropagationPolicy(metav1.DeletePropagationForeground),
	}

	interval := 10 * time.Second
	maxRetries := 90 // ~15 minutes

	// Trigger deletion for any remaining ACM namespaces
	acmNamespaces := r.currentAcmNamespaces(ctx)
	if len(acmNamespaces) > 0 {
		r.Log.Info("Deleting ACM namespaces")
		for _, nsName := range r.currentAcmNamespaces(ctx) {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
				}}
			r.Log.Info(fmt.Sprintf("Deleting namespace %s", nsName))
			if err := r.Client.Delete(ctx, ns, deleteOpts...); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete namespace %s: %w", nsName, err)
			}
		}

		// Verify ACM namespaces have been deleted
		current := 0
		r.Log.Info("Waiting until ACM namespaces are deleted")
		for len(r.currentAcmNamespaces(ctx)) > 0 {
			if current < maxRetries {
				time.Sleep(interval)
				current += 1
			} else {
				return fmt.Errorf("timed out waiting for ACM namespace deletion")
			}
		}
	} else {
		r.Log.Info("No ACM namespaces found")
	}

	// Trigger deletion for any remaining ACM CRDs
	acmCrds := r.currentAcmCrds(ctx)
	if len(acmCrds) > 0 {
		r.Log.Info("Deleting ACM CRDs")

		for _, crdName := range r.currentAcmCrds(ctx) {
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: crdName,
				}}
			r.Log.Info(fmt.Sprintf("Deleting CRD %s", crdName))
			if err := r.Client.Delete(ctx, crd, deleteOpts...); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete CRD %s: %w", crdName, err)
			}
		}

		// Verify ACM CRDs have been deleted
		current := 0
		r.Log.Info("Waiting until ACM CRDs are deleted")
		for len(r.currentAcmCrds(ctx)) > 0 {
			if current < maxRetries {
				time.Sleep(interval)
				current += 1
			} else {
				return fmt.Errorf("timed out waiting for ACM CRD deletion")
			}
		}
	} else {
		r.Log.Info("No ACM CRDs found")
	}

	// Delete remaining cluster resources leftover from ACM (or install)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "assisted-installer",
		}}
	if err := r.Client.Delete(ctx, ns, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete assisted-installer namespace: %w", err)
	}

	roles := []string{
		"klusterlet",
		"klusterlet-bootstrap-kubeconfig",
		"open-cluster-management:klusterlet-admin-aggregate-clusterrole",
	}
	for _, role := range roles {
		roleStruct := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{
				Name: role,
			}}
		if err := r.Client.Delete(ctx, roleStruct, deleteOpts...); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete clusterrole %s: %w", role, err)
		}
	}

	roleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "klusterlet",
		}}
	if err := r.Client.Delete(ctx, roleBinding, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete klusterlet clusterrolebinding: %w", err)
	}

	// If observability is enabled, there may be a copy of the accessor secret in openshift-monitoring namespace
	observabilitySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "openshift-monitoring",
			Name:      "observability-alertmanager-accessor",
		}}
	if err := r.Client.Delete(ctx, observabilitySecret, deleteOpts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete observability secret: %w", err)
	}

	return nil
}

// Get the LCA image ref
// TODO: Is there a better way to access the image ref?
func (r *SeedGeneratorReconciler) getLcaImage(ctx context.Context) (image string, err error) {
	pod := &corev1.Pod{}
	if err = r.Client.Get(ctx, types.NamespacedName{Name: os.Getenv("MY_POD_NAME"), Namespace: os.Getenv("MY_POD_NAMESPACE")}, pod); err != nil {
		err = fmt.Errorf("failed to get pod info: %w", err)
		return
	}

	for _, container := range pod.Spec.Containers {
		if container.Name == "manager" {
			image = container.Image
			return
		}
	}

	err = fmt.Errorf("unable to determine LCA image")
	return
}

// Launch a container to run the ibu-imager
func (r *SeedGeneratorReconciler) launchImager(seedgen *seedgenv1alpha1.SeedGenerator) error {
	r.Log.Info("Launching ibu-imager")
	recertImage := seedgen.Spec.RecertImage
	if recertImage == "" {
		recertImage = common.DefaultRecertImage
	}

	imagerCmdArgs := []string{
		"podman", "run", "--privileged", "--pid=host", "--name=ibu_imager", "--replace", "--net=host",
		"-v", "/etc:/etc", "-v", "/var:/var", "-v", "/var/run:/var/run", "-v", "/run/systemd/journal/socket:/run/systemd/journal/socket",
		"-v", fmt.Sprintf("%s:%s", seedgenAuthFile, seedgenAuthFile),
		"--entrypoint", "ibu-imager",
		lcaImage,
		"create",
		"--authfile", seedgenAuthFile,
		"--image", seedgen.Spec.SeedImage,
		"--recert-image", recertImage,
	}

	// In order to have the ibu-imager container both survive the LCA pod shutdown and have continued network access
	// after all other pods are shutdown, we're using systemd-run to launch it as a transient service-unit
	systemdRunOpts := []string{"--collect", "--wait", "--unit", "lca-generate-seed-image"}
	if _, err := r.Executor.Execute("systemd-run", append(systemdRunOpts, imagerCmdArgs...)...); err != nil {
		return fmt.Errorf("failed to run ibu-imager container: %w", err)
	}

	// We should never get here, as the ibu-imager will shutdown this pod
	return nil
}

// Check whether the system can be used for seed generation
func (r *SeedGeneratorReconciler) validateSystem(ctx context.Context) (msg string) {
	// Ensure there are no ACM addons enabled on the seed SNO
	if acmNsList := r.currentAcmAddonNamespaces(ctx); len(acmNsList) > 0 {
		msg = fmt.Sprintf("Rejected due to presence of ACM addon(s): %s", strings.Join(acmNsList, ", "))
		return
	}

	// TODO: Remove this dnsmasq check once ACM includes it? Or should we just keep it regardless, for dev systems not installed via ACM?
	dnsmasqConfigScript := "/usr/local/bin/dnsmasq_config.sh"
	if _, err := os.Stat(common.PathOutsideChroot(dnsmasqConfigScript)); os.IsNotExist(err) {
		msg = "Rejected due to system missing dnsmasq config required for IBU"
		return
	}

	return
}

// Generate the seed image
func (r *SeedGeneratorReconciler) generateSeedImage(ctx context.Context, seedgen *seedgenv1alpha1.SeedGenerator) error {
	workdir := common.PathOutsideChroot(seedgenWorkingDir)
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		if err = os.RemoveAll(workdir); err != nil {
			return fmt.Errorf("failed to delete %s: %w", workdir, err)
		}
	}

	if err := os.Mkdir(workdir, 0o700); err != nil {
		return fmt.Errorf("failed to create workdir: %w", err)
	}

	lcaNamespace := os.Getenv("MY_POD_NAMESPACE")

	// Get the seedgen secret
	seedGenSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: utils.SeedGenSecretName, Namespace: lcaNamespace}, seedGenSecret); err != nil {
		return fmt.Errorf("could not access secret %s in %s: %w", utils.SeedGenSecretName, lcaNamespace, err)
	}

	if err := commonUtils.MarshalToFile(seedGenSecret, common.PathOutsideChroot(storedSeedgenSecret)); err != nil {
		return fmt.Errorf("failed to write secret to %s: %w", storedSeedgenSecret, err)
	}

	if seedAuth, exists := seedGenSecret.Data["seedAuth"]; exists {
		if err := os.WriteFile(common.PathOutsideChroot(seedgenAuthFile), seedAuth, 0o644); err != nil {
			return fmt.Errorf("failed to write %s: %w", seedgenAuthFile, err)
		}
	} else {
		return fmt.Errorf("could not find seedAuth in %s secret", utils.SeedGenSecretName)
	}

	// Save the seedgen CR in order to restore it after the ibu-imager is complete
	if err := commonUtils.MarshalToFile(seedgen, common.PathOutsideChroot(storedSeedgenCR)); err != nil {
		return fmt.Errorf("failed to write CR to %s: %w", storedSeedgenCR, err)
	}

	if hubKubeconfig, exists := seedGenSecret.Data["hubKubeconfig"]; exists {
		// Create client for access to hub
		hubClient, err := r.createHubClient(hubKubeconfig)
		if err != nil {
			return fmt.Errorf("failed to create hub client: %w", err)
		}

		if r.managedClusterExists(ctx, hubClient) {
			// Save the ACM resources from hub needed for re-import
			r.Log.Info("Collecting ACM import data")
			if err := r.deregisterFromHub(ctx, hubClient); err != nil {
				return err
			}

		} else {
			r.Log.Info("ManagedCluster does not exist on hub")
		}
	} else {
		r.Log.Info(fmt.Sprintf("No hubKubeconfig found in secret %s. Skipping hub interaction", utils.SeedGenSecretName))
	}

	// Clean up cluster resources
	r.Log.Info("Cleaning cluster resources")
	if err := r.cleanupClusterResources(ctx); err != nil {
		return err
	}

	// TODO: Can this be done cleanly via client? The client.DeleteAllOf seems to require a specified namespace, so maybe loop over the namespaces
	r.Log.Info("Cleaning completed and failed pods")
	kubeconfigArg := fmt.Sprintf("--kubeconfig=%s", common.KubeconfigFile)
	if _, err := r.Executor.Execute("oc", "delete", "pod", kubeconfigArg, "--field-selector=status.phase==Succeeded", "--all-namespaces"); err != nil {
		return fmt.Errorf("failed to cleanup Succeeded pods: %w", err)
	}
	if _, err := r.Executor.Execute("oc", "delete", "pod", kubeconfigArg, "--field-selector=status.phase==Failed", "--all-namespaces"); err != nil {
		return fmt.Errorf("failed to cleanup Failed pods: %w", err)
	}

	r.Log.Info("Deleting seedgen CR")
	if err := r.Client.Delete(ctx, seedgen); err != nil {
		return fmt.Errorf("unable to delete seedgen CR: %w", err)
	}

	if err := r.launchImager(seedgen); err != nil {
		return fmt.Errorf("imager failed: %w", err)
	}

	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *SeedGeneratorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (nextReconcile ctrl.Result, err error) {
	r.Log.Info("Start reconciling SeedGen", "name", req.NamespacedName)
	defer func() {
		if nextReconcile.RequeueAfter > 0 {
			r.Log.Info("Finish reconciling SeedGen", "name", req.NamespacedName, "requeueAfter", nextReconcile.RequeueAfter.Seconds())
		} else {
			r.Log.Info("Finish reconciling SeedGen", "name", req.NamespacedName, "requeueRightAway", nextReconcile.Requeue)
		}
	}()

	nextReconcile = doNotRequeue()

	if req.Name != utils.SeedGenName {
		r.Log.Info(fmt.Sprintf("Unexpected name (%s). Expected %s", req.Name, utils.SeedGenName))
		return
	}

	if lcaImage, err = r.getLcaImage(ctx); err != nil {
		return
	}

	// Get the cluster name
	cmClient := clusterinfo.NewClusterInfoClient(r.Client)
	clusterData, err := cmClient.CreateClusterInfo(ctx)
	if err != nil {
		return
	}
	clusterName = clusterData.ClusterName

	// Get the SeedGenerator CR
	seedgen := &seedgenv1alpha1.SeedGenerator{}
	err = r.Get(ctx, req.NamespacedName, seedgen)
	if err != nil {
		if errors.IsNotFound(err) {
			err = nil
			return
		}
		r.Log.Error(err, "Failed to get SeedGenerator")
		return
	}

	if isSeedGenFailed(seedgen) {
		r.Log.Info("Seed Generation previously failed")
		return
	}

	if rejection := r.validateSystem(ctx); len(rejection) > 0 {
		setSeedGenStatusFailedWithMessage(seedgen, rejection)
		r.Log.Info(fmt.Sprintf("Seed generation rejected: system validation failed: %s", rejection))

		// Update status
		err = r.updateStatus(ctx, seedgen)
		return
	}

	// TODO: Handle restoring the CR from file and completing processing
	if firstReconcile(seedgen) {
		setSeedGenStatusInProgress(seedgen)
		if err = r.updateStatus(ctx, seedgen); err != nil {
			err = fmt.Errorf("failed to update status: %w", err)
			return
		}

		r.Log.Info(fmt.Sprintf("Generating seed image: %s", seedgen.Spec.SeedImage))
		if err = r.generateSeedImage(ctx, seedgen); err != nil {
			setSeedGenStatusFailed(seedgen)
			if upderr := r.updateStatus(ctx, seedgen); upderr != nil {
				r.Log.Error(upderr, "Failed to update status")
			}

			return
		}
	} else if isSeedGenInProgress(seedgen) {
		r.Log.Info("Seed Generation is in progress")
	} else if isSeedGenCompleted(seedgen) {
		r.Log.Info("Seed Generation is completed")
	}

	// Update status
	err = r.updateStatus(ctx, seedgen)
	return
}

func setSeedGenStatusFailedWithMessage(seedgen *seedgenv1alpha1.SeedGenerator, msg string) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenCompleted,
		utils.SeedGenConditionReasons.Failed,
		metav1.ConditionFalse,
		msg,
		seedgen.Generation)
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.Failed,
		metav1.ConditionFalse,
		msg,
		seedgen.Generation)
}

func setSeedGenStatusFailed(seedgen *seedgenv1alpha1.SeedGenerator) {
	setSeedGenStatusFailedWithMessage(seedgen, "Seed Generation failed")
}

func setSeedGenStatusInProgress(seedgen *seedgenv1alpha1.SeedGenerator) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.Completed,
		metav1.ConditionTrue,
		"Seed Generation in progress",
		seedgen.Generation)
}

//nolint:unused
func setSeedGenStatusCompleted(seedgen *seedgenv1alpha1.SeedGenerator) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenCompleted,
		utils.SeedGenConditionReasons.Completed,
		metav1.ConditionTrue,
		"Seed Generation completed",
		seedgen.Generation)
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.Completed,
		metav1.ConditionFalse,
		"Seed Generation completed",
		seedgen.Generation)
}

func isSeedGenFailed(seedgen *seedgenv1alpha1.SeedGenerator) bool {
	seedgenCompletedCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenCompleted))
	seedgenInProgressCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenInProgress))

	// Only allow start if both conditions are absent
	return (seedgenInProgressCondition != nil && seedgenInProgressCondition.Reason == string(utils.SeedGenConditionReasons.Failed)) ||
		(seedgenCompletedCondition != nil && seedgenCompletedCondition.Reason == string(utils.SeedGenConditionReasons.Failed))
}

func isSeedGenInProgress(seedgen *seedgenv1alpha1.SeedGenerator) bool {
	seedgenInProgressCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenInProgress))

	return seedgenInProgressCondition != nil && seedgenInProgressCondition.Status == metav1.ConditionTrue
}

func isSeedGenCompleted(seedgen *seedgenv1alpha1.SeedGenerator) bool {
	seedgenCompletedCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenCompleted))

	return seedgenCompletedCondition != nil && seedgenCompletedCondition.Status == metav1.ConditionTrue
}

func firstReconcile(seedgen *seedgenv1alpha1.SeedGenerator) bool {
	seedgenCompletedCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenCompleted))
	seedgenInProgressCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenInProgress))

	// Only allow start if both conditions are absent
	return seedgenInProgressCondition == nil && seedgenCompletedCondition == nil
}

func (r *SeedGeneratorReconciler) updateStatus(ctx context.Context, seedgen *seedgenv1alpha1.SeedGenerator) error {
	seedgen.Status.ObservedGeneration = seedgen.ObjectMeta.Generation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := r.Status().Update(ctx, seedgen)
		return err
	})

	if err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeedGeneratorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("SeedGenerator")

	return ctrl.NewControllerManagedBy(mgr).
		For(&seedgenv1alpha1.SeedGenerator{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Generation is only updated on spec changes (also on deletion),
				// not metadata or status
				oldGeneration := e.ObjectOld.GetGeneration()
				newGeneration := e.ObjectNew.GetGeneration()
				// spec update only for SeedGen
				return oldGeneration != newGeneration
			},
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc:  func(de event.DeleteEvent) bool { return false },
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
