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
//+kubebuilder:resource:path=seedgenerators,shortName=seedgen
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SeedGenerator is the Schema for the seedgenerators API
// +operator-sdk:csv:customresourcedefinitions:displayName="Seed Generator",resources={{Namespace, v1}}
type SeedGenerator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeedGeneratorSpec   `json:"spec,omitempty"`
	Status SeedGeneratorStatus `json:"status,omitempty"`
}

// SeedGeneratorSpec defines the desired state of SeedGenerator
type SeedGeneratorSpec struct {
	SeedImage   string `json:"seedImage,omitempty"`
	RecertImage string `json:"recertImage,omitempty"`
}

// SeedGeneratorStatus defines the observed state of SeedGenerator
type SeedGeneratorStatus struct {
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Status"
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
	StartedAt          metav1.Time `json:"startedAt,omitempty"`
	CompletedAt        metav1.Time `json:"completedAt,omitempty"`
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions"
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true

// SeedGeneratorList contains a list of SeedGenerator
type SeedGeneratorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeedGenerator `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeedGenerator{}, &SeedGeneratorList{})
}
