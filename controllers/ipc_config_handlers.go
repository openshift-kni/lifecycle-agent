package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	ipcv1 "github.com/openshift-kni/lifecycle-agent/api/ipconfig/v1"
	controllerutils "github.com/openshift-kni/lifecycle-agent/controllers/utils"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/internal/reboot"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
	lcautils "github.com/openshift-kni/lifecycle-agent/utils"
	machineconfigv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	IPConfigPhasePrePivot  = "pre-pivot"
	IPConfigPhasePostPivot = "post-pivot"
)

type IPCConfigTwoPhaseHandler struct {
	Client          client.Client
	NoncachedClient client.Reader
	RPMOstreeClient rpmostreeclient.IClient
	OstreeClient    ostreeclient.IClient
	ChrootOps       ops.Ops
	RebootClient    reboot.RebootIntf
}

func NewIPCConfigTwoPhaseHandler(
	client client.Client,
	noncachedClient client.Reader,
	rpmostreeClient rpmostreeclient.IClient,
	ostreeClient ostreeclient.IClient,
	chrootOps ops.Ops,
	rebootClient reboot.RebootIntf,
) IPConfigTwoPhaseHandlerInterface {
	return &IPCConfigTwoPhaseHandler{
		Client:          client,
		NoncachedClient: noncachedClient,
		RPMOstreeClient: rpmostreeClient,
		OstreeClient:    ostreeClient,
		ChrootOps:       chrootOps,
		RebootClient:    rebootClient,
	}
}

type IPCConfigStageHandler struct {
	Client          client.Client
	NoncachedClient client.Reader
	RPMOstreeClient rpmostreeclient.IClient
	ChrootOps       ops.Ops
	PhasesHandler   IPConfigTwoPhaseHandlerInterface
}

func NewIPCConfigStageHandler(
	client client.Client,
	noncachedClient client.Reader,
	rpmOstreeClient rpmostreeclient.IClient,
	chrootOps ops.Ops,
	phasesHandler IPConfigTwoPhaseHandlerInterface,
) IPConfigStageHandler {
	return &IPCConfigStageHandler{
		Client:          client,
		NoncachedClient: noncachedClient,
		RPMOstreeClient: rpmOstreeClient,
		ChrootOps:       chrootOps,
		PhasesHandler:   phasesHandler,
	}
}

func (h *IPCConfigStageHandler) Handle(ctx context.Context, ipc *ipcv1.IPConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("IPConfig Config handler")
	logger.Info("Handler started")

	defer func() {
		logger.Info("Handler completed")
	}()

	if isIPTransitionRequested(ipc) {
		if err := validateIPConfigStage(ipc); err != nil {
			controllerutils.SetIPStatusInvalidTransition(
				ipc, fmt.Sprintf("invalid IPConfig stage: %s", ipc.Spec.Stage),
			)
			if err := h.Client.Status().Update(ctx, ipc); err != nil {
				logger.Error(err, "Failed to update IPConfig status after invalid transition")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}
			return doNotRequeue(), nil
		}

		controllerutils.ClearIPInvalidTransitionStatusConditions(ipc)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to clear IPConfig invalid transition status conditions")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		controllerutils.SetIPIdleStatusFalse(
			ipc,
			controllerutils.ConditionReasons.ConfigurationInProgress,
			controllerutils.ConfigurationInProgress,
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to update IPConfig idle status to configuration in progress")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		if err := h.validateConfigStart(ctx, ipc); err != nil {
			controllerutils.SetIPConfigStatusFailed(ipc, fmt.Sprintf("config validation failed: %s", err.Error()))
			if err := h.Client.Status().Update(ctx, ipc); err != nil {
				logger.Error(err, "Failed to update IPConfig status after validation failure")
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
			}

			logger.Info("Validation failed", "reason", err.Error())
			return doNotRequeue(), nil
		}

		controllerutils.SetIPConfigStatusInProgress(ipc, controllerutils.ConfigurationInProgress)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			logger.Error(err, "Failed to update IPConfig status to in-progress")
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
	}

	// stop when completed or failed
	if !controllerutils.IsIPStageInProgress(ipc, ipcv1.IPStages.Config) {
		logger.Info("Stage is not in progress")
		return doNotRequeue(), nil
	}

	targetStaterootBooted, err := isTargetStaterootBooted(ipc, h.RPMOstreeClient)
	if err != nil {
		logger.Error(err, "Failed to determine whether target stateroot is booted")
		return requeueWithError(fmt.Errorf("failed to check if target stateroot is booted: %w", err))
	}

	isUnbootedStaterootAvailable, err := isUnbootedStaterootAvailable(h.RPMOstreeClient)
	if err != nil {
		logger.Error(err, "Failed to determine whether an unbooted stateroot is available")
		return requeueWithError(fmt.Errorf("failed to check if unbooted stateroot is available: %w", err))
	}

	if !(lo.FromPtr(targetStaterootBooted) && lo.FromPtr(isUnbootedStaterootAvailable)) {
		logger.Info(
			"Config stage handler: running pre-pivot",
			"targetStaterootBooted", lo.FromPtr(targetStaterootBooted),
			"unbootedStaterootAvailable", lo.FromPtr(isUnbootedStaterootAvailable),
		)
		result, err := h.PhasesHandler.PrePivot(ctx, ipc, logger)
		if err != nil {
			logger.Error(err, "Pre-pivot phase failed")
			return result, fmt.Errorf("failed to run pre pivot: %w", err)
		}

		logger.Info("Returning after pre-pivot phase")
		return result, nil
	}

	controllerutils.StopIPPhase(h.Client, logger, ipc, IPConfigPhasePrePivot)

	result, err := h.PhasesHandler.PostPivot(ctx, ipc, logger)
	if err != nil {
		logger.Error(err, "Post-pivot phase failed")
		return result, fmt.Errorf("failed to run post pivot: %w", err)
	}

	if result.RequeueAfter != 0 {
		logger.Info("Returning after post-pivot phase")
		return result, nil
	}

	controllerutils.StopIPStageHistory(h.Client, logger, ipc)
	controllerutils.SetIPConfigStatusCompleted(ipc, "Configuration completed successfully")
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		logger.Error(err, "Failed to update IPConfig status to completed")
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	logger.Info("Completed successfully")

	return result, nil
}

