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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:path=imagebasedclusterupgrades,shortName=icu
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ImageBasedClusterUpgrade is the Schema for the ImageBasedClusterUpgrades API
// +operator-sdk:csv:customresourcedefinitions:displayName="Image-based Cluster Upgrade",resources={{Namespace, v1},{Deployment,apps/v1}}
type ImageBasedClusterUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImageBasedClusterUpgradeSpec   `json:"spec,omitempty"`
	Status ImageBasedClusterUpgradeStatus `json:"status,omitempty"`
}

// ImageBasedClusterUpgradeSpec defines the desired state of ImageBasedClusterUpgrade
type ImageBasedClusterUpgradeSpec struct {
	Stage            string       `json:"stage,omitempty"`
	SeedImageRef     SeedImageRef `json:"seedImageRef,omitempty"`
	AdditionalImages ConfigMapRef `json:"additionalImages,omitempty"`
	OADPContent      ConfigMapRef `json:"oadpContent,omitempty"`
	ExtraManifests   ConfigMapRef `json:"extraManifests,omitempty"`
	RollbackTarget   string       `json:"rollbackTarget,omitempty"`
}

type SeedImageRef struct {
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
}

type ConfigMapRef struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type ImageBasedClusterUpgradeStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	StartedAt          metav1.Time `json:"startedAt,omitempty"`
	CompletedAt        metav1.Time `json:"completedAt,omitempty"`
	StateRoots         []StateRoot `json:"stateRoots,omitempty"`
}

type StateRoot struct {
	Version   string     `json:"version,omitempty"`
	PodStates []PodState `json:"podStates,omitempty"`
}

type PodState struct {
}

// +kubebuilder:object:root=true
// ImageBasedClusterUpgradeList contains a list of ImageBasedClusterUpgrade
type ImageBasedClusterUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ImageBasedClusterUpgrade `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ImageBasedClusterUpgrade{}, &ImageBasedClusterUpgradeList{})
}
