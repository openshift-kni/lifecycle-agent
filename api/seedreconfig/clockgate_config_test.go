package seedreconfig

import (
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
		{name: "rtc refclock", config: "refclock RTC /dev/rtc0:utc", want: false},
		{name: "rtc refclock lowercase", config: "refclock rtc /dev/rtc0", want: false},
		{name: "driverless refclock", config: "refclock", want: false},
		{name: "local clock pseudo-ip", config: "server 127.127.1.0", want: false},
		{name: "loopback server", config: "server 127.0.0.1", want: false},
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