func (h *IPCConfigTwoPhaseHandler) PrePivot(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
	logger logr.Logger,
) (ctrl.Result, error) {
	controllerutils.StartIPPhase(h.Client, logger, ipc, IPConfigPhasePrePivot)
	logger.Info("Starting pre-pivot phase")

	if err := statusIPsMatchSpec(ipc); err == nil {
		controllerutils.SetIPConfigStatusCompleted(ipc, "Spec and status match; nothing to do")
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		logger.Info("IPConfig status matches spec")

		return doNotRequeue(), nil
	}

	if shouldSkipClusterHealthChecks(
		ipc,
		controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation,
	) {
		logger.Info(
			"Skipping cluster health checks due to annotation",
			"annotation", controllerutils.SkipIPConfigPreConfigurationClusterHealthChecksAnnotation,
		)
	} else {
		if err := CheckHealth(ctx, h.NoncachedClient, logger.WithName("HealthCheck")); err != nil {
			msg := fmt.Sprintf("Waiting for system to stabilize: %s", err.Error())
			controllerutils.SetIPConfigStatusInProgress(ipc, msg)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
			}
			return requeueWithHealthCheckInterval(), nil
		}
	}

	if err := h.copyLcaCliToHost(logger); err != nil {
		controllerutils.SetIPConfigStatusFailed(ipc, err.Error())
		if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
		}
		return requeueWithError(fmt.Errorf("failed to copy lca-cli binary: %w", err))
	}

	if err := reboot.WriteIPCAutoRollbackConfigFile(logger, ipc, h.ChrootOps); err != nil {
		controllerutils.SetIPConfigStatusFailed(ipc, fmt.Sprintf("failed to write ip-config auto-rollback config: %s", err.Error()))
		if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
		}
		return requeueWithError(fmt.Errorf("failed to write ip-config auto-rollback config: %w", err))
	}

	if err := h.writeIPConfigPrePivotConfig(ipc); err != nil {
		controllerutils.SetIPConfigStatusFailed(
			ipc,
			fmt.Sprintf("failed to write ip-config pre-pivot config: %s", err.Error()),
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		return requeueWithError(fmt.Errorf("failed to write ip-config pre-pivot config: %w", err))
	}

	if err := h.writeIPConfigPostPivotConfig(ipc); err != nil {
		controllerutils.SetIPConfigStatusFailed(
			ipc,
			fmt.Sprintf("failed to write ip-config post-pivot config: %s", err.Error()),
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		return requeueWithError(fmt.Errorf("failed to write ip-config post-pivot config: %w", err))
	}

	if err := exportIPConfigForUncontrolledRollback(ipc, h.ChrootOps); err != nil {
		return requeueWithError(fmt.Errorf("failed to export ipconfig for uncontrolled rollback: %w", err))
	}

	if err := h.RunLcaCliIPConfigPrePivot(logger); err != nil {
		controllerutils.SetIPConfigStatusFailed(
			ipc,
			fmt.Sprintf("ip-config pre-pivot failed. error: %s", err.Error()),
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}
		logger.Error(err, "IP-config pre-pivot failed")
		return doNotRequeue(), nil
	}

	// We shouldn't reach here on successful ip-config pre-pivot

	return requeueWithShortInterval(), nil
}

func (h *IPCConfigTwoPhaseHandler) PostPivot(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
	logger logr.Logger,
) (ctrl.Result, error) {
	controllerutils.StartIPPhase(h.Client, logger, ipc, IPConfigPhasePostPivot)
	logger.Info("Starting post-pivot phase")

	if err := h.RebootClient.DisableInitMonitor(); err != nil {
		controllerutils.SetIPConfigStatusFailed(
			ipc,
			fmt.Sprintf("failed to disable init monitor: %s", err.Error()),
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		return requeueWithError(fmt.Errorf("failed to disable init monitor: %w", err))
	}

	if shouldSkipClusterHealthChecks(
		ipc,
		controllerutils.SkipIPConfigPostConfigurationClusterHealthChecksAnnotation,
	) {
		logger.Info(
			"Skipping cluster health checks due to annotation",
			"annotation", controllerutils.SkipIPConfigPostConfigurationClusterHealthChecksAnnotation,
		)
	} else {
		if err := CheckHealth(ctx, h.NoncachedClient, logger); err != nil {
			controllerutils.SetIPConfigStatusInProgress(
				ipc,
				fmt.Sprintf("Waiting for system to stabilize: %s", err.Error()),
			)
			if uerr := h.Client.Status().Update(ctx, ipc); uerr != nil {
				return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", uerr))
			}

			// Best-effort cleanup: pods stuck in ImagePullBackOff/ErrImagePull can block health checks forever.
			h.deleteImagePullBackOffPodsBestEffort(ctx, logger.WithName("ImagePullBackOffCleanup"))

			return requeueWithHealthCheckInterval(), nil
		}
	}

	controllerutils.SetIPConfigStatusInProgress(ipc, "Cluster has stabilized")
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	if err := statusIPsMatchSpec(ipc); err != nil {
		controllerutils.SetIPConfigStatusInProgress(
			ipc,
			fmt.Sprintf("Waiting for current IPs to match spec: %s", err.Error()),
		)
		if err := h.Client.Status().Update(ctx, ipc); err != nil {
			return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
		}

		return requeueWithHealthCheckInterval(), nil
	}

	controllerutils.StopIPPhase(h.Client, logger, ipc, IPConfigPhasePostPivot)
	if err := h.Client.Status().Update(ctx, ipc); err != nil {
		return requeueWithError(fmt.Errorf("failed to update ipconfig status: %w", err))
	}

	logger.Info("Finished post-pivot phase successfully")

	return doNotRequeue(), nil
}

func (h *IPCConfigTwoPhaseHandler) deleteImagePullBackOffPodsBestEffort(ctx context.Context, logger logr.Logger) {
	pods := &corev1.PodList{}
	if err := h.NoncachedClient.List(ctx, pods); err != nil {
		logger.Error(err, "Failed to list pods for ImagePullBackOff cleanup")
		return
	}

	for i := range pods.Items {
		p := pods.Items[i]

		if p.DeletionTimestamp != nil {
			continue
		}

		// Static pod mirror pods are continuously reconciled by the kubelet;
		// deleting them is not useful and can create churn.
		if _, ok := p.Annotations[corev1.MirrorPodAnnotationKey]; ok {
			continue
		}

		reason := imagePullBackOffReason(&p)
		if reason == "" {
			continue
		}

		if err := h.Client.Delete(ctx, &p, client.GracePeriodSeconds(0)); err != nil &&
			!k8serrors.IsNotFound(err) {
			logger.Error(
				err, "Failed to delete pod stuck in image pull backoff",
				"namespace", p.Namespace, "name", p.Name, "reason", reason,
			)
			continue
		}
		logger.Info(
			"Deleted pod stuck in image pull backoff",
			"namespace", p.Namespace,
			"name", p.Name,
			"reason", reason,
		)
	}
}

func imagePullBackOffReason(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}

	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		if r := cs.State.Waiting.Reason; r == controllerutils.PodContainerWaitingReasonImagePullBackOff ||
			r == controllerutils.PodContainerWaitingReasonErrImagePull {
			return r
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		if r := cs.State.Waiting.Reason; r == controllerutils.PodContainerWaitingReasonImagePullBackOff ||
			r == controllerutils.PodContainerWaitingReasonErrImagePull {
			return r
		}
	}

	return ""
}

func statusIPsMatchSpec(ipc *ipcv1.IPConfig) error {
	mismatches := []string{}

	if ipc.Status.IPv4 == nil && ipc.Status.IPv6 == nil && ipc.Status.VLANID == 0 {
		return fmt.Errorf("status networking not yet populated")
	}

	if ipc.Spec.DNSFilterOutFamily != "" {
		if ipc.Status.DNSFilterOutFamily != ipc.Spec.DNSFilterOutFamily {
			mismatches = append(mismatches, fmt.Sprintf(
				"dnsFilterOutFamily mismatch: spec=%s status=%s",
				ipc.Spec.DNSFilterOutFamily, ipc.Status.DNSFilterOutFamily,
			))
		}
	}

	if ipc.Spec.VLANID > 0 {
		if ipc.Status.VLANID != ipc.Spec.VLANID {
			mismatches = append(mismatches, fmt.Sprintf(
				"vlan mismatch: spec=%d status=%d",
				ipc.Spec.VLANID, ipc.Status.VLANID,
			))
		}
	}

	if len(ipc.Spec.DNSServers) > 0 {
		if !reflect.DeepEqual(ipc.Spec.DNSServers, ipc.Status.DNSServers) {
			mismatches = append(
				mismatches,
				fmt.Sprintf("dnsServers mismatch: spec=%v status=%v", ipc.Spec.DNSServers, ipc.Status.DNSServers),
			)
		}
	}

	if v4 := ipc.Spec.IPv4; v4 != nil {
		v4Mismatches := checkFamilyStatusMatchesSpec(common.IPv4FamilyName, v4, ipc.Status.IPv4)
		mismatches = append(mismatches, v4Mismatches...)
	}

	if v6 := ipc.Spec.IPv6; v6 != nil {
		v6Mismatches := checkFamilyStatusMatchesSpec(common.IPv6FamilyName, v6, ipc.Status.IPv6)
		mismatches = append(mismatches, v6Mismatches...)
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("desired network not observed in status: %s", strings.Join(mismatches, ", "))
	}

	return nil
}

func checkFamilyStatusMatchesSpec(
	family string,
	spec interface{},
	status interface{},
) []string {
	mismatches := []string{}

	switch family {
	case common.IPv4FamilyName:
		specV4, _ := spec.(*ipcv1.IPv4Config)
		statusV4, _ := status.(*ipcv1.IPv4Status)
		mismatches = append(mismatches, checkIPFamilySpecMatchesStatusV4(specV4, statusV4)...)
	case common.IPv6FamilyName:
		specV6, _ := spec.(*ipcv1.IPv6Config)
		statusV6, _ := status.(*ipcv1.IPv6Status)
		mismatches = append(mismatches, checkIPFamilySpecMatchesStatusV6(specV6, statusV6)...)
	default:
		mismatches = append(mismatches, fmt.Sprintf("unknown family: %s", family))
	}

	return mismatches
}

func checkIPFamilySpecMatchesStatusV4(spec *ipcv1.IPv4Config, status *ipcv1.IPv4Status) []string {
	mismatches := []string{}
	if spec == nil {
		return mismatches
	}
	if status == nil {
		return append(mismatches, "ipv4 missing from status")
	}
	if spec.Gateway != "" && spec.Gateway != status.Gateway {
		mismatches = append(mismatches, fmt.Sprintf("ipv4 gateway mismatch: spec=%s status=%s", spec.Gateway, status.Gateway))
	}
	if status.Address == "" {
		mismatches = append(mismatches, "ipv4 address missing from status")
	} else if !ipEqual(spec.Address, status.Address) {
		mismatches = append(mismatches, fmt.Sprintf("ipv4 address mismatch: want %s got %s", spec.Address, status.Address))
	}
	if spec.MachineNetwork != "" {
		if status.MachineNetwork == "" {
			mismatches = append(mismatches, fmt.Sprintf("ipv4 machineNetwork not observed: want %s", spec.MachineNetwork))
		} else if !cidrEqual(spec.MachineNetwork, status.MachineNetwork) {
			mismatches = append(mismatches, fmt.Sprintf("ipv4 machineNetwork mismatch: want %s got %s", spec.MachineNetwork, status.MachineNetwork))
		}
	}
	return mismatches
}

func checkIPFamilySpecMatchesStatusV6(spec *ipcv1.IPv6Config, status *ipcv1.IPv6Status) []string {
	mismatches := []string{}
	if spec == nil {
		return mismatches
	}
	if status == nil {
		return append(mismatches, "ipv6 missing from status")
	}
	if spec.Gateway != "" && spec.Gateway != status.Gateway {
		mismatches = append(mismatches, fmt.Sprintf("ipv6 gateway mismatch: spec=%s status=%s", spec.Gateway, status.Gateway))
	}
	if status.Address == "" {
		mismatches = append(mismatches, "ipv6 address missing from status")
	} else if !ipEqual(spec.Address, status.Address) {
		mismatches = append(mismatches, fmt.Sprintf("ipv6 address mismatch: want %s got %s", spec.Address, status.Address))
	}
	if spec.MachineNetwork != "" {
		if status.MachineNetwork == "" {
			mismatches = append(mismatches, fmt.Sprintf("ipv6 machineNetwork not observed: want %s", spec.MachineNetwork))
		} else if !cidrEqual(spec.MachineNetwork, status.MachineNetwork) {
			mismatches = append(mismatches, fmt.Sprintf("ipv6 machineNetwork mismatch: want %s got %s", spec.MachineNetwork, status.MachineNetwork))
		}
	}
	return mismatches
}

func ipEqual(a, b string) bool {
	// Normalize potential CIDR-style inputs (e.g. "192.0.2.10/24") to plain IPs
	normalize := func(s string) string {
		if strings.Contains(s, "/") {
			s = strings.SplitN(s, "/", 2)[0]
		}
		return s
	}

	na := normalize(a)
	nb := normalize(b)

	ipA := net.ParseIP(na)
	ipB := net.ParseIP(nb)

	// If parsing fails, fall back to plain string comparison
	if ipA == nil || ipB == nil {
		return a == b
	}

	return ipA.Equal(ipB)
}

func cidrEqual(a, b string) bool {
	na, ap, ea := parseCIDR(a)
	nb, bp, eb := parseCIDR(b)
	if ea != nil || eb != nil {
		return a == b
	}
	return ap == bp && net.ParseIP(na).Equal(net.ParseIP(nb))
}

func parseCIDR(c string) (string, int, error) {
	c = strings.TrimSpace(c)
	_, ipNet, err := net.ParseCIDR(c)
	if err != nil {
		return "", 0, fmt.Errorf("failed to parse CIDR %s: %w", c, err)
	}
	ones, _ := ipNet.Mask.Size()
	return ipNet.IP.String(), ones, nil
}

// writeIPConfigPrePivotConfigToNewStateroot writes the ip-config pre-pivot configuration file into the new stateroot etc
func (h *IPCConfigTwoPhaseHandler) writeIPConfigPrePivotConfig(ipc *ipcv1.IPConfig) error {
	cfg := common.IPConfigPrePivotConfig{}

	if v := ipc.Spec.IPv4; v != nil {
		if v.Address != "" {
			cfg.IPv4Address = strings.Split(v.Address, "/")[0]
		}
		if v.MachineNetwork != "" {
			cfg.IPv4MachineNetwork = v.MachineNetwork
		}
		if v.Gateway != "" {
			cfg.DesiredIPv4Gateway = v.Gateway
		}
	}

	if v := ipc.Spec.IPv6; v != nil {
		if v.Address != "" {
			cfg.IPv6Address = strings.Split(v.Address, "/")[0]
		}
		if v.MachineNetwork != "" {
			cfg.IPv6MachineNetwork = v.MachineNetwork
		}
		if v.Gateway != "" {
			cfg.DesiredIPv6Gateway = v.Gateway
		}
	}

	if len(ipc.Spec.DNSServers) > 0 {
		cfg.DNSServers = append([]string{}, ipc.Spec.DNSServers...)
	}

	if ipc.Spec.VLANID > 0 {
		cfg.VLANID = ipc.Spec.VLANID
	}

	completeIPConfigPrePivotConfigFromStatus(&cfg, ipc)

	if v, ok := ipc.GetAnnotations()[controllerutils.RecertPullSecretAnnotation]; ok && v != "" {
		cfg.PullSecretRefName = v
	}

	if ipc.Spec.DNSFilterOutFamily != "" {
		cfg.DNSFilterOutFamily = ipc.Spec.DNSFilterOutFamily
	}

	cfg.InstallInitMonitor = true
	cfg.InstallIPConfigurationService = true
	cfg.NewStaterootName = buildIPConfigStaterootName(ipc)

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal ip-config pre-pivot config: %w", err)
	}

	if err := h.ChrootOps.WriteFile(
		common.PathOutsideChroot(common.IPConfigPrePivotFlagsFile),
		data,
		0o600,
	); err != nil {
		return fmt.Errorf("failed to write ip-config pre-pivot config: %w", err)
	}

	return nil
}

