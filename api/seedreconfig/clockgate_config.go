package seedreconfig

import (
	"fmt"
	"strings"
	"time"
)

// This file holds the pure validation/interpretation helpers for the two
// SeedReconfiguration fields that drive the post-pivot clock gate: MaxSeedAge and
// ChronyConfig. They live here, next to the field definitions, so the build-time
// create-usb validation (lca-cli/createusb) and the runtime clock gate
// (lca-cli/postpivot) share one implementation and cannot drift. Both packages already
// import api/seedreconfig, and this package imports nothing internal, so this is the only
// place both can reach without an import cycle.

const (
	// DefaultMaxSeedAge is the default plausibility ceiling for the system clock relative to
	// the seed's creation time: a conservative warehouse->ship->field shelf life that catches
	// gross RTC errors (an unset/battery-dead clock landing in 1970/2000, or a wildly future
	// date) without tripping on realistic logistics. It is configurable per deployment via
	// SeedReconfiguration.MaxSeedAge. 180 days was confirmed as a reasonable starting default.
	DefaultMaxSeedAge = 180 * 24 * time.Hour

	// MaxAllowedSeedAge caps a configured MaxSeedAge so an absurd or overflowing value is
	// rejected with a clear config error rather than silently widening the window to meaningless.
	MaxAllowedSeedAge = 10 * 365 * 24 * time.Hour
)

// ParseMaxSeedAge validates and resolves a configured MaxSeedAge value. An empty value uses
// the default (180 days); a non-empty value must be a positive Go duration string no larger
// than MaxAllowedSeedAge, otherwise a config error is returned so callers fail closed rather
// than silently accepting an implausible window. It is used both at build time (create-usb
// manifest validation) and at runtime (the clock gate).
func ParseMaxSeedAge(configured string) (time.Duration, error) {
	if strings.TrimSpace(configured) == "" {
		return DefaultMaxSeedAge, nil
	}
	d, err := time.ParseDuration(configured)
	if err != nil {
		return 0, fmt.Errorf("maxSeedAge %q is not a valid duration: %w", configured, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("maxSeedAge %q must be positive", configured)
	}
	if d > MaxAllowedSeedAge {
		return 0, fmt.Errorf("maxSeedAge %q exceeds the maximum allowed (%s)", configured, MaxAllowedSeedAge)
	}
	return d, nil
}

// ChronyConfigHasTrustedSource reports whether a chrony configuration declares at least one
// genuine external or on-link time source that could actually step the clock -- a server,
// pool, or peer pointing at a real address, or a hardware reference clock (refclock). It
// deliberately excludes sources backed by the same unreliable RTC the clock gate exists to
// distrust: the NTP local-clock pseudo-address family (127.127.x.x), loopback servers, the
// `local stratum` fallback directive, and the RTC refclock driver. A config carrying only
// those is treated as having no trusted source, so the gate falls through to the RTC
// plausibility window instead of being bypassed by the very clock it is meant to check.
func ChronyConfigHasTrustedSource(chronyConfig string) bool {
	for _, raw := range strings.Split(chronyConfig, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || isChronyCommentLine(line) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToLower(fields[0]) {
		case "server", "pool", "peer":
			if len(fields) < 2 || isLocalClockAddress(fields[1]) {
				continue
			}
			return true
		case "refclock":
			// A non-RTC hardware reference clock (PHC/PPS/GPS) is a genuine on-link time
			// source. The RTC driver, however, is backed by the very same unreliable RTC the
			// gate exists to distrust: a wrong-but-stable RTC would satisfy `waitsync` and
			// bypass the seed window. Reject `refclock RTC ...` (and a driverless refclock,
			// which cannot be confirmed) so those configurations fall through to the window.
			if len(fields) < 2 || strings.EqualFold(fields[1], "RTC") {
				continue
			}
			return true
		}
		// `local stratum N`, `rtcsync`, `rtcfile`, and other non-source directives are ignored.
	}
	return false
}

// isChronyCommentLine reports whether a trimmed chrony config line is a comment. chrony treats
// lines beginning with '#', '%', '!', or ';' as comments.
func isChronyCommentLine(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case '#', '%', '!', ';':
		return true
	}
	return false
}

// isLocalClockAddress reports whether addr is the local/RTC-backed pseudo-source the gate must
// not trust: the NTP reference-clock pseudo-IP family (127.127.x.x) or a loopback address.
func isLocalClockAddress(addr string) bool {
	a := strings.ToLower(strings.TrimSpace(addr))
	if strings.HasPrefix(a, "127.127.") {
		return true
	}
	switch a {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
