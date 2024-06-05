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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/util/retry"

	"github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/healthcheck"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	commonUtils "github.com/openshift-kni/lifecycle-agent/utils"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"

	"github.com/go-logr/logr"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ibuv1 "github.com/openshift-kni/lifecycle-agent/api/imagebasedupgrade/v1"
	seedgenv1 "github.com/openshift-kni/lifecycle-agent/api/seedgenerator/v1"
	mcv1 "github.com/openshift/api/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeedGeneratorReconciler reconciles a SeedGenerator object
type SeedGeneratorReconciler struct {
	client.Client
	NoncachedClient client.Reader
	Log             logr.Logger
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	Executor        ops.Execute
	Mux             *sync.Mutex
}

var (
	lcaImage            string
	seedgenAuthFile     = filepath.Join(utils.SeedgenWorkspacePath, "auth.json")
	imagerContainerName = "lca_image_builder"
)

const (
	EnvSkipRecert = "SEEDGEN_SKIP_RECERT"

	// The following consts are used for certain progress status messages, which may also factor into the reconciler phase check
	msgLaunchingImager   = "Launching imager container"
	msgFinalizingSeedgen = "Finalizing seed generation"
	msgWaitingForStable  = "Waiting for system to stabilize"
	msgSeedgenFailed     = "Seed generation failed"
)

// SeedGen reconciler phases
type seedgenReconcilerPhase string

// Stages defines the string values for valid stages
var phases = struct {
	PhaseInitial    seedgenReconcilerPhase // SeedGen hasn't started yet
	PhaseGenerating seedgenReconcilerPhase // SeedGen is in the first phase of work, ending with the launch of the imager
	PhaseFinalizing seedgenReconcilerPhase // SeedGen has previously launched the imager and is in the final phase of seed generation
	PhaseCompleted  seedgenReconcilerPhase // SeedGen has successfully completed
	PhaseFailed     seedgenReconcilerPhase // SeedGen has failed
}{
	PhaseInitial:    "initial",
	PhaseGenerating: "generating",
	PhaseFinalizing: "finalizing",
	PhaseCompleted:  "completed",
	PhaseFailed:     "failed",
}

//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lca.openshift.io,resources=seedgenerators/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=delete
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigs,verbs=get;list;watch;delete

// getPhase determines the reconciler phase based on the seedgen CR status conditions
func getPhase(seedgen *seedgenv1.SeedGenerator) seedgenReconcilerPhase {
	seedgenCompletedCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenCompleted))
	seedgenInProgressCondition := meta.FindStatusCondition(seedgen.Status.Conditions, string(utils.SeedGenConditionTypes.SeedGenInProgress))

	// If neither condition is set, the reconciler phase is phaseInitial
	if seedgenInProgressCondition == nil && seedgenCompletedCondition == nil {
		return phases.PhaseInitial
	}

	// If either condition is set to Failed, the reconciler phase is phaseFailed
	if (seedgenInProgressCondition != nil && seedgenInProgressCondition.Reason == string(utils.SeedGenConditionReasons.Failed)) ||
		(seedgenCompletedCondition != nil && seedgenCompletedCondition.Reason == string(utils.SeedGenConditionReasons.Failed)) {
		return phases.PhaseFailed
	}

	// If the Completed condition is set to True, the reconciler phase is phaseCompleted
	if seedgenCompletedCondition != nil && seedgenCompletedCondition.Status == metav1.ConditionTrue {
		return phases.PhaseCompleted
	}

	// If the InProgress condition is set to True, check the status message to determine the reconciler phase
	if seedgenInProgressCondition != nil && seedgenInProgressCondition.Status == metav1.ConditionTrue {
		msg := seedgenInProgressCondition.Message
		if msg == msgLaunchingImager {
			// Reconciler phase is phaseFinalizing
			return phases.PhaseFinalizing
		} else if msg == "" {
			return phases.PhaseInitial
		}
	}

	// Reconciler phase is phaseGenerating
	return phases.PhaseGenerating
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