// completeIPConfigPrePivotConfigFromStatus completes the ip-config pre-pivot configuration from the status.
// It only backfills values that must be provided to the cli regrdless of they change.
func completeIPConfigPrePivotConfigFromStatus(cfg *common.IPConfigPrePivotConfig, ipc *ipcv1.IPConfig) {
	if cfg == nil || ipc == nil {
		return
	}

	backfillPrePivotVLANFromStatus(cfg, ipc)
	backfillPrePivotIPv4FromStatus(cfg, ipc.Status.IPv4)
	backfillPrePivotIPv6FromStatus(cfg, ipc.Status.IPv6)
}

func backfillPrePivotVLANFromStatus(cfg *common.IPConfigPrePivotConfig, ipc *ipcv1.IPConfig) {
	if cfg == nil || ipc == nil {
		return
	}
	if cfg.VLANID != 0 {
		return
	}
	if ipc.Status.VLANID <= 0 {
		return
	}
	cfg.VLANID = ipc.Status.VLANID
}

func backfillPrePivotIPv4FromStatus(
	cfg *common.IPConfigPrePivotConfig,
	status *ipcv1.IPv4Status,
) {
	if cfg == nil {
		return
	}
	if status == nil {
		return
	}
	if cfg.IPv4Address == "" && status.Address != "" {
		cfg.IPv4Address = status.Address
	}
	if cfg.IPv4MachineNetwork == "" && status.MachineNetwork != "" {
		cfg.IPv4MachineNetwork = status.MachineNetwork
	}
	if cfg.DesiredIPv4Gateway == "" && status.Gateway != "" {
		cfg.DesiredIPv4Gateway = status.Gateway
	}
	if cfg.CurrentIPv4Gateway == "" && status.Gateway != "" {
		cfg.CurrentIPv4Gateway = status.Gateway
	}
}

