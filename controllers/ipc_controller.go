package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	igntypes "github.com/coreos/ignition/v2/config/v3_2/types"
	"github.com/go-logr/logr"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	"github.com/samber/lo"
)

//+kubebuilder:rbac:groups=lca.openshift.io,resources=ipconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lca.openshift.io,resources=ipconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigs,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get
//+kubebuilder:rbac:groups="",resources=pods,verbs=get
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
//+kubebuilder:rbac:groups=config.openshift.io,resources=proxies,verbs=get;list;watch

// IPConfigReconciler reconciles an IPConfig object
type IPConfigReconciler struct {
	client.Client
	NoncachedClient client.Reader
	Scheme          *runtime.Scheme
	ChrootOps       ops.Ops
	NsenterOps      ops.Ops
	RebootClient    reboot.RebootIntf
	RPMOstreeClient rpmostreeclient.IClient
	OstreeClient    ostreeclient.IClient
	Clientset       *kubernetes.Clientset
	IdleHandler     IPConfigStageHandler
	ConfigHandler   IPConfigStageHandler
	RollbackHandler IPConfigStageHandler
	Mux             *sync.Mutex
}

//go:generate mockgen -source=ipc_controller.go -package=controllers -destination=ipc_controller_mock.go
type IPConfigTwoPhaseHandlerInterface interface {
	PrePivot(ctx context.Context, ipc *ipcv1.IPConfig, logger logr.Logger) (ctrl.Result, error)
	PostPivot(ctx context.Context, ipc *ipcv1.IPConfig, logger logr.Logger) (ctrl.Result, error)
}

//go:generate mockgen -source=ipc_controller.go -package=controllers -destination=ipc_controller_mock.go
type IPConfigStageHandler interface {
	Handle(ctx context.Context, ipc *ipcv1.IPConfig) (ctrl.Result, error)
}

func (r *IPConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	if r.Mux != nil {
		r.Mux.Lock()
		defer r.Mux.Unlock()
	}

	logger := log.FromContext(ctx).WithName("IPConfig")
	logger.Info(
		"Start reconciling IPConfig",
		"name", req.NamespacedName.Name,
		"namespace", req.NamespacedName.Namespace,
	)

	// Ensure the workspace directory exists once at the start of reconcile
	if err := r.ChrootOps.MkdirAll(common.PathOutsideChroot(common.LCAWorkspaceDir), 0o700); err != nil {
		return requeueWithError(fmt.Errorf("failed to create workspace dir: %w", err))
	}

	ipc, err := r.getIPConfig(ctx, logger)
	if err != nil {
		return requeueWithError(fmt.Errorf("failed to get IPConfig: %w", err))
	}
	ipc.Status.ObservedGeneration = ipc.Generation

	defer func() {
		validNextStages, ierr := validNextStages(ipc, r.RPMOstreeClient)
		if ierr != nil {
			if err != nil {
				err = fmt.Errorf("%w; also failed to validate next stages: %s", err, ierr.Error())
			} else {
				err = fmt.Errorf("failed to validate next stages: %w", ierr)
			}
		}

		ipc.Status.ValidNextStages = validNextStages
		if uErr := r.Client.Status().Update(ctx, ipc); uErr != nil {
			if err != nil {
				err = fmt.Errorf("%w; also failed to update ipconfig status: %s", err, uErr.Error())
			} else {
				err = fmt.Errorf("failed to update ipconfig status: %w", uErr)
			}
		}
	}()

	if ipc.Status.ValidNextStages == nil {
		validNextStages, err := validNextStages(ipc, r.RPMOstreeClient)
		if err != nil {
			return requeueWithError(fmt.Errorf("failed to get valid next stages: %w", err))
		}
		ipc.Status.ValidNextStages = validNextStages
		if err := r.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
	}

	if err := r.refreshStatus(ctx, ipc); err != nil {
		return requeueWithError(fmt.Errorf("failed to refresh status: %w", err))
	}

	if err := r.Client.Status().Update(ctx, ipc); err != nil {
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	if err := r.cacheRecertImageIfNeeded(ctx, ipc, logger); err != nil {
		logger.Error(err, "recert image caching failed")
	}

	annotations := ipc.GetAnnotations()
	if annotations != nil && annotations[controllerutils.TriggerReconcileAnnotation] != "" {
		delete(annotations, controllerutils.TriggerReconcileAnnotation)
		ipc.SetAnnotations(annotations)
		if err := r.Client.Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig annotations: %w", err))
		}
	}

	// Start stage history timer. The timer is stopped from inside the handlers when they complete successfully
	controllerutils.StartIPStageHistory(r.Client, logger, ipc)
	// .status.history is reset as long as the desired stage is Idle
	controllerutils.ResetIPHistory(r.Client, logger, ipc)

	switch ipc.Spec.Stage {
	case ipcv1.IPStages.Idle:
		res, handleErr := r.IdleHandler.Handle(ctx, ipc)
		if handleErr != nil {
			return requeueWithError(fmt.Errorf("idle handler failed: %w", handleErr))
		}
		return res, nil
	case ipcv1.IPStages.Config:
		res, handleErr := r.ConfigHandler.Handle(ctx, ipc)
		if handleErr != nil {
			return requeueWithError(fmt.Errorf("config handler failed: %w", handleErr))
		}
		return res, nil
	case ipcv1.IPStages.Rollback:
		res, handleErr := r.RollbackHandler.Handle(ctx, ipc)
		if handleErr != nil {
			return requeueWithError(fmt.Errorf("rollback handler failed: %w", handleErr))
		}
		return res, nil
	default:
		// Shouldn't happen
		logger.Error(nil, "invalid IPConfig stage", "stage", ipc.Spec.Stage)
		return doNotRequeue(), nil
	}
}