func (r *SeedGeneratorReconciler) waitForPullSecretOverride(ctx context.Context, dockerConfigJSON []byte) error {
	updatedPullSecret, _ := lcautils.UpdatePullSecretFromDockerConfig(ctx, r.Client, dockerConfigJSON)

	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer deadlineCancel()
	err := wait.PollUntilContextCancel(deadlineCtx, 30*time.Second, true, func(ctx context.Context) (done bool, err error) {
		r.Log.Info("Waiting for MCO to override pull-secret file", "image registry auth file location", common.ImageRegistryAuthFile)
		dockerConfigJSON, err := os.ReadFile(filepath.Join(common.Host, common.ImageRegistryAuthFile))
		if err != nil {
			r.Log.Info(fmt.Sprintf("Failed to read %s file with error %s, will retry",
				common.ImageRegistryAuthFile, err))
			return false, nil
		}
		if strings.TrimSpace(string(dockerConfigJSON)) != string(updatedPullSecret.Data[".dockerconfigjson"]) {
			return false, nil
		}
		if err := healthcheck.AreMachineConfigPoolsReady(deadlineCtx, r.NoncachedClient, r.Log); err != nil {
			r.Log.Info(fmt.Sprintf("Waiting for MCP: %s", err.Error()))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("timed out waiting for MCO to override pull-secret file: %w", err)
	}

	return nil
}

func (r *SeedGeneratorReconciler) cleanupOldRenderedMachineConfigs(ctx context.Context) error {
	r.Log.Info("Cleaning old machine configs")
	mcps := &mcv1.MachineConfigPoolList{}
	err := r.Client.List(ctx, mcps)
	if err != nil {
		return fmt.Errorf("failed to list machine config pools, err: %w", err)
	}
	var currentMCs []string
	for _, mcp := range mcps.Items {
		currentMCs = append(currentMCs, mcp.Spec.Configuration.Name)
	}

	mcs := &mcv1.MachineConfigList{}
	err = r.Client.List(ctx, mcs)
	if err != nil {
		return fmt.Errorf("failed to list machine configs, err: %w", err)
	}
	for _, mc := range mcs.Items {
		if !strings.HasPrefix(mc.Name, "rendered") || lo.Contains(currentMCs, mc.Name) {
			continue
		}
		r.Log.Info(fmt.Sprintf("Deleting machine config %s", mc.Name))
		if err := r.Client.Delete(ctx, mc.DeepCopy()); err != nil {
			return fmt.Errorf("failed to delete machine config %s, err: %w", mc.Name, err)
		}
	}
	return nil
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
	if err = r.Client.Get(ctx, types.NamespacedName{Name: os.Getenv("MY_POD_NAME"), Namespace: common.LcaNamespace}, pod); err != nil {
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

// Delete the previous imager container, if it exists
func (r *SeedGeneratorReconciler) rmPreviousImagerContainer() error {
	_, err := r.Executor.Execute("podman", "rm", "-i", "-f", imagerContainerName)
	if err != nil {
		return fmt.Errorf("failed to run podman rm command: %w", err)
	}

	return nil
}

// getRecertImagePullSpec returns the recert image pull-spec, based on the following priority order:
//   - Use recertImage from seedgen spec, if specified
//   - If not, get the value from the recert image environment variable
//   - If environment variable is not set, use the default value
func (r *SeedGeneratorReconciler) getRecertImagePullSpec(seedgen *seedgenv1.SeedGenerator) (recertImage string) {
	if seedgen.Spec.RecertImage == "" {
		recertImage = os.Getenv(common.RecertImageEnvKey)
		if recertImage == "" {
			recertImage = common.DefaultRecertImage
		}
	} else {
		recertImage = seedgen.Spec.RecertImage
	}

	return
}

func (r *SeedGeneratorReconciler) pullRecertImagePullSpec(seedgen *seedgenv1.SeedGenerator) error {
	recertImage := r.getRecertImagePullSpec(seedgen)

	_, err := r.Executor.Execute("podman", "pull", "--authfile", common.ImageRegistryAuthFile, recertImage)
	if err != nil {
		return fmt.Errorf("failed to pull recertImage (%s): %w", recertImage, err)
	}

	return nil
}

// Launch a container to run the imager
func (r *SeedGeneratorReconciler) launchImager(seedgen *seedgenv1.SeedGenerator) error {
	r.Log.Info("Launching imager")
	recertImage := r.getRecertImagePullSpec(seedgen)

	skipRecert := false
	skipRecertEnvValue := os.Getenv(EnvSkipRecert)
	if skipRecertEnvValue == "TRUE" {
		skipRecert = true
		r.Log.Info(fmt.Sprintf("Skipping recert validation because %s=%s", EnvSkipRecert, skipRecertEnvValue))
	}

	imagerCmdArgs := []string{
		"podman", "run", "--privileged", "--pid=host",
		fmt.Sprintf("--name=%s", imagerContainerName),
		"--replace", "--net=host",
		// --http-proxy=true is already the default, but we're setting it
		// explicitly to emphasize that we depend on it for the seed image
		// generator to have network access in proxy-only environments
		"--http-proxy=true",
		"-v", "/etc:/etc", "-v", "/var:/var", "-v", "/var/run:/var/run", "-v", "/run/systemd/journal/socket:/run/systemd/journal/socket",
		"-v", fmt.Sprintf("%s:%s", seedgenAuthFile, seedgenAuthFile),
		"--entrypoint", "lca-cli",
		lcaImage,
		"create",
		"--authfile", seedgenAuthFile,
		"--image", seedgen.Spec.SeedImage,
		"--recert-image", recertImage,
	}

	if skipRecert {
		imagerCmdArgs = append(imagerCmdArgs, "--skip-recert-validation")
	}

	// In order to have the imager container both survive the LCA pod shutdown and have continued network access
	// after all other pods are shutdown, we're using systemd-run to launch it as a transient service-unit
	systemdRunOpts := []string{
		"--collect",
		"--wait",
		"--unit", "lca-generate-seed-image",
		// Ensure the proxy environment variables of the LCA container are
		// passed through systemd-run to the podman process (which will pass it
		// to the seed image generation container thanks to `--http-proxy=true`)
		"--setenv", "HTTP_PROXY",
		"--setenv", "HTTPS_PROXY",
		"--setenv", "NO_PROXY",
	}

	if _, err := r.Executor.Execute("systemd-run", append(systemdRunOpts, imagerCmdArgs...)...); err != nil {
		return fmt.Errorf("failed to run imager container: %w", err)
	}

	// We should never get here, as the imager will shutdown this pod
	return nil
}

// checkImagerStatus examines the lca_cli container, returning nil if it exited successfully
func (r *SeedGeneratorReconciler) checkImagerStatus() error {
	type ContainerState struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
	}

	type ContainerInfo struct {
		State ContainerState `json:"State"`
	}

	expectedStatus := "exited"
	expectedExitCode := 0

	r.Log.Info("Checking status of lca_cli container")

	output, err := r.Executor.Execute("podman", "inspect", "--format", "json", imagerContainerName)
	if err != nil {
		return fmt.Errorf("failed to run podman inspect command: %w", err)
	}

	var containers []ContainerInfo

	if err := json.Unmarshal([]byte(output), &containers); err != nil {
		return fmt.Errorf("unable to parse podman inspect command output: %w", err)
	}

	if len(containers) != 1 {
		return fmt.Errorf("expected 1 item in podman inspect output, got %d", len(containers))
	}

	if containers[0].State.Status != expectedStatus {
		return fmt.Errorf("expected container status %s, found: %s", expectedStatus, containers[0].State.Status)
	}

	if containers[0].State.ExitCode != expectedExitCode {
		return fmt.Errorf("expected container status %d, found: %d", expectedExitCode, containers[0].State.ExitCode)
	}

	r.Log.Info("Seed image generation was successful")
	return nil
}

// Check whether the system can be used for seed generation
func (r *SeedGeneratorReconciler) validateSystem(ctx context.Context) (msg string) {
	// Check that the "ostree admin set-default" feature is available
	if !ostreeclient.NewClient(r.Executor, false).IsOstreeAdminSetDefaultFeatureEnabled() {
		msg = "Rejected: Installed release does not support \"ostree admin set-default\" feature"
		return
	}

	// Verify that the klusterlet CRD is absent, indicating the cluster has been detached from ACM (if originally deployed via ACM)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.Get(ctx, types.NamespacedName{Name: "klusterlets.operator.open-cluster-management.io"}, crd); !errors.IsNotFound(err) {
		if err == nil {
			msg = "Rejected: Cluster must be detached from hub prior to seed generation"
		} else {
			msg = "Failure occurred during check for klusterlets CRD in system validation"
		}
		return
	}

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

	// Ensure cluster's pull-secret is not sanitized
	dockerConfigJSON, _ := os.ReadFile(filepath.Join(common.Host, common.ImageRegistryAuthFile))
	if strings.TrimSpace(string(dockerConfigJSON)) == strings.TrimSpace(common.PullSecretEmptyData) {
		msg = "Rejected due to invalid cluster pull-secret (previously sanitized without proper restore)"
		return
	}

	return
}

func (r *SeedGeneratorReconciler) restoreSeedgenCRIfNeeded(ctx context.Context, seedgen *seedgenv1.SeedGenerator) error {
	r.Log.Info("Restoring seedgen CR in DB")

	// Clear the ResourceVersion
	seedgen.SetResourceVersion("")

	// Save status as the seedgen structure gets over-written by the create call
	// with the result which has no status
	status := seedgen.Status
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return client.IgnoreAlreadyExists(r.Client.Create(ctx, seedgen)) //nolint:wrapcheck
	}); err != nil {
		return fmt.Errorf("failed to create seedgen during restore: %w", err)
	}

	// Put the saved status into the newly create seedgen with the right resource
	// version which is required for the update call to work
	seedgen.Status = status
	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return r.Client.Status().Update(ctx, seedgen) //nolint:wrapcheck
	}); err != nil {
		return fmt.Errorf("failed to update seedgen status during restore: %w", err)
	}

	return nil
}

