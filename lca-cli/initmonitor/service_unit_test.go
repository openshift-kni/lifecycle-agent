package initmonitor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconfigureResultWriterServiceUnit asserts the shipped systemd unit has the ordering and
// gating the design depends on: After= (ordering only, not Requires=/Requisite=),
// WantedBy=multi-user.target, TimeoutStartSec=0, and ConditionPathExists on both the handoff
// and the post-pivot completion marker.
func TestReconfigureResultWriterServiceUnit(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	unitPath := filepath.Join(filepath.Dir(thisFile), "..", "installation_configuration_files", "services", "lca-reconfigure-result-writer.service")
	data, err := os.ReadFile(unitPath) //nolint:gosec // fixed in-repo test path
	require.NoError(t, err)
	unit := string(data)

	assert.Contains(t, unit, "After=installation-configuration.service")
	assert.Contains(t, unit, "ConditionPathExists=/var/lib/lca/reconfigure-handoff.json")
	assert.Contains(t, unit, "ConditionPathExists=/var/lib/lca/reconfigure-completed")
	assert.Contains(t, unit, "Type=oneshot")
	assert.Contains(t, unit, "TimeoutStartSec=0")
	assert.Contains(t, unit, "ExecStart=/usr/local/bin/lca-cli init-monitor --reconfigure-result-writer")
	assert.Contains(t, unit, "WantedBy=multi-user.target")

	// Ordering only: a hard dependency would misread the RemainAfterExit=no post-pivot's
	// successful (inactive) exit as a failure. Check active directive lines (ignoring comments)
	// so the explanatory comment mentioning these keywords does not trip the assertion.
	for _, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			continue
		}
		assert.False(t, strings.HasPrefix(line, "Requisite="), "unit must not use Requisite=: %q", line)
		assert.False(t, strings.HasPrefix(line, "Requires="), "unit must not use Requires=: %q", line)
	}
}