func backfillPrePivotIPv6FromStatus(
	cfg *common.IPConfigPrePivotConfig,
	status *ipcv1.IPv6Status,
) {
	if cfg == nil {
		return
	}
	if status == nil {
		return
	}
	if cfg.IPv6Address == "" && status.Address != "" {
		cfg.IPv6Address = strings.TrimSpace(status.Address)
	}
	if cfg.IPv6MachineNetwork == "" && status.MachineNetwork != "" {
		cfg.IPv6MachineNetwork = status.MachineNetwork
	}
	if cfg.DesiredIPv6Gateway == "" && status.Gateway != "" {
		cfg.DesiredIPv6Gateway = status.Gateway
	}
	if cfg.CurrentIPv6Gateway == "" && status.Gateway != "" {
		cfg.CurrentIPv6Gateway = status.Gateway
	}
}

// writeIPConfigPostPivotConfig writes the ip-config post-pivot configuration file into the host workspace.
// This file is consumed by `lca-cli ip-config post-pivot` after the reboot into the target stateroot.
func (h *IPCConfigTwoPhaseHandler) writeIPConfigPostPivotConfig(ipc *ipcv1.IPConfig) error {
	cfg := common.IPConfigPostPivotConfig{
		RecertImage: getRecertImage(ipc),
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal ip-config post-pivot config: %w", err)
	}

	if err := h.ChrootOps.WriteFile(
		common.PathOutsideChroot(common.IPConfigPostPivotFlagsFile),
		data,
		0o600,
	); err != nil {
		return fmt.Errorf("failed to write ip-config post-pivot config: %w", err)
	}

	return nil
}