func (r *SeedGeneratorReconciler) restoreSeedgenSecretCR(ctx context.Context, secret *corev1.Secret) error {
	r.Log.Info("Restoring seedgen secret CR")

	// Strip the ResourceVersion, otherwise the restore fails
	secret.SetResourceVersion("")

	if err := common.RetryOnConflictOrRetriable(retry.DefaultBackoff, func() error {
		return client.IgnoreAlreadyExists(r.Client.Create(ctx, secret)) //nolint:wrapcheck
	}); err != nil {
		return fmt.Errorf("failed to create seedgen secret: %w", err)
	}

	return nil
}

func (r *SeedGeneratorReconciler) wipeExistingWorkspace() error {
	workdir := common.PathOutsideChroot(utils.SeedgenWorkspacePath)
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		if err = os.RemoveAll(workdir); err != nil {
			return fmt.Errorf("failed to delete %s: %w", workdir, err)
		}
	}
	return nil
}

func (r *SeedGeneratorReconciler) setupWorkspace() error {
	if err := r.wipeExistingWorkspace(); err != nil {
		return fmt.Errorf("failed to wipe previous workspace: %w", err)
	}

	if err := r.rmPreviousImagerContainer(); err != nil {
		return fmt.Errorf("failed to delete previous imager container: %w", err)
	}

	if err := os.Mkdir(common.PathOutsideChroot(utils.SeedgenWorkspacePath), 0o700); err != nil {
		return fmt.Errorf("failed to create workdir: %w", err)
	}
	return nil
}

