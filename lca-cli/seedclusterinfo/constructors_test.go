package seedclusterinfo

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/utils"
)

func TestNewFromClusterInfo(t *testing.T) {
	clusterInfo := &utils.ClusterInfo{
		OCPVersion:               "4.20.1",
		BaseDomain:               "example.com",
		ClusterName:              "seed",
		NodeIPs:                  []string{"192.168.10.5", "fd00::5"},
		ReleaseRegistry:          "quay.io",
		Hostname:                 "seed-master",
		MirrorRegistryConfigured: true,
		MachineNetworks:          []string{"192.168.10.0/24"},
	}
	bundle := &AdditionalTrustBundle{HasUserCaBundle: true, ProxyConfigmapName: "user-ca-bundle"}

	info := NewFromClusterInfo(clusterInfo, "quay.io/recert:latest", true, true, bundle,
		"/dev/disk/by-partlabel/varlibcontainers", "ingress-operator@1700000000")

	assert.Equal(t, &SeedClusterInfo{
		SeedClusterOCPVersion:            "4.20.1",
		BaseDomain:                       "example.com",
		ClusterName:                      "seed",
		NodeIPs:                          []string{"192.168.10.5", "fd00::5"},
		ReleaseRegistry:                  "quay.io",
		SNOHostname:                      "seed-master",
		MirrorRegistryConfigured:         true,
		RecertImagePullSpec:              "quay.io/recert:latest",
		HasProxy:                         true,
		HasFIPS:                          true,
		AdditionalTrustBundle:            bundle,
		MachineNetworks:                  []string{"192.168.10.0/24"},
		ContainerStorageMountpointTarget: "/dev/disk/by-partlabel/varlibcontainers",
		IngressCertificateCN:             "ingress-operator@1700000000",
	}, info)
}

func TestReadSeedClusterInfoFromFile(t *testing.T) {
	dir := t.TempDir()
	in := &SeedClusterInfo{SeedClusterOCPVersion: "4.20.1", ClusterName: "seed", NodeIPs: []string{"10.0.0.5"}}
	file := filepath.Join(dir, common.SeedClusterInfoFileName)
	assert.NoError(t, utils.MarshalToFile(in, file))

	out, err := ReadSeedClusterInfoFromFile(file)
	assert.NoError(t, err)
	assert.Equal(t, in, out)

	_, err = ReadSeedClusterInfoFromFile(filepath.Join(dir, "missing.json"))
	assert.ErrorContains(t, err, "failed to create seed cluster info from file")
}