// getRecertImage resolves the recert image to use in priority: annotation, env, default
func getRecertImage(ipc *ipcv1.IPConfig) string {
	if v := ipc.GetAnnotations()[controllerutils.RecertImageAnnotation]; v != "" {
		return v
	}
	if v := os.Getenv(common.RecertImageEnvKey); v != "" {
		return v
	}
	return common.DefaultRecertImage
}

// RunLcaCliIPConfigPrePivot schedules an lca-cli ip-config pre-pivot via systemd-run.
func (h *IPCConfigTwoPhaseHandler) RunLcaCliIPConfigPrePivot(
	logger logr.Logger,
) error {
	logger.Info("Scheduling lca-cli ip-config pre-pivot via systemd-run")

	args := []string{
		"--wait",
		"--collect",
		"--property", controllerutils.SystemdExitTypeCgroup,
		"--unit", controllerutils.IPConfigPrePivotUnit,
		"--description", controllerutils.IPConfigPrePivotDescription,
		controllerutils.LcaCliBinaryName, "ip-config", "pre-pivot",
	}

	if _, err := h.ChrootOps.RunSystemdAction(args...); err != nil {
		return fmt.Errorf("ip-config pre-pivot failed")
	}

	// We should never get here when the ip-config pre-pivot command succeeds.

	return nil
}

