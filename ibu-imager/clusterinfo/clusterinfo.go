package clusterinfo

import (
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ClusterInfo struct that describe current cluster critical info
type ClusterInfo struct {
	Version                  string `json:"version,omitempty"`
	Domain                   string `json:"domain,omitempty"`
	ClusterName              string `json:"cluster_name,omitempty"`
	ClusterID                string `json:"cluster_id,omitempty"`
	MasterIP                 string `json:"master_ip,omitempty"`
	ReleaseRegistry          string `json:"release_registry,omitempty"`
	Hostname                 string `json:"hostname,omitempty"`
	MirrorRegistryConfigured bool   `json:"mirror_registry_configured,omitempty"`
}

type installConfigMetadata struct {
	Name string `json:"name"`
}

type BasicInstallConfig struct {
	BaseDomain string                `json:"baseDomain"`
	Metadata   installConfigMetadata `json:"metadata"`
}

// InfoClient client to create cluster info object
type InfoClient struct {
	client runtimeclient.Client
}

func NewClusterInfoClient(client runtimeclient.Client) *InfoClient {
	return &InfoClient{
		client: client,
	}
}