func validNextStages(ipc *ipcv1.IPConfig, rpmOstreeClient rpmostreeclient.IClient) ([]ipcv1.IPConfigStage, error) {
	inProgressStage := controllerutils.GetIPInProgressStage(ipc)

	if inProgressStage == ipcv1.IPStages.Rollback ||
		controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Rollback) {
		return []ipcv1.IPConfigStage{}, nil
	}

	if inProgressStage == ipcv1.IPStages.Config ||
		controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Config) {
		isTargetStaterootBooted, err := isTargetStaterootBooted(ipc, rpmOstreeClient)
		if err != nil {
			return nil, fmt.Errorf("failed to check if target stateroot is booted: %w", err)
		}

		isUnbootedStaterootAvailable, err := isUnbootedStaterootAvailable(rpmOstreeClient)
		if err != nil {
			return nil, fmt.Errorf("failed to check if unbooted stateroot is available: %w", err)
		}

		if lo.FromPtr(isTargetStaterootBooted) && lo.FromPtr(isUnbootedStaterootAvailable) {
			return []ipcv1.IPConfigStage{ipcv1.IPStages.Rollback}, nil
		}

		return []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, nil
	}

	// no in progress stage, check completed stages in reverse order
	if controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Rollback) {
		return []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, nil
	}
	if controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Config) {
		return []ipcv1.IPConfigStage{ipcv1.IPStages.Idle, ipcv1.IPStages.Rollback}, nil
	}
	if controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Idle) {
		return []ipcv1.IPConfigStage{ipcv1.IPStages.Config}, nil
	}

	// initial IPConfig creation - no idle condition
	idleCondition := meta.FindStatusCondition(ipc.Status.Conditions, string(controllerutils.ConditionTypes.Idle))
	if idleCondition == nil {
		return []ipcv1.IPConfigStage{ipcv1.IPStages.Idle}, nil
	}

	return []ipcv1.IPConfigStage{}, nil
}

// isTargetStaterootBooted determines whether the stateroot prepared for this IP change is currently booted.
// It reconstructs the expected stateroot name from the spec (matching the lca-cli prepare logic) and queries rpm-ostree.
func isTargetStaterootBooted(ipc *ipcv1.IPConfig, rpmOstreeClient rpmostreeclient.IClient) (*bool, error) {
	if rpmOstreeClient == nil {
		return nil, fmt.Errorf("rpmOstreeClient is nil")
	}

	targetStaterootName := buildIPConfigStaterootName(ipc)
	if targetStaterootName == "" {
		return nil, fmt.Errorf("failed to build target stateroot name")
	}

	booted, err := rpmOstreeClient.IsStaterootBooted(targetStaterootName)
	if err != nil {
		return nil, fmt.Errorf("failed to check if target stateroot is booted: %w", err)
	}

	return lo.ToPtr(booted), nil
}

func isUnbootedStaterootAvailable(rpmOstreeClient rpmostreeclient.IClient) (*bool, error) {
	if rpmOstreeClient == nil {
		return nil, fmt.Errorf("rpmOstreeClient is nil")
	}

	unbootedStaterootName, err := rpmOstreeClient.GetUnbootedStaterootName()
	if err != nil || unbootedStaterootName == "" {
		return lo.ToPtr(false), nil
	}

	return lo.ToPtr(true), nil
}