func (h *IPCConfigStageHandler) validateIPCNetworkSpec(ctx context.Context, ipc *ipcv1.IPConfig) error {
	if err := validateAddressChanges(ipc); err != nil {
		return fmt.Errorf("validation of IP address changes failed: %w", err)
	}

	if err := h.validateClusterAndNetworkSpecCompatability(ctx, ipc); err != nil {
		return fmt.Errorf("validation of cluster and network spec compatibility failed: %w", err)
	}

	return nil
}

func (h *IPCConfigStageHandler) validateClusterAndNetworkSpecCompatability(
	ctx context.Context,
	ipc *ipcv1.IPConfig,
) error {
	nodeIPs, err := lcautils.GetNodeInternalIPs(ctx, h.Client)
	if err != nil {
		return fmt.Errorf("failed to get node internal IPs: %w", err)
	}

	clusterHasIPv4, clusterHasIPv6 := common.DetectClusterIPFamilies(nodeIPs)

	if ipc.Spec.IPv4 != nil && !clusterHasIPv4 {
		return fmt.Errorf("specified IPv4 in the spec, but the cluster does not have IPv4")
	}

	if ipc.Spec.IPv6 != nil && !clusterHasIPv6 {
		return fmt.Errorf("specified IPv6 in the spec, but the cluster does not have IPv6")
	}

	for _, s := range ipc.Spec.DNSServers {
		ip := net.ParseIP(s)
		if ip == nil {
			return fmt.Errorf("dnsServers contains an invalid IP: %q", s)
		}

		if ip.To4() != nil && !clusterHasIPv4 {
			return fmt.Errorf("dnsServers contains an IPv4 address %q but the cluster does not have IPv4", s)
		}

		// Note: ip.To16() is non-nil for IPv4 addresses too; ensure we only treat
		// it as IPv6 when it's not IPv4.
		if ip.To4() == nil && ip.To16() != nil && !clusterHasIPv6 {
			return fmt.Errorf("dnsServers contains an IPv6 address %q but the cluster does not have IPv6", s)
		}
	}

	if ipc.Spec.DNSFilterOutFamily != "" &&
		ipc.Spec.DNSFilterOutFamily != common.DNSFamilyNone &&
		!(clusterHasIPv4 && clusterHasIPv6) {
		return fmt.Errorf("dnsFilterOutFamily is supported only on dual-stack clusters")
	}

	return nil
}

