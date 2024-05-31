package seedreconfig

type PEM string

const (
	SeedReconfigurationVersion = 1
	// BlockDeviceLabel is a configuration volume label to set while providing iso with configuration files
	BlockDeviceLabel = "cluster-config"
)

// SeedReconfiguration contains all the information that is required to
// transform a machine started from a SNO seed image (which contains dummy seed
// configuration) into a SNO cluster with a desired configuration. During an
// IBU, this information is taken from the cluster that is being upgraded (the
// original SNO's LCA writes a file to the seed stateroot to communicate this
// information). During an IBI, this information is placed in the configuration
// ISO created by the image-based-install-operator.
//
// WARNING: Changes to this struct and its sub-structs should not be made
// lightly, as it is also used by the image-based-install-operator. Any changes
// made here will also need to be handled in the image-based-install-operator
// or be backwards compatible. If you've made a breaking change, you will need
// to increment the SeedReconfigVersion constant to avoid silent breakage and
// allow for backwards compatibility code.
type SeedReconfiguration struct {
	// The version of the SeedReconfiguration struct format. This is used to detect
	// breaking changes to the struct.
	APIVersion int `json:"api_version"`

	// The desired base domain for the cluster. Equivalent to install-config.yaml's baseDomain.
	// This will replace the base domain of the seed cluster.
	BaseDomain string `json:"base_domain,omitempty"`

	// The desired cluster name for the cluster. Equivalent to install-config.yaml's clusterName.
	// This will replace the cluster name of the seed cluster.
	ClusterName string `json:"cluster_name,omitempty"`

	// The desired cluster-ID. During an IBU, this is the ID of the original
	// SNO. During an IBI, this can either be empty, in which case a new
	// cluster-ID will be generated by LCA, or it can be set to the ID of the
	// new cluster, in case one had to be pre-generated for some reason.
	ClusterID string `json:"cluster_id,omitempty"`

	// The desired infra ID for the cluster. No install-config.yaml equivalent,
	// as it is randomly generated from the cluster name + some random chars.
	// Similarly to cluster-ID, this can either be empty, in which case a new
	// infra-ID will be generated by LCA, or it can be set to the infra ID of
	// the new cluster, in case one had to be pre-generated for some reason.
	InfraID string `json:"infra_id,omitempty"`

	// The desired IP address of the SNO node.
	NodeIP string `json:"node_ip,omitempty"`

	// The container registry used to host the release image of the seed cluster.
	ReleaseRegistry string `json:"release_registry,omitempty"`

	// The desired hostname of the SNO node.
	Hostname string `json:"hostname,omitempty"`

	// KubeconfigCryptoRetention contains all the crypto material that is
	// required for recert to ensure existing kubeconfigs can be used to access
	// the cluster after recert.
	//
	// In the case of UBI, this material is taken from the cluster that is
	// being upgraded. In the case of IBI, this material is generated when the
	// cluster's kubeconfig is being prepared in advance.
	KubeconfigCryptoRetention KubeConfigCryptoRetention

	// SSHKey is the public Secure Shell (SSH) key to provide access to
	// instances. Equivalent to install-config.yaml's sshKey. This will replace
	// the SSH keys of the seed cluster.
	SSHKey string `json:"ssh_key,omitempty"`

	// KubeadminPasswordHash is the hash of the password for the kubeadmin
	// user, as can be found in the kubeadmin key of the kube-system/kubeadmin
	// secret. This will replace the kubeadmin password of the seed cluster.
	// The seed image must have an existing kubeadmin password secret, this is
	// because the secret is rejected by OCP if its creation timestamp differs
	// from the kube-system namespace creation timestamp by more than 1 hour.
	// During IBU, this is taken from the cluster that is being upgraded.
	// During IBI, this is generated in advance so it can be displayed to the
	// user. If empty, the kubeadmin password secret of the seed cluster will
	// be deleted (thus disabling kubeadmin password login), to ensure we're
	// not accepting a possibly compromised seed password.
	KubeadminPasswordHash string `json:"kubeadmin_password_hash,omitempty"`

	// RawNMStateConfig contains nmstate configuration YAML file provided as string.
	// String will be written to file and will be applied with "nmstatectl apply" command.
	// Example of nmstate configurations can be found in this link https://nmstate.io/examples.html
	// This field will be used for IBI process as in IBU we will copy nmconnection files
	RawNMStateConfig string `json:"raw_nm_state_config,omitempty"`

	// PullSecret is the secret to use when pulling images. Equivalent to install-config.yaml's pullSecret.
	PullSecret string `json:"pull_secret,omitempty"`

	// MachineNetwork is the subnet provided by user for the ocp cluster.
	// This will be used to create the node network and choose ip address for the node.
	// Equivalent to install-config.yaml's machineNetwork.
	MachineNetwork string `json:"machine_network,omitempty"`

	// Proxy is the proxy settings for the cluster. Equivalent to
	// install-config.yaml's proxy. This will replace the proxy settings of the
	// seed cluster. During IBI, the HTTP and HTTPS and NO proxy settings are
	// given by the user, and IBIO will also calculate a final NO_PROXY
	// configuration based on the user provided NO_PROXY and the cluster's
	// configuration. During an IBU, this is simply taken from the cluster's
	// Proxy CR .spec field.
	Proxy *Proxy `json:"proxy,omitempty"`

	// StatusProxy is the Proxy configuration from the Proxy CR's status field.
	// The Cluster Network Operator calculates a proxy configuration based on
	// the user provided proxy configuration (taken from the Proxy CR's spec)
	// and puts that proxy configuration in the Proxy CR's status. The status
	// proxy configuration is the one that's actually used by the operators in
	// the cluster. During a seed reconfiguration, we need to know both the
	// status proxy configuration and the user's spec proxy configuration, as
	// both are required to perform a rollout free seed reconfiguration.
	// Otherwise we would have to teach recert how to calculate the status
	// proxy from the spec proxy which is non-trivial. During an IBU, we simply
	// take this status from the cluster's Proxy CR .status field. During an
	// IBI, IBIO should do its best to emulate the behavior of the CNO so that
	// the status proxy configuration is identical to what CNO would have
	// calculated, so that rollouts are avoided.
	StatusProxy *Proxy `json:"status_proxy,omitempty"`

	// The install-config.yaml used to generate the cluster. This is used to
	// populate the cluster-config-v1 configmaps in the cluster, which usually
	// hold a slightly modified version of the user's installation
	// install-config.yaml. Changing this is important because the Cluster
	// Network Operator uses the information in this configmap to set the Proxy
	// CR's no_proxy status field. In IBU, LCA simply copies it from the
	// upgraded cluster. In IBI, a fake install-config should be generated by
	// the IBIO. This parameter is required when the proxy parameters are set.
	InstallConfig string `json:"install_config,omitempty"`

	// The chrony configuration to be used in the cluster. This is used to
	// populate the /etc/chrony.conf file on the node.
	// This file is used to configure the chrony service on the node and to provide ntp servers.
	// In IBU case data will be taken from the upgraded cluster /etc/chrony.conf file.
	// In IBI case data will be taken from the user provided configuration.
	ChronyConfig string `json:"chrony_config,omitempty"`
}

