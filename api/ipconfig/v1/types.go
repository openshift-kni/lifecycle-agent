/*
Copyright 2025.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=ipconfigs,scope=Cluster,shortName=ipc
// +operator-sdk:csv:customresourcedefinitions:displayName="IP Configuration",resources={{Namespace, v1}}
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Desired Stage",type="string",JSONPath=".spec.stage"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".status.conditions[-1:].reason"
// +kubebuilder:printcolumn:name="Details",type="string",JSONPath=".status.conditions[-1:].message"
// +kubebuilder:printcolumn:name="Current IPv4",type="string",JSONPath=".status.network.clusterNetwork.ipv4.address",priority=1
// +kubebuilder:printcolumn:name="Desired IPv4",type="string",JSONPath=".spec.ipv4.address",priority=1
// +kubebuilder:printcolumn:name="Current IPv6",type="string",JSONPath=".status.network.clusterNetwork.ipv6.address",priority=1
// +kubebuilder:printcolumn:name="Desired IPv6",type="string",JSONPath=".spec.ipv6.address",priority=1
// +kubebuilder:validation:XValidation:message="ipconfig is a singleton, metadata.name must be 'ipconfig'", rule="self.metadata.name == 'ipconfig'"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv4 while ipconfig is not idle",rule="!has(oldSelf.status) || oldSelf.status.conditions.exists(c, c.type=='Idle' && c.status=='True') || has(oldSelf.spec.ipv4) && has(self.spec.ipv4) && oldSelf.spec.ipv4==self.spec.ipv4 || !has(self.spec.ipv4) && !has(oldSelf.spec.ipv4)"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv6 while ipconfig is not idle",rule="!has(oldSelf.status) || oldSelf.status.conditions.exists(c, c.type=='Idle' && c.status=='True') || has(oldSelf.spec.ipv6) && has(self.spec.ipv6) && oldSelf.spec.ipv6==self.spec.ipv6 || !has(self.spec.ipv6) && !has(oldSelf.spec.ipv6)"
// +kubebuilder:validation:XValidation:message="can not change spec.vlan while ipconfig is not idle",rule="!has(oldSelf.status) || oldSelf.status.conditions.exists(c, c.type=='Idle' && c.status=='True') || has(oldSelf.spec.vlan) && has(self.spec.vlan) && oldSelf.spec.vlan==self.spec.vlan || !has(self.spec.vlan) && !has(oldSelf.spec.vlan)"
// +kubebuilder:validation:XValidation:message="can not change spec.dnsResolutionFamily while ipconfig is not idle",rule="!has(oldSelf.status) || oldSelf.status.conditions.exists(c, c.type=='Idle' && c.status=='True') || has(oldSelf.spec.dnsResolutionFamily) && has(self.spec.dnsResolutionFamily) && oldSelf.spec.dnsResolutionFamily==self.spec.dnsResolutionFamily || !has(self.spec.dnsResolutionFamily) && !has(oldSelf.spec.dnsResolutionFamily)"
// +kubebuilder:validation:XValidation:message="can not change spec.autoRollbackOnFailure while ipconfig is not idle",rule="!has(oldSelf.status) || oldSelf.status.conditions.exists(c, c.type=='Idle' && c.status=='True') || has(oldSelf.spec.autoRollbackOnFailure) && has(self.spec.autoRollbackOnFailure) && oldSelf.spec.autoRollbackOnFailure==self.spec.autoRollbackOnFailure || !has(self.spec.autoRollbackOnFailure) && !has(oldSelf.spec.autoRollbackOnFailure)"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv4.machineNetwork unless spec.ipv4.address also changes",rule="!(has(oldSelf.spec.ipv4) && has(self.spec.ipv4)) || oldSelf.spec.ipv4.machineNetwork == self.spec.ipv4.machineNetwork || oldSelf.spec.ipv4.address != self.spec.ipv4.address"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv4.gateway unless spec.ipv4.address also changes",rule="!(has(oldSelf.spec.ipv4) && has(self.spec.ipv4)) || oldSelf.spec.ipv4.gateway == self.spec.ipv4.gateway || oldSelf.spec.ipv4.address != self.spec.ipv4.address"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv4.dnsServer unless spec.ipv4.address also changes",rule="!(has(oldSelf.spec.ipv4) && has(self.spec.ipv4)) || oldSelf.spec.ipv4.dnsServer == self.spec.ipv4.dnsServer || oldSelf.spec.ipv4.address != self.spec.ipv4.address"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv6.machineNetwork unless spec.ipv6.address also changes",rule="!(has(oldSelf.spec.ipv6) && has(self.spec.ipv6)) || oldSelf.spec.ipv6.machineNetwork == self.spec.ipv6.machineNetwork || oldSelf.spec.ipv6.address != self.spec.ipv6.address"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv6.gateway unless spec.ipv6.address also changes",rule="!(has(oldSelf.spec.ipv6) && has(self.spec.ipv6)) || oldSelf.spec.ipv6.gateway == self.spec.ipv6.gateway || oldSelf.spec.ipv6.address != self.spec.ipv6.address"
// +kubebuilder:validation:XValidation:message="can not change spec.ipv6.dnsServer unless spec.ipv6.address also changes",rule="!(has(oldSelf.spec.ipv6) && has(self.spec.ipv6)) || oldSelf.spec.ipv6.dnsServer == self.spec.ipv6.dnsServer || oldSelf.spec.ipv6.address != self.spec.ipv6.address"

// IPConfig is the Schema for controlling node IP configuration lifecycle via lca-cli ip-config.
type IPConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPConfigSpec   `json:"spec,omitempty"`
	Status IPConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// IPConfigList contains a list of IPConfig
type IPConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPConfig `json:"items"`
}

// IPConfigStage defines the type for the stage field
type IPConfigStage string

var IPStages = struct {
	Idle     IPConfigStage
	Config   IPConfigStage
	Rollback IPConfigStage
}{
	Idle:     "Idle",
	Config:   "Config",
	Rollback: "Rollback",
}

// IPFamilyConfig represents a single stack configuration
type IPFamilyConfig struct {
	// +kubebuilder:validation:Required
	// Address is the full address with prefix length (e.g., 192.0.2.10/24)
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Address string `json:"address,omitempty"`
	// +kubebuilder:validation:Required
	// MachineNetwork is the CIDR of the machine network (e.g., 192.0.2.0/24)
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	MachineNetwork string `json:"machineNetwork,omitempty"`
	// +kubebuilder:validation:Required
	// Gateway is the default gateway address
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Gateway string `json:"gateway,omitempty"`
	// +kubebuilder:validation:Required
	// DNSServer is the DNS server IP to use
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	DNSServer string `json:"dnsServer,omitempty"`
}

// VLANConfig represents optional VLAN configuration for the detected br-ex path
type VLANConfig struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	ID int `json:"id,omitempty"`
}

// IPConfigSpec defines the desired state of IPConfig
type IPConfigSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Idle;Config;Rollback
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Stage",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:select"}
	Stage IPConfigStage `json:"stage,omitempty"`

	// IPv4 stack (omit for IPv6-only)
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="IPv4"
	IPv4 *IPFamilyConfig `json:"ipv4,omitempty"`

	// IPv6 stack (omit for IPv4-only)
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="IPv6"
	IPv6 *IPFamilyConfig `json:"ipv6,omitempty"`

	// Optional VLAN applied to br-ex path
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="VLAN"
	VLAN *VLANConfig `json:"vlan,omitempty"`

	// DNSResolutionFamily selects the IP family to resolve DNS records to on dual-stack clusters.
	// When set, the other IP family will be filtered out from DNS responses.
	// +kubebuilder:validation:Enum=ipv4;ipv6
	// +optional
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="DNS Resolution Family",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:select"}
	DNSResolutionFamily string `json:"dnsResolutionFamily,omitempty"`

	// AutoRollbackOnFailure defines automatic rollback settings for IPConfig if the configuration
	// does not complete within the specified time limit. Behavior mirrors IBU.
	// +optional
	//+operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Auto Rollback On Failure"
	AutoRollbackOnFailure *AutoRollbackOnFailure `json:"autoRollbackOnFailure,omitempty"`
}

// AutoRollbackOnFailure defines automatic rollback settings if the IP configuration does not
// complete within the specified time limit.
type AutoRollbackOnFailure struct {
	// InitMonitorTimeoutSeconds defines the time frame in seconds. If not defined or set to 0,
	// the default value of 1800 seconds (30 minutes) is used.
	// +kubebuilder:validation:Minimum=0
	//+operator-sdk:csv:customresourcedefinitions:type=spec,xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	InitMonitorTimeoutSeconds int `json:"initMonitorTimeoutSeconds,omitempty"`
}

// IPConfigStatus defines the observed state of IPConfig
type IPConfigStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Observed Generation"
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions",xDescriptors={"urn:alm:descriptor:io.kubernetes.conditions"}
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ValidNextStages enumerates allowed next transitions from current stage
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Valid Next Stage"
	ValidNextStages []IPConfigStage `json:"validNextStages,omitempty"`

	// Network groups host and cluster network information
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Network"
	Network *NetworkStatus `json:"network,omitempty"`

	// DNSResolutionFamily reports the active DNS response filtering family:
	// "ipv4", "ipv6" or "none" (no filter set)
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="DNS Resolution Family"
	DNSResolutionFamily string `json:"dnsResolutionFamily,omitempty"`

	// History stores timing info of different IPConfig stages and their important phases
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="History"
	History []*IPHistory `json:"history,omitempty"`
}

type HostIPStatus struct {
	// DNSServer is the DNS server IP to use
	DNSServer string `json:"dnsServer,omitempty"`
	// Gateway is the default gateway address
	Gateway string `json:"gateway,omitempty"`
}

// HostNetworkStatus summarizes current host network
type HostNetworkStatus struct {
	// IPv4 summarizes the current IPv4 on the host network
	IPv4 *HostIPStatus `json:"ipv4,omitempty"`
	// IPv6 summarizes the current IPv6 on the host network
	IPv6 *HostIPStatus `json:"ipv6,omitempty"`
	// VLANID is the VLAN identifier on the br-ex uplink path, if any
	VLANID int `json:"vlanId,omitempty"`
}

// ClusterNetworkStatus summarizes cluster network using lists of strings
type ClusterNetworkStatus struct {
	// IPv4 summarizes the current IPv4 on the cluster network
	IPv4 *ClusterIPStatus `json:"ipv4,omitempty"`
	// IPv6 summarizes the current IPv6 on the cluster network
	IPv6 *ClusterIPStatus `json:"ipv6,omitempty"`
}

// NetworkStatus groups host and cluster network views
type NetworkStatus struct {
	// HostNetwork summarizes current host network
	HostNetwork *HostNetworkStatus `json:"hostNetwork,omitempty"`
	// ClusterNetwork summarizes cluster network using lists of strings
	ClusterNetwork *ClusterNetworkStatus `json:"clusterNetwork,omitempty"`
}

// ClusterIPStatus represents a single IP family view on the cluster network
type ClusterIPStatus struct {
	// Address is the node internal IP (plain address, no prefix)
	Address string `json:"address,omitempty"`
	// MachineNetwork is the matching machine network CIDR for the IP
	MachineNetwork string `json:"machineNetwork,omitempty"`
}

// IPHistory mirrors IBU history for IPConfig stages
type IPHistory struct {
	// Stage The desired stage name read from spec
	Stage IPConfigStage `json:"stage,omitempty"`
	// Phases allows a granular view of important tasks within a Stage
	Phases []*IPPhase `json:"phases,omitempty"`
	// StartTime A timestamp to indicate the Stage has started
	StartTime metav1.Time `json:"startTime,omitempty"`
	// CompletionTime A timestamp indicating the Stage completed successfully
	CompletionTime metav1.Time `json:"completionTime,omitempty"`
}

// IPPhase represents a sub-step within a stage
type IPPhase struct {
	// Phase current phase within a Stage
	Phase string `json:"phase,omitempty"`
	// StartTime A timestamp indicating the Phase has started
	StartTime metav1.Time `json:"startTime,omitempty"`
	// CompletionTime A timestamp indicating the phase completed successfully
	CompletionTime metav1.Time `json:"completionTime,omitempty"`
}

func init() {
	SchemeBuilder.Register(&IPConfig{}, &IPConfigList{})
}