// Generate the seed image
func (r *SeedGeneratorReconciler) generateSeedImage(ctx context.Context, seedgen *seedgenv1.SeedGenerator) (nextReconcile ctrl.Result, rc error) {
	// Wait for system stability before starting seed generation
	r.Log.Info("Checking system health")
	if err := healthcheck.HealthChecks(ctx, r.NoncachedClient, r.Log); err != nil {
		r.Log.Info(fmt.Sprintf("health check failed: %s", err.Error()))
		setSeedGenStatusInProgress(seedgen, fmt.Sprintf("%s: %s", msgWaitingForStable, err.Error()))
		nextReconcile = requeueWithHealthCheckInterval()
		return
	}

	r.Log.Info("Health check passed")

	setSeedGenStatusInProgress(seedgen, "Starting seed generation")
	if err := r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "failed to update seedgen CR status")
		return
	}

	nextReconcile = doNotRequeue()
	if err := r.setupWorkspace(); err != nil {
		rc = err
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// Pull the recertImage first, to avoid potential failures late in the seed image generation procedure
	setSeedGenStatusInProgress(seedgen, "Pulling recert image")
	if err := r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "failed to update seedgen CR status")
		return
	}

	if err := r.pullRecertImagePullSpec(seedgen); err != nil {
		rc = fmt.Errorf("failed to pull recert image: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	setSeedGenStatusInProgress(seedgen, "Preparing for seed generation")
	if err := r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "failed to update seedgen CR status")
		return
	}

	// Get the seedgen secret
	seedGenSecret := &corev1.Secret{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: utils.SeedGenSecretName, Namespace: common.LcaNamespace}, seedGenSecret); err != nil {
		rc = fmt.Errorf("could not access secret %s in %s: %w", utils.SeedGenSecretName, common.LcaNamespace, err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// Save the seedgen secret CR in order to restore it after the imager is complete
	if err := commonUtils.MarshalToFile(seedGenSecret, common.PathOutsideChroot(utils.SeedGenStoredSecretCR)); err != nil {
		rc = fmt.Errorf("failed to write secret to %s: %w", utils.SeedGenStoredSecretCR, err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	if seedAuth, exists := seedGenSecret.Data["seedAuth"]; exists {
		if err := os.WriteFile(common.PathOutsideChroot(seedgenAuthFile), seedAuth, 0o600); err != nil {
			rc = fmt.Errorf("failed to write %s: %w", seedgenAuthFile, err)
			setSeedGenStatusFailed(seedgen, rc.Error())
			return
		}
	} else {
		rc = fmt.Errorf("could not find seedAuth in %s secret", utils.SeedGenSecretName)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// Get the cluster's pull-secret
	originalPullSecretData, err := lcautils.GetSecretData(ctx, common.PullSecretName, common.OpenshiftConfigNamespace, corev1.DockerConfigJsonKey, r.Client)
	if err != nil {
		rc = fmt.Errorf("could not access pull-secret %s in %s: %w", common.PullSecretName, common.OpenshiftConfigNamespace, err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// Save the cluster's pull-secret in order to restore it after the imager is complete
	if err := os.WriteFile(common.PathOutsideChroot(utils.StoredPullSecret), []byte(originalPullSecretData), 0o600); err != nil {
		rc = fmt.Errorf("failed to write pull-secret to %s: %w", utils.StoredPullSecret, err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	setSeedGenStatusInProgress(seedgen, "Cleaning cluster resources")
	if err := r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "failed to update seedgen CR status")
		return
	}

	// Clean up cluster resources
	r.Log.Info("Cleaning cluster resources")
	if err := r.cleanupClusterResources(ctx); err != nil {
		rc = fmt.Errorf("failed to cleanup resources: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// TODO: Can this be done cleanly via client? The client.DeleteAllOf seems to require a specified namespace, so maybe loop over the namespaces
	r.Log.Info("Cleaning completed and failed pods")
	kubeconfigArg := fmt.Sprintf("--kubeconfig=%s", common.KubeconfigFile)
	if _, err := r.Executor.Execute("oc", "delete", "pod", kubeconfigArg, "--field-selector=status.phase==Succeeded", "--all-namespaces"); err != nil {
		rc = fmt.Errorf("failed to cleanup Succeeded pods: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}
	if _, err := r.Executor.Execute("oc", "delete", "pod", kubeconfigArg, "--field-selector=status.phase==Failed", "--all-namespaces"); err != nil {
		rc = fmt.Errorf("failed to cleanup Failed pods: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	r.Log.Info("Sanitize cluster's pull-secret from sensitive data")
	if err := r.waitForPullSecretOverride(ctx, []byte(common.PullSecretEmptyData)); err != nil {
		rc = fmt.Errorf("failed sanitizing cluster's pull-secret: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}
	// In the success case, the pod will block until terminated by the imager container.
	// Create a deferred function to restore the secret CR in the case where a failure happens
	// before that point.
	defer r.waitForPullSecretOverride(ctx, []byte(originalPullSecretData))

	if err := r.cleanupOldRenderedMachineConfigs(ctx); err != nil {
		rc = fmt.Errorf("failed to cleanup old machine configs, err: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// Final stage of initial seed generation is to delete the CR and launch the container.
	// Update the CR status prior to its saving and deletion
	setSeedGenStatusInProgress(seedgen, msgLaunchingImager)
	if err := r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "failed to update seedgen CR status")
		return
	}

	// Save the seedgen CR in order to restore it after the imager is complete
	if err := commonUtils.MarshalToFile(seedgen, common.PathOutsideChroot(utils.SeedGenStoredCR)); err != nil {
		rc = fmt.Errorf("failed to write CR to %s: %w", utils.SeedGenStoredCR, err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	r.Log.Info("Deleting seedgen secret CR")
	if err := r.Client.Delete(ctx, seedGenSecret); err != nil {
		rc = fmt.Errorf("unable to delete seedgen secret CR: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}
	// In the success case, the pod will block until terminated by the imager container.
	// Create a deferred function to restore the secret CR in the case where a failure happens
	// before that point.
	defer r.restoreSeedgenSecretCR(ctx, seedGenSecret)

	r.Log.Info("Deleting seedgen CR")
	if err := r.Client.Delete(ctx, seedgen); err != nil {
		rc = fmt.Errorf("unable to delete seedgen CR: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}
	// In the success case, the pod will block until terminated by the imager container.
	// Create a deferred function to restore the seedgen CR in the case where a failure happens
	// before that point.
	defer r.restoreSeedgenCRIfNeeded(ctx, seedgen)

	// Delete the IBU CR prior to launching the imager, so it's not in the seed image
	ibu := &ibuv1.ImageBasedUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name: utils.IBUName,
		}}
	if err := r.Client.Delete(ctx, ibu); client.IgnoreNotFound(err) != nil {
		rc = fmt.Errorf("failed to delete IBU CR: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	if err := r.launchImager(seedgen); err != nil {
		rc = fmt.Errorf("imager failed: %w", err)
		setSeedGenStatusFailed(seedgen, rc.Error())
		return
	}

	// If we've gotten this far, something has gone wrong
	rc = fmt.Errorf("unexpected return from launching imager container")
	setSeedGenStatusFailed(seedgen, rc.Error())
	return
}

// finishSeedgen runs after the imager container completes and restores kubelet, once the LCA operator restarts
func (r *SeedGeneratorReconciler) finishSeedgen() error {
	// Check exit status of lca_cli container
	if err := r.checkImagerStatus(); err != nil {
		return fmt.Errorf("imager container status check failed: %w", err)
	}

	if err := r.wipeExistingWorkspace(); err != nil {
		return fmt.Errorf("failed to wipe workspace: %w", err)
	}

	return nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *SeedGeneratorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (nextReconcile ctrl.Result, rc error) {
	if r.Mux != nil {
		r.Mux.Lock()
		defer r.Mux.Unlock()
	}

	var err error
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
		rc = err
		return
	}

	// Use a non-cached query to Get the SeedGen CR, to ensure we aren't running against a stale cached CR
	seedgen := &seedgenv1.SeedGenerator{}
	err = common.RetryOnRetriable(common.RetryBackoffTwoMinutes, func() error {
		return r.NoncachedClient.Get(ctx, req.NamespacedName, seedgen) //nolint:wrapcheck
	})
	if err != nil {
		if errors.IsNotFound(err) {
			return
		}
		r.Log.Error(err, "Failed to get SeedGenerator")

		// This is likely a case where the API is down, so requeue and try again shortly
		nextReconcile = requeueWithShortInterval()

		rc = err
		return
	}

	switch phase := getPhase(seedgen); phase {
	case phases.PhaseFailed:
		r.Log.Info("Seed Generation has failed. Please delete and recreate the CR to try again")
		return
	case phases.PhaseCompleted:
		r.Log.Info("Seed Generation is completed")
		return
	case phases.PhaseInitial:
		// Run the system validation
		if rejection := r.validateSystem(ctx); len(rejection) > 0 {
			setSeedGenStatusFailed(seedgen, rejection)
			r.Log.Info(fmt.Sprintf("Seed generation rejected: system validation failed: %s", rejection))

			// Update status
			if err = r.updateStatus(ctx, seedgen); err != nil {
				r.Log.Error(err, "Failed to update status")
			}
			return
		}

		setSeedGenStatusInProgress(seedgen, msgWaitingForStable)
		nextReconcile = requeueImmediately()
	case phases.PhaseGenerating:
		r.Log.Info(fmt.Sprintf("Generating seed image: %s", seedgen.Spec.SeedImage))
		if nextReconcile, err = r.generateSeedImage(ctx, seedgen); err != nil {
			_ = r.wipeExistingWorkspace()

			// CR Status will have been updated by generateSeedImage, so just log the failure
			r.Log.Error(err, "Seed generation failed")
		}
	case phases.PhaseFinalizing:
		r.Log.Info("Finalizing Seed Generation")
		setSeedGenStatusInProgress(seedgen, msgFinalizingSeedgen)
		if err := r.updateStatus(ctx, seedgen); err != nil {
			r.Log.Error(err, "failed to update seedgen CR status")
		}

		if err = r.finishSeedgen(); err != nil {
			r.Log.Error(err, "Seed generation failed")
			setSeedGenStatusFailed(seedgen, fmt.Sprintf("Seed generation failed: %s", err))
		} else {
			setSeedGenStatusCompleted(seedgen)
		}
		nextReconcile = doNotRequeue()
	}

	// Update status
	if err = r.updateStatus(ctx, seedgen); err != nil {
		r.Log.Error(err, "Failed to update status")
	}
	return
}

// Utility functions for conditions/status
func setSeedGenStatusFailed(seedgen *seedgenv1.SeedGenerator, msg string) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenCompleted,
		utils.SeedGenConditionReasons.Failed,
		metav1.ConditionFalse,
		fmt.Sprintf("%s: %s", msgSeedgenFailed, msg),
		seedgen.Generation)
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.Failed,
		metav1.ConditionFalse,
		msg,
		seedgen.Generation)
}

func setSeedGenStatusInProgress(seedgen *seedgenv1.SeedGenerator, msg string) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.InProgress,
		metav1.ConditionTrue,
		msg,
		seedgen.Generation)
}

func setSeedGenStatusCompleted(seedgen *seedgenv1.SeedGenerator) {
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenInProgress,
		utils.SeedGenConditionReasons.Completed,
		metav1.ConditionFalse,
		"Seed Generation completed",
		seedgen.Generation)
	utils.SetStatusCondition(&seedgen.Status.Conditions,
		utils.SeedGenConditionTypes.SeedGenCompleted,
		utils.SeedGenConditionReasons.Completed,
		metav1.ConditionTrue,
		"Seed Generation completed",
		seedgen.Generation)
}

func (r *SeedGeneratorReconciler) updateStatus(ctx context.Context, seedgen *seedgenv1.SeedGenerator) error {
	seedgen.Status.ObservedGeneration = seedgen.ObjectMeta.Generation
	err := common.RetryOnRetriable(common.RetryBackoffTwoMinutes, func() error {
		return r.Status().Update(ctx, seedgen) //nolint:wrapcheck
	})

	if err != nil {
		return fmt.Errorf("failed to update seedgen status: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeedGeneratorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("SeedGenerator")

	//nolint:wrapcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&seedgenv1.SeedGenerator{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc:  func(e event.UpdateEvent) bool { return false },
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc:  func(de event.DeleteEvent) bool { return false },
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
