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

package ipconfig

const (
	// External bridge name and related identifiers
	BridgeExternalName    = "br-ex"
	BrExMachineConfigName = "10-br-ex"

	// Default route destinations used when discovering host gateways via nmstate
	DefaultRouteV4 = "0.0.0.0/0"
	DefaultRouteV6 = "::/0"

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
