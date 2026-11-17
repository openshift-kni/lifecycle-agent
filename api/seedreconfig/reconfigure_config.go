package seedreconfig

import (
	"fmt"
	"strings"
	"time"
)

// This file holds the pure validation/interpretation helper for the
// ReconfigureConvergenceTimeout field that drives the self-contained reconfigure
// result-writer (see lca-cli/initmonitor). It lives here, next to the field definition, so
// the build-time create-usb validation (lca-cli/createusb) and the runtime result-writer
// share one implementation and cannot drift. Both packages already import api/seedreconfig,
// and this package imports nothing internal, so this is the only place both can reach
// without an import cycle. It mirrors clockgate_config.go's ParseMaxSeedAge.

const (
	// DefaultReconfigureConvergenceTimeout is the deadline, anchored at post-pivot start, the
	// reconfigure result-writer waits for the node to converge before recording a failure and
	// halting the node powered-on. On SNO the reconfigure (recert -> reboot -> all cluster
	// operators settled) typically converges in ~20-40 minutes; 60 minutes sits well beyond
	// expected convergence while still bounding failure-detection latency to about an hour. It
	// is configurable per deployment via SeedReconfiguration.ReconfigureConvergenceTimeout.
	DefaultReconfigureConvergenceTimeout = 60 * time.Minute

	// MaxAllowedReconfigureConvergenceTimeout caps a configured ReconfigureConvergenceTimeout so
	// an absurd or overflowing value is rejected with a clear config error rather than silently
	// disabling failure detection for a day or more.
	MaxAllowedReconfigureConvergenceTimeout = 24 * time.Hour
)

// ParseReconfigureConvergenceTimeout validates and resolves a configured
// ReconfigureConvergenceTimeout value. An empty value uses the default (60 minutes); a
// non-empty value must be a positive Go duration string no larger than
// MaxAllowedReconfigureConvergenceTimeout, otherwise a config error is returned so callers
// fail closed rather than silently accepting an implausible window. It is used both at build
// time (create-usb manifest validation) and at runtime (the result-writer).
func ParseReconfigureConvergenceTimeout(configured string) (time.Duration, error) {
	if strings.TrimSpace(configured) == "" {
		return DefaultReconfigureConvergenceTimeout, nil
	}
	d, err := time.ParseDuration(configured)
	if err != nil {
		return 0, fmt.Errorf("reconfigureConvergenceTimeout %q is not a valid duration: %w", configured, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("reconfigureConvergenceTimeout %q must be positive", configured)
	}
	if d > MaxAllowedReconfigureConvergenceTimeout {
		return 0, fmt.Errorf("reconfigureConvergenceTimeout %q exceeds the maximum allowed (%s)", configured, MaxAllowedReconfigureConvergenceTimeout)
	}
	return d, nil
}
