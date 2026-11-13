package seedreconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParseReconfigureConvergenceTimeout(t *testing.T) {
	testcases := []struct {
		name      string
		configure string
		want      time.Duration
		wantErr   bool
	}{
		{name: "empty uses default", configure: "", want: DefaultReconfigureConvergenceTimeout},
		{name: "whitespace uses default", configure: "   ", want: DefaultReconfigureConvergenceTimeout},
		{name: "valid minutes", configure: "90m", want: 90 * time.Minute},
		{name: "valid hours", configure: "2h", want: 2 * time.Hour},
		{name: "at cap", configure: "24h", want: MaxAllowedReconfigureConvergenceTimeout},
		{name: "not parseable", configure: "60min", wantErr: true},
		{name: "negative", configure: "-5m", wantErr: true},
		{name: "zero", configure: "0m", wantErr: true},
		{name: "over cap", configure: "48h", wantErr: true},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReconfigureConvergenceTimeout(tc.configure)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
