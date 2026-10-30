package lcacli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// unitDirectives returns the values of every "key=value" line in a systemd unit, keyed by key
func unitDirectives(t *testing.T, unitPath string) map[string][]string {
	data, err := os.ReadFile(unitPath)
	require.NoError(t, err)

	directives := map[string][]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found {
			directives[key] = append(directives[key], value)
		}
	}
	return directives
}

// The nmconnection profiles must be restored before NetworkManager first starts, otherwise NM
// auto-activates a default DHCP profile on the NIC and keeps it (OCPBUGS-135078). Anything that
// delays the restore past NetworkManager startup, such as waiting for coreos-update-ca-trust.service
// like installation-configuration.service does, brings the race back.
func TestRestoreNMConnectionsServiceOrdering(t *testing.T) {
	directives := unitDirectives(t, filepath.Join("installation_configuration_files", "services", "lca-restore-nmconnections.service"))

	assert.Contains(t, directives["Before"], "NetworkManager.service")
	for _, after := range directives["After"] {
		assert.NotContains(t, after, "coreos-update-ca-trust.service")
		assert.NotContains(t, after, "NetworkManager")
	}
	assert.Equal(t, []string{"/usr/local/bin/lca-cli restore-nmconnections"}, directives["ExecStart"])
	assert.Equal(t, []string{"multi-user.target"}, directives["WantedBy"])

	// The condition must point at the folder post-pivot copies from, as seen from the new stateroot
	// (/opt is a symlink to /var/opt on RHCOS)
	assert.Equal(t,
		[]string{filepath.Join("/var", common.OptOpenshift, common.NetworkDir, "system-connections")},
		directives["ConditionDirectoryNotEmpty"])
}
