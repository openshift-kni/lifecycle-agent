package seedreconfig

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseMaxSeedAge(t *testing.T) {
	testcases := []struct {
		name      string
		configure string
		want      time.Duration
		wantErr   bool
	}{
		{name: "empty uses default", configure: "", want: DefaultMaxSeedAge},
		{name: "whitespace uses default", configure: "   ", want: DefaultMaxSeedAge},
		{name: "valid duration", configure: "4320h", want: 4320 * time.Hour},
		{name: "not parseable", configure: "180d", wantErr: true},
		{name: "negative", configure: "-1h", wantErr: true},
		{name: "zero", configure: "0h", wantErr: true},
		{name: "overflow / absurd", configure: "200000h", wantErr: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMaxSeedAge(tc.configure)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestChronyConfigHasTrustedSource proves that only genuine external, peer, pool, or hardware
// refclock sources satisfy the trusted-source predicate.
func TestChronyConfigHasTrustedSource(t *testing.T) {
	testcases := []struct {
		name   string
		config string
		want   bool
	}{
		{name: "empty", config: "", want: false},
		{name: "server external", config: "server ntp.example.com iburst", want: true},
		{name: "pool external", config: "pool pool.ntp.org iburst", want: true},
		{name: "peer external", config: "peer 10.0.0.5", want: true},
		{name: "hardware refclock", config: "refclock PHC /dev/ptp0 poll 0", want: true},
		{name: "PPS-only refclock", config: "refclock PPS /dev/pps0 lock NMEA", want: false},
		{name: "two-field refclock", config: "refclock PHC", want: false},
		{name: "include with default refclock", config: "include /etc/chrony.d/*.conf\nrefclock PHC /dev/ptp0", want: false},
		{name: "confdir with default refclock", config: "confdir /etc/chrony.d\nrefclock PHC /dev/ptp0", want: false},
		{name: "include with explicit refclock refid", config: "include /etc/chrony.d/*.conf\nrefclock PHC /dev/ptp0 refid PTP", want: true},
		{name: "PHC external PPS mode", config: "refclock PHC /dev/ptp2:extpps:pin=1 width 0.2 poll 2\nlocal stratum 10", want: false},
		{name: "PHC generic PPS option", config: "refclock PHC /dev/ptp1 poll 3 pps\nlocal stratum 10", want: false},
		{name: "unlocked SOCK refclock", config: "refclock SOCK /var/run/chrony.ttyS0.sock\nlocal stratum 10", want: false},
		{name: "local-standard refclock", config: "refclock PHC /dev/ptp0 local\nlocal stratum 10", want: false},
		{name: "locked SOCK with missing target", config: "refclock SOCK /var/run/chrony.ttyS0.sock lock NMEA", want: false},
		{name: "locked SOCK with RTC target", config: "refclock RTC /dev/rtc0 refid BAD\nrefclock SOCK /var/run/chrony.ttyS0.sock lock BAD", want: false},
		{name: "locked SOCK with trusted PHC target", config: "refclock PHC /dev/ptp0 refid GOOD\nrefclock SOCK /var/run/chrony.ttyS0.sock lock GOOD", want: true},
		{name: "locked SOCK with SOCK target", config: "refclock SOCK /var/run/chrony.full.sock refid GOOD\nrefclock SOCK /var/run/chrony.ttyS0.sock lock GOOD", want: false},
		{name: "rtc refclock", config: "refclock RTC /dev/rtc0:utc", want: false},
		{name: "rtc refclock lowercase", config: "refclock rtc /dev/rtc0", want: false},
		{name: "driverless refclock", config: "refclock", want: false},
		{name: "local clock pseudo-ip", config: "server 127.127.1.0", want: false},
		{name: "IPv4 loopback", config: "server 127.0.0.1", want: false},
		{name: "alternate IPv4 loopback", config: "server 127.0.0.2", want: false},
		{name: "IPv6 loopback", config: "server ::1", want: false},
		{name: "localhost", config: "server localhost", want: false},
		{name: "local stratum fallback", config: "local stratum 10", want: false},
		{name: "rtcsync only", config: "rtcsync", want: false},
		{name: "comments ignored", config: "# server ntp.example.com\n! pool bad\nrtcsync", want: false},
		{name: "mixed local and real", config: "server 127.127.1.0\nserver ntp.example.com", want: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ChronyConfigHasTrustedSource(tc.config))
		})
	}
}

// TestChronyRTCRefclockRefIDs proves that RTC refclock identifiers, including explicit IDs
// that may collide with trusted refclocks, are retained for rejection.
func TestChronyRTCRefclockRefIDs(t *testing.T) {
	got := ChronyRTCRefclockRefIDs("refclock PHC /dev/ptp0\nrefclock RTC /dev/rtc0\nrefclock RTC /dev/rtc1 refid PHC1\n")
	assert.Equal(t, []ChronyRefclockRefID{
		{Value: "RTC1"},
		{Value: "PHC1", ExplicitRefID: true},
	}, got)

	got = ChronyRTCRefclockRefIDs("include /etc/chrony.d/*.conf\nrefclock RTC /dev/rtc0\nrefclock RTC /dev/rtc1 refid PHC1\n")
	assert.Equal(t, []ChronyRefclockRefID{
		{Value: "PHC1", ExplicitRefID: true},
	}, got)
}

// TestChronyRefclockRuntimeValueIndexBoundaries proves that default refclock IDs follow
// Chrony's global-index encoding at the transition from single-digit to two-digit indices.
func TestChronyRefclockRuntimeValueIndexBoundaries(t *testing.T) {
	fields := []string{"refclock", "PHC", "/dev/ptp0"}
	testcases := []struct {
		index int
		want  string
	}{
		{index: 9, want: "PHC9"},
		{index: 10, want: "PH10"},
		{index: 11, want: "PH11"},
	}
	for _, tc := range testcases {
		t.Run(fmt.Sprintf("index-%d", tc.index), func(t *testing.T) {
			got, explicit := chronyRefclockRuntimeValue(fields, tc.index)
			assert.False(t, explicit)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestChronyTrustedSourcesWithIncludes proves that default refclock IDs are not trusted when
// included configuration files may change Chrony's global refclock numbering, while explicit
// refids remain usable.
func TestChronyTrustedSourcesWithIncludes(t *testing.T) {
	got := ChronyTrustedSources("include /etc/chrony.d/*.conf\nrefclock PHC /dev/ptp0\nrefclock PHC /dev/ptp1 refid PTP\n")
	assert.Equal(t, []ChronyTrustedSource{
		{Kind: "refclock", Value: "PTP", ExplicitRefID: true},
	}, got)
}

// TestChronyTrustedSources proves that trusted-source extraction preserves configured
// network names, custom refclock refids, and chrony's three-character default refclock IDs.
func TestChronyTrustedSources(t *testing.T) {
	got := ChronyTrustedSources("server ntp.example.com iburst\npool pool.example.com\npeer 192.0.2.10\nrefclock PHC /dev/ptp0\nrefclock SOCK /var/run/chrony/chrony.sock\nrefclock SOCK /var/run/chrony.locked.sock lock NMEA\nrefclock PHC /dev/ptp3 refid NMEA\nrefclock PPS /dev/pps0 lock NMEA\nrefclock PHC /dev/ptp1 refid PTP\nrefclock PHC /dev/ptp2 refid RTC\nrefclock RTC /dev/rtc0\nserver 127.0.0.2\nlocal stratum 10\n")
	assert.Equal(t, []ChronyTrustedSource{
		{Kind: "server", Value: "ntp.example.com"},
		{Kind: "pool", Value: "pool.example.com"},
		{Kind: "peer", Value: "192.0.2.10"},
		{Kind: "refclock", Value: "PHC0"},
		{Kind: "refclock", Value: "SOC2"},
		{Kind: "refclock", Value: "NMEA", ExplicitRefID: true},
		{Kind: "refclock", Value: "PTP", ExplicitRefID: true},
		{Kind: "refclock", Value: "RTC", ExplicitRefID: true},
	}, got)
}