func buildIPConfigStaterootName(ipc *ipcv1.IPConfig) string {
	var ipv4, ipv6, vlan, dnsIPFamily string
	if ipc.Spec.IPv4 != nil {
		ipv4 = ipc.Spec.IPv4.Address
	}

	if ipc.Spec.IPv6 != nil {
		ipv6 = ipc.Spec.IPv6.Address
	}

	if ipc.Spec.VLANID > 0 {
		vlan = strconv.Itoa(ipc.Spec.VLANID)
	}

	if ipc.Spec.DNSResolutionFamily != "" {
		dnsIPFamily = ipc.Spec.DNSResolutionFamily
	}

	return common.BuildNewStaterootNameForIPConfig(common.IPConfigStaterootParams{
		IPv4Address: ipv4,
		IPv6Address: ipv6,
		VLANID:      vlan,
		DNSIPFamily: dnsIPFamily,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipcv1.IPConfig{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
					return true
				}

				// trigger reconcile upon adding or removing ManualCleanupAnnotation
				_, oldExist := e.ObjectOld.GetAnnotations()[controllerutils.ManualCleanupAnnotation]
				_, newExist := e.ObjectNew.GetAnnotations()[controllerutils.ManualCleanupAnnotation]
				if oldExist != newExist {
					return true
				}

				// trigger reconcile upon adding or updating TriggerReconcileAnnotation
				oldValue, oldHas := e.ObjectOld.GetAnnotations()[controllerutils.TriggerReconcileAnnotation]
				newValue, newHas := e.ObjectNew.GetAnnotations()[controllerutils.TriggerReconcileAnnotation]
				if (!oldHas && newHas) || (oldHas && newHas && oldValue != newValue) {
					return true
				}

				// trigger reconcile upon adding or updating recert image annotation
				oldValue, oldHas = e.ObjectOld.GetAnnotations()[controllerutils.RecertImageAnnotation]
				newValue, newHas = e.ObjectNew.GetAnnotations()[controllerutils.RecertImageAnnotation]
				if (!oldHas && newHas) || (oldHas && newHas && oldValue != newValue) {
					return true
				}

				// trigger reconcile upon adding or updating recert pull secret annotation
				oldValue, oldHas = e.ObjectOld.GetAnnotations()[controllerutils.RecertPullSecretAnnotation]
				newValue, newHas = e.ObjectNew.GetAnnotations()[controllerutils.RecertPullSecretAnnotation]
				if (!oldHas && newHas) || (oldHas && newHas && oldValue != newValue) {
					return true
				}

				return false
			},
			CreateFunc:  func(ce event.CreateEvent) bool { return true },
			GenericFunc: func(ge event.GenericEvent) bool { return false },
			DeleteFunc: func(de event.DeleteEvent) bool {
				if de.Object.GetName() == common.IPConfigName {
					ipc := de.Object.(*ipcv1.IPConfig)
					filePath := common.PathOutsideChroot(common.IPCFilePath)
					if controllerutils.IsIPStageCompleted(ipc, ipcv1.IPStages.Idle) ||
						controllerutils.IsIPStageFailed(ipc, ipcv1.IPStages.Rollback) {
						if err := r.ChrootOps.RemoveFile(filePath); err != nil {
							if !r.ChrootOps.IsNotExist(err) {
								fmt.Printf("Failed to remove IPConfig from %s: %v", filePath, err)
							}
						}
					} else {
						raw, err := json.Marshal(de.Object)
						if err != nil {
							fmt.Printf("Failed to marshal deleted IPConfig for %s: %v", filePath, err)
						} else if err := r.ChrootOps.WriteFile(filePath, raw, 0o600); err != nil {
							fmt.Printf("Failed to save deleted IPConfig to %s: %v", filePath, err)
						}
					}
					return true
				}
				return false
			},
		})).
		Complete(r)
}

// getIPConfig tries to get the IPConfig CR by performing the following operations in order:
//   - Fetching from the API from the non-cached client or initializes it if it doesn't exist.
//   - If the latter fails, it attempts to restore the IPConfig CR from the file system.
//   - If the restoration fails, it creates a new IPConfig CR.
func (r *IPConfigReconciler) getIPConfig(ctx context.Context, logger logr.Logger) (*ipcv1.IPConfig, error) {
	ipc := &ipcv1.IPConfig{}
	if err := r.NoncachedClient.Get(ctx, client.ObjectKey{Name: common.IPConfigName}, ipc); err != nil {
		if errors.IsNotFound(err) {
			if initErr := lcautils.InitIPConfig(ctx, r.Client, &logger); initErr != nil {
				return nil, fmt.Errorf("failed to initialize IPConfig: %w", initErr)
			}
			return ipc, nil
		}
		return nil, fmt.Errorf("failed to get IPConfig: %w", err)
	}
	return ipc, nil
}