// validateAddressChanges enforces that for a single IP family (IPv4/IPv6),
// machineNetwork / gateway are only allowed to change when the
// address changes as well. Gateway / machineNetwork change without address change are not supported
// at the moment.
func validateAddressChanges(ipc *ipcv1.IPConfig) error {
	if ipc == nil {
		return nil
	}

	if v4 := ipc.Spec.IPv4; v4 != nil {
		if err := validateFamilyAddressChanges(
			common.IPv4FamilyName,
			v4,
			ipc.Status.IPv4,
		); err != nil {
			return err
		}
	}

	if v6 := ipc.Spec.IPv6; v6 != nil {
		if err := validateFamilyAddressChanges(
			common.IPv6FamilyName,
			v6,
			ipc.Status.IPv6,
		); err != nil {
			return err
		}
	}

	if len(ipc.Spec.DNSServers) > 0 && len(ipc.Status.DNSServers) > 0 &&
		!reflect.DeepEqual(ipc.Spec.DNSServers, ipc.Status.DNSServers) {
		ipChanged := false
		if ipc.Spec.IPv4 != nil && ipc.Status.IPv4 != nil && !ipEqual(ipc.Spec.IPv4.Address, ipc.Status.IPv4.Address) {
			ipChanged = true
		}
		if ipc.Spec.IPv6 != nil && ipc.Status.IPv6 != nil && !ipEqual(ipc.Spec.IPv6.Address, ipc.Status.IPv6.Address) {
			ipChanged = true
		}
		if !ipChanged {
			return fmt.Errorf("dnsServers can be changed only if address is also changed")
		}
	}

	return nil
}

