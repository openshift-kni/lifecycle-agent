package ipconfig

const (
	// External bridge name and related identifiers
	BridgeExternalName    = "br-ex"
	BrExMachineConfigName = "10-br-ex"

	// Systemd unit name for nodeip rerun
	NodeipRerunUnitName = "sno-nodeip-rerun.service"
)

// MachineConfigPool and annotation string keys
const (
	MCPMasterName               = "master"
	MachineConfigDesiredAnnoKey = "machineconfiguration.openshift.io/desiredConfig"
	MachineConfigCurrentAnnoKey = "machineconfiguration.openshift.io/currentConfig"
)

// MachineConfigPool condition type names
const (
	MCPConditionUpdating = "Updating"
	MCPConditionUpdated  = "Updated"
	MCPConditionDegraded = "Degraded"
)
