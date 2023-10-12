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

package v1beta1

import (
	configv1 "github.com/openshift/api/config/v1"
	agentv1 "github.com/stolostron/klusterlet-addon-controller/pkg/apis/agent/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterRelocationSpec defines the desired state of ClusterRelocation
type ClusterRelocationSpec struct {
	// Important: Run "make" to regenerate code after modifying this file

	// ACMRegistration allows you to register this cluster to a remote ACM cluster.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	ACMRegistration *ACMRegistration `json:"acmRegistration,omitempty"`

	// AddInternalDNSEntries deploys a MachineConfig which adds api and *.apps entries for the new domain to dnsmasq on SNO clusters.
	// Setting this to true will cause a reboot.
	// If you don't enable this option, you need to make sure that the cluster can resolve the new domain address via some other method.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	AddInternalDNSEntries *bool `json:"addInternalDNSEntries,omitempty"`

	// APICertRef is a reference to a TLS secret that will be used for the API server.
	// If it is omitted, a certificate will be generated and signed by loadbalancer-serving-signer.
	// The type of the secret must be kubernetes.io/tls.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	APICertRef *corev1.SecretReference `json:"apiCertRef,omitempty"`

	// CatalogSources define new CatalogSources to install on the cluster.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	CatalogSources []CatalogSource `json:"catalogSources,omitempty"`

	// Domain defines the new base domain for the cluster.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	Domain string `json:"domain"`

	// ImageDigestMirrors is used to configured a mirror registry on the cluster.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	ImageDigestMirrors []configv1.ImageDigestMirrors `json:"imageDigestMirrors,omitempty"`

	// IngressCertRef is a reference to a TLS secret that will be used for the Ingress Controller.
	// If it is omitted, a certificate will be generated and signed by loadbalancer-serving-signer.
	// The type of the secret must be kubernetes.io/tls.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	IngressCertRef *corev1.SecretReference `json:"ingressCertRef,omitempty"`

	// PullSecretRef is a reference to new cluster-wide pull secret.
	// If defined, it will replace the secret located at openshift-config/pull-secret.
	// The type of the secret must be kubernetes.io/dockerconfigjson.
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	PullSecretRef *corev1.SecretReference `json:"pullSecretRef,omitempty"`

	// RegistryCert is a new trusted CA certificate.
	// It will be added to image.config.openshift.io/cluster (additionalTrustedCA).
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	RegistryCert *RegistryCert `json:"registryCert,omitempty"`

	// SSHKeys defines a list of authorized SSH keys for the 'core' user.
	// If defined, it will be appended to the existing authorized SSH key(s).
	//+operator-sdk:csv:customresourcedefinitions:type=spec
	SSHKeys []string `json:"sshKeys,omitempty"`
}

// ClusterRelocationStatus defines the observed state of ClusterRelocation
type ClusterRelocationStatus struct {
	// Important: Run "make" to regenerate code after modifying this file

	// Conditions represent the latest available observations of an object's state
	//+operator-sdk:csv:customresourcedefinitions:type=status
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster

// ClusterRelocation is the Schema for the clusterrelocations API
// +operator-sdk:csv:customresourcedefinitions:resources={{Secret,v1,"generated-api-secret"},{Secret,v1,"generated-ingress-secret"}}
type ClusterRelocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterRelocationSpec   `json:"spec"`
	Status ClusterRelocationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterRelocationList contains a list of ClusterRelocation
type ClusterRelocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRelocation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterRelocation{}, &ClusterRelocationList{})
}

type CatalogSource struct {
	// Name is the name of the CatalogSource.
	Name string `json:"name"`

	// Image is an operator-registry container image to instantiate a registry-server with.
	Image string `json:"image"`
}

type RegistryCert struct {
	// RegistryHostname is the hostname of the new registry.
	RegistryHostname string `json:"registryHostname"`

	// RegistryPort is the port number that the registry is served on.
	RegistryPort *int `json:"registryPort,omitempty"`

	// Certificate is the certificate for the trusted certificate authority associated with the registry.
	Certificate string `json:"certificate"`
}

type ACMRegistration struct {
	// URL is the API URL of the ACM cluster.
	URL string `json:"url"`

	// ClusterName will be the name of the ManagedCluster in ACM.
	ClusterName string `json:"clusterName"`

	// ManagedClusterSet is the ManagedClusterSet that the ManagedCluster will join. Defaults to 'default'.
	ManagedClusterSet *string `json:"managedClusterSet,omitempty"`

	// acmSecret is a secret reference with credentials for the ACM cluster.
	// It must have a 'token' field. Optionally, it can have a 'ca.crt' field
	// which provides the CA bundle for the ACM cluster.
	// The secret is deleted once ACM registration succeeds.
	// The type of the secret must be Opaque.
	ACMSecret corev1.SecretReference `json:"acmSecret"`

	// KlusterletAddonConfig is the klusterlet add-on configuration.
	KlusterletAddonConfig *agentv1.KlusterletAddonConfigSpec `json:"klusterletAddonConfig,omitempty"`
}

const (
	ConditionTypeReady      string = "Ready"
	ConditionTypeReconciled string = "Reconciled"
)

const (
	PullSecretName       string = "pull-secret"
	BackupPullSecretName string = "backup-pull-secret"
	ConfigNamespace      string = "openshift-config"
	IngressNamespace     string = "openshift-ingress"
)

const (
	// ValidationSucceededReason represents the fact that the validation of
	// the resource has succeeded.
	ValidationSucceededReason string = "ValidationSucceeded"

	// ValidationFailedReason represents the fact that the validation of
	// the resource has failed.
	ValidationFailedReason string = "ValidationFailed"

	// ReconciliationSucceededReason represents the fact that the validation of
	// the resource has succeeded.
	ReconciliationSucceededReason string = "ReconciliationSucceeded"

	APIReconciliationFailedReason        string = "APIReconciliationFailed"
	IngressReconciliationFailedReason    string = "IngressReconciliationFailed"
	PullSecretReconciliationFailedReason string = "PullSecretReconciliationFailed"
	SSHReconciliationFailedReason        string = "SSHReconciliationFailed"
	RegistryReconciliationFailedReason   string = "RegistryReconciliationFailed"
	MirrorReconciliationFailedReason     string = "MirrorReconciliationFailed"
	CatalogReconciliationFailedReason    string = "CatalogReconciliationFailed"
	DNSReconciliationFailedReason        string = "DNSReconciliationFailed"
	ACMReconciliationFailedReason        string = "ACMReconciliationFailed"
	InProgressReconciliationFailedReason string = "ReconcileInProgress"
)