func validateIPConfigStage(ipc *ipcv1.IPConfig) error {
	if !lo.Contains(ipc.Status.ValidNextStages, ipc.Spec.Stage) {
		return fmt.Errorf("invalid IPConfig stage: %s", ipc.Spec.Stage)
	}

	return nil
}

// cacheRecertImageIfNeeded pulls and caches the recert image if it hasn't been cached yet.
// If the annotation is not provided, it resolves the image via getRecertImage.
func (r *IPConfigReconciler) cacheRecertImageIfNeeded(ctx context.Context, ipc *ipcv1.IPConfig, logger logr.Logger) error {
	annotations := ipc.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	image := annotations[controllerutils.RecertImageAnnotation]
	if image == "" {
		image = getRecertImage(ipc)
	}

	if cached := annotations[controllerutils.RecertCachedImageAnnotation]; cached == image {
		return nil
	}

	authFile := common.ImageRegistryAuthFile
	if name := annotations[controllerutils.RecertPullSecretAnnotation]; name != "" {
		pullSecret, err := lcautils.GetSecretData(
			ctx,
			name,
			common.LcaNamespace,
			corev1.DockerConfigJsonKey,
			r.Client,
		)
		if err != nil {
			return fmt.Errorf(
				"failed to get pull-secret with the name %s in namespace %s holding the key %s: %w",
				name,
				common.LcaNamespace,
				corev1.DockerConfigJsonKey,
				err,
			)
		}

		tmpPath := filepath.Join(
			common.PathOutsideChroot(common.LCAWorkspaceDir),
			fmt.Sprintf("recert-pull-secret-%d.json", time.Now().UnixNano()),
		)
		if err := r.ChrootOps.WriteFile(tmpPath, []byte(pullSecret), 0o600); err != nil {
			return fmt.Errorf("failed to write pull secret to temp file %s: %w", tmpPath, err)
		}
		defer func() {
			if derr := r.ChrootOps.RemoveFile(tmpPath); derr != nil && !r.ChrootOps.IsNotExist(derr) {
				logger.Error(derr, "failed to remove temp recert pull secret file", "path", tmpPath)
			}
		}()
		authFile = tmpPath
	}

	if _, err := r.ChrootOps.RunBashInHostNamespace(
		"podman",
		"pull",
		"--authfile",
		authFile,
		image,
	); err != nil {
		return fmt.Errorf("failed to pull recert image %s: %w", image, err)
	}

	annotations[controllerutils.RecertCachedImageAnnotation] = image
	ipc.SetAnnotations(annotations)
	if err := r.Client.Update(ctx, ipc); err != nil {
		return fmt.Errorf("failed to update annotations after caching recert image: %w", err)
	}

	logger.Info("recert image cached on host", "image", image)

	return nil
}

func isIPTransitionRequested(ipc *ipcv1.IPConfig) bool {
	desiredStage := ipc.Spec.Stage
	if desiredStage == ipcv1.IPStages.Idle {
		return !(controllerutils.IsIPStageCompleted(ipc, desiredStage) ||
			controllerutils.IsIPStageInProgress(ipc, desiredStage))
	}
	return !(controllerutils.IsIPStageCompletedOrFailed(ipc, desiredStage) ||
		controllerutils.IsIPStageInProgress(ipc, desiredStage))
}

func (r *IPConfigReconciler) refreshStatus(ctx context.Context, ipc *ipcv1.IPConfig) error {
	output, err := r.nmstateShowJSON()
	if err != nil {
		return fmt.Errorf("failed to get nmstate output: %w", err)
	}

	state, err := lcautils.ParseNmstate(output)
	if err != nil {
		return fmt.Errorf("failed to parse nmstate output: %w", err)
	}

	dnsV4, dnsV6 := lcautils.ExtractDNS(state)
	gw4, gw6 := lcautils.FindDefaultGateways(
		state,
		controllerutils.BridgeExternalName,
		controllerutils.DefaultRouteV4,
		controllerutils.DefaultRouteV6,
	)
	vlanID, err := lcautils.ExtractBrExVLANID(state, controllerutils.BridgeExternalName)
	if err != nil {
		return fmt.Errorf("failed to extract BrEx VLAN ID: %w", err)
	}

	nodeIPs, err := lcautils.GetNodeInternalIPs(ctx, r.NoncachedClient)
	if err != nil {
		return fmt.Errorf("failed to find node IPs: %w", err)
	}

	machineCIDRs, err := lcautils.GetMachineNetworks(ctx, r.NoncachedClient)
	if err != nil {
		return fmt.Errorf("failed to find machine networks: %w", err)
	}

	ipv4, ipv6, vlan := buildNetworkStatus(
		gw4,
		gw6,
		dnsV4,
		dnsV6,
		nodeIPs,
		machineCIDRs,
		vlanID,
	)

	ipc.Status.IPv4 = ipv4
	ipc.Status.IPv6 = ipv6
	ipc.Status.VLANID = vlan

	fam, err := r.inferDNSResolutionFamilyFromMC(ctx)
	if err != nil {
		return fmt.Errorf("failed to infer DNS resolution family from MC: %w", err)
	}
	ipc.Status.DNSResolutionFamily = lo.FromPtr(fam)

	return nil
}