type KubeConfigCryptoRetention struct {
	KubeAPICrypto  KubeAPICrypto
	IngresssCrypto IngresssCrypto
}

type KubeAPICrypto struct {
	ServingCrypto    ServingCrypto
	ClientAuthCrypto ClientAuthCrypto
}

type ServingCrypto struct {
	LocalhostSignerPrivateKey      PEM `json:"localhost_signer_private_key,omitempty"`
	ServiceNetworkSignerPrivateKey PEM `json:"service_network_signer_private_key,omitempty"`
	LoadbalancerSignerPrivateKey   PEM `json:"loadbalancer_external_signer_private_key,omitempty"`
}

type ClientAuthCrypto struct {
	AdminCACertificate PEM `json:"admin_ca_certificate,omitempty"`
}

type IngresssCrypto struct {
	IngressCA PEM `json:"ingress_ca,omitempty"`
}

// Proxy defines the proxy settings for the cluster.
// At least one of HTTPProxy or HTTPSProxy is required.
// Aims to be the same as https://github.com/openshift/installer/blob/ad59622147974f2d2d62bcdeaf342ae4f87ed84f/pkg/types/installconfig.go#L454-L468
type Proxy struct {
	// HTTPProxy is the URL of the proxy for HTTP requests.
	// +optional
	HTTPProxy string `json:"httpProxy,omitempty"`

	// HTTPSProxy is the URL of the proxy for HTTPS requests.
	// +optional
	HTTPSProxy string `json:"httpsProxy,omitempty"`

	// NoProxy is a comma-separated list of domains and CIDRs for which the proxy should not be used.
	// +optional
	NoProxy string `json:"noProxy,omitempty"`
}