// validateFamilyAddressChanges enforces that for a single IP family (IPv4/IPv6),
// machineNetwork / gateway are only allowed to change when the
// address changes as well.
func validateFamilyAddressChanges(
	family string,
	spec interface{},
	status interface{},
) error {
	switch family {
	case common.IPv4FamilyName:
		specV4, _ := spec.(*ipcv1.IPv4Config)
		statusV4, _ := status.(*ipcv1.IPv4Status)
		return validateFamilyAddressChangesV4(specV4, statusV4)
	case common.IPv6FamilyName:
		specV6, _ := spec.(*ipcv1.IPv6Config)
		statusV6, _ := status.(*ipcv1.IPv6Status)
		return validateFamilyAddressChangesV6(specV6, statusV6)
	default:
		return fmt.Errorf("unknown family: %s", family)
	}
}

func validateFamilyAddressChangesV4(spec *ipcv1.IPv4Config, status *ipcv1.IPv4Status) error {
	if spec == nil || status == nil {
		return nil
	}
	if !ipEqual(spec.Address, status.Address) {
		return nil
	}
	if spec.MachineNetwork != "" &&
		status.MachineNetwork != "" &&
		!cidrEqual(spec.MachineNetwork, status.MachineNetwork) {
		return fmt.Errorf("%s machineNetwork can be changed only if address is also changed", common.IPv4FamilyName)
	}
	if spec.Gateway != "" && spec.Gateway != status.Gateway {
		return fmt.Errorf("%s gateway can be changed only if address is also changed", common.IPv4FamilyName)
	}
	return nil
}

func validateFamilyAddressChangesV6(spec *ipcv1.IPv6Config, status *ipcv1.IPv6Status) error {
	if spec == nil || status == nil {
		return nil
	}
	if !ipEqual(spec.Address, status.Address) {
		return nil
	}
	if spec.MachineNetwork != "" &&
		status.MachineNetwork != "" &&
		!cidrEqual(spec.MachineNetwork, status.MachineNetwork) {
		return fmt.Errorf("%s machineNetwork can be changed only if address is also changed", common.IPv6FamilyName)
	}
	if spec.Gateway != "" && spec.Gateway != status.Gateway {
		return fmt.Errorf("%s gateway can be changed only if address is also changed", common.IPv6FamilyName)
	}

	return nil
}

func (h *IPCConfigStageHandler) validateDNSMasqMCExists(ctx context.Context) error {
	mc := &machineconfigv1.MachineConfig{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: common.DnsmasqMachineConfigName}, mc); err != nil {
		return fmt.Errorf("failed to get dnsmasq machine config: %w", err)
	}

	return nil
}

func (h *IPCConfigTwoPhaseHandler) copyLcaCliToHost(logger logr.Logger) error {
	src := controllerutils.LcaCliBinaryContainerPath
	dst := common.PathOutsideChroot(common.LcaCliBinaryHostPath)
	logger.Info("Copying lca-cli binary", "src", src, "dst", dst)
	if err := h.ChrootOps.CopyFile(src, dst, 0o777); err != nil {
		return fmt.Errorf("failed to copy lca-cli binary: %w", err)
	}

	return nil
}

func (h *IPCConfigStageHandler) validateSNO(ctx context.Context) error {
	_, err := lcautils.GetSNOMasterNode(ctx, h.Client)
	if err != nil {
		return fmt.Errorf("failed to validate SNO master node: %w", err)
	}
	return nil
}

func exportIPConfigForUncontrolledRollback(ipc *ipcv1.IPConfig, chrootOps ops.Ops) error {
	ipcCopy := ipc.DeepCopy()
	controllerutils.SetIPConfigStatusFailed(ipcCopy, "Uncontrolled rollback")
	filePath := common.PathOutsideChroot(common.IPCFilePath)
	raw, err := json.Marshal(ipcCopy)
	if err != nil {
		return fmt.Errorf("failed to marshal copy of IPConfig CR for rollback: %w", err)
	}
	if err := chrootOps.WriteFile(filePath, raw, 0o600); err != nil {
		return fmt.Errorf("failed to save copy of IPConfig CR for rollback: %w", err)
	}
	return nil
}

func (h *IPCConfigStageHandler) validateConfigStart(ctx context.Context, ipc *ipcv1.IPConfig) error {
	if err := h.validateSNO(ctx); err != nil {
		return fmt.Errorf("validation of SNO failed: %w", err)
	}

	if err := h.validateDNSMasqMCExists(ctx); err != nil {
		return fmt.Errorf("validation of DNSMasq machine config failed: %w", err)
	}

	if err := h.validateIPCNetworkSpec(ctx, ipc); err != nil {
		return fmt.Errorf("validation of IPConfig network spec failed: %w", err)
	}

	return nil
}