// inferDNSResolutionFamilyFromMC inspects the MachineConfig used to configure dnsmasq
// and infers the active DNS filter: "ipv4", "ipv6" or "none" when not set.
// It is assumed that the dnsmasq MachineConfig is the only one that contains the dnsmasq filter file.
// and it can only container one of the known filters or not exist.
func (r *IPConfigReconciler) inferDNSResolutionFamilyFromMC(ctx context.Context) (*string, error) {
	mc := &machineconfigv1.MachineConfig{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: common.DnsmasqMachineConfigName}, mc); err != nil {
		return nil, fmt.Errorf("failed to get dnsmasq machine config: %w", err)
	}

	var cfg igntypes.Config
	if len(mc.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(mc.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("failed to parse ignition config: %w", err)
		}
	}

	v4Encoded := url.PathEscape(common.DnsmasqFilterIPv4)
	v6Encoded := url.PathEscape(common.DnsmasqFilterIPv6)
	v4Source := fmt.Sprintf(common.DataURLBase64Template, v4Encoded)
	v6Source := fmt.Sprintf(common.DataURLBase64Template, v6Encoded)

	for _, f := range cfg.Storage.Files {
		if f.Path != common.DnsmasqFilterTargetPath {
			continue
		}

		if f.Contents.Source == nil {
			return nil, fmt.Errorf("contents source is nil for file %s", f.Path)
		}

		switch *f.Contents.Source {
		case v4Source:
			return lo.ToPtr(common.IPv4FamilyName), nil
		case v6Source:
			return lo.ToPtr(common.IPv6FamilyName), nil
		default:
			return nil, fmt.Errorf("unknown contents source: %s", *f.Contents.Source)
		}
	}

	return lo.ToPtr("none"), nil
}

func (r *IPConfigReconciler) nmstateShowJSON() (string, error) {
	output, err := r.NsenterOps.RunInHostNamespace("nmstatectl", "show", "--json", "-q")
	if err != nil {
		return "", fmt.Errorf("failed to run nmstatectl show --json: %w", err)
	}

	return output, nil
}

func buildNetworkStatus(
	gw4 string,
	gw6 string,
	dnsV4 string,
	dnsV6 string,
	nodeIPs []string,
	machineCIDRs []string,
	vlanID *int,
) (*ipcv1.IPv4Status, *ipcv1.IPv6Status, int) {
	var ipv4 *ipcv1.IPv4Status
	var ipv6 *ipcv1.IPv6Status
	var vlan int

	var nodeIPv4, nodeIPv6 string
	for _, ip := range nodeIPs {
		if strings.Contains(ip, ":") {
			if nodeIPv6 == "" {
				nodeIPv6 = ip
			}
		} else {
			if nodeIPv4 == "" {
				nodeIPv4 = ip
			}
		}
	}

	// Note: status.ipv4/status.ipv6 are intentionally a flattened shape matching the spec.
	// We populate Address/MachineNetwork from node internal IPs, and Gateway/DNSServer from nmstate.
	// Only populate a family when the node actually has an internal IP for that family.
	if nodeIPv4 != "" {
		ipv4 = &ipcv1.IPv4Status{
			Address:        nodeIPv4,
			MachineNetwork: lcautils.FindMatchingCIDR(nodeIPv4, machineCIDRs),
			Gateway:        gw4,
			DNSServer:      dnsV4,
		}
	}

	if nodeIPv6 != "" {
		ipv6 = &ipcv1.IPv6Status{
			Address:        nodeIPv6,
			MachineNetwork: lcautils.FindMatchingCIDR(nodeIPv6, machineCIDRs),
			Gateway:        gw6,
			DNSServer:      dnsV6,
		}
	}

	if vlanID != nil {
		vlan = *vlanID
	}

	return ipv4, ipv6, vlan
}
