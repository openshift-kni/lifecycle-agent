package utils

import (
	"testing"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/stretchr/testify/assert"
)

func TestValidateIPFamilyConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		family      string
		addr        string
		networkCIDR string
		gateway     string
		wantErr     bool
		errContains string
	}{
		{
			name:   "all empty",
			family: common.IPv4FamilyName,
		},
		{
			name:        "valid ipv4 full",
			family:      common.IPv4FamilyName,
			addr:        "192.168.1.10",
			networkCIDR: "192.168.1.0/24",
			gateway:     "192.168.1.1",
		},
		{
			name:        "valid ipv4 no gateway",
			family:      common.IPv4FamilyName,
			addr:        "10.0.0.5",
			networkCIDR: "10.0.0.0/8",
		},
		{
			name:        "invalid ipv4 addr",
			family:      common.IPv4FamilyName,
			addr:        "not-an-ip",
			wantErr:     true,
			errContains: "invalid IPV4 address",
		},
		{
			name:        "ipv6 addr for ipv4 family",
			family:      common.IPv4FamilyName,
			addr:        "2001:db8::1",
			wantErr:     true,
			errContains: "invalid IPV4 address",
		},
		{
			name:        "invalid ipv4 cidr",
			family:      common.IPv4FamilyName,
			networkCIDR: "not-a-cidr",
			wantErr:     true,
			errContains: "invalid ipv4 machine network CIDR",
		},
		{
			name:        "ipv6 cidr for ipv4 family",
			family:      common.IPv4FamilyName,
			networkCIDR: "2001:db8::/64",
			wantErr:     true,
			errContains: "invalid ipv4 machine network CIDR",
		},
		{
			name:        "invalid ipv4 gateway",
			family:      common.IPv4FamilyName,
			gateway:     "bad-gw",
			wantErr:     true,
			errContains: "invalid IPV4 gateway",
		},
		{
			name:        "ipv4 gateway outside cidr",
			family:      common.IPv4FamilyName,
			networkCIDR: "10.0.0.0/24",
			gateway:     "192.168.1.1",
			wantErr:     true,
			errContains: "not within machine network",
		},
		{
			name:        "ipv4 gateway inside cidr",
			family:      common.IPv4FamilyName,
			networkCIDR: "10.0.0.0/24",
			gateway:     "10.0.0.1",
		},
		{
			name:        "valid ipv6 full",
			family:      common.IPv6FamilyName,
			addr:        "2001:db8::10",
			networkCIDR: "2001:db8::/64",
			gateway:     "fe80::1",
		},
		{
			name:        "ipv4 addr for ipv6 family",
			family:      common.IPv6FamilyName,
			addr:        "192.168.1.1",
			wantErr:     true,
			errContains: "invalid IPV6 address",
		},
		{
			name:        "ipv4 cidr for ipv6 family",
			family:      common.IPv6FamilyName,
			networkCIDR: "10.0.0.0/8",
			wantErr:     true,
			errContains: "invalid ipv6 machine network CIDR",
		},
		{
			// IPv6 gateways may be link-local and outside the machine network CIDR — no error expected
			name:        "ipv6 gateway outside cidr is allowed",
			family:      common.IPv6FamilyName,
			networkCIDR: "2001:db8::/64",
			gateway:     "fe80::1",
		},
		{
			name:        "unsupported family with addr",
			family:      "ipv5",
			addr:        "192.168.1.1",
			wantErr:     true,
			errContains: "unsupported IP family",
		},
		{
			name:        "unsupported family with cidr",
			family:      "ipv5",
			networkCIDR: "10.0.0.0/8",
			wantErr:     true,
			errContains: "unsupported IP family",
		},
		{
			name:        "ipv6 addr as ipv4 gateway",
			family:      common.IPv4FamilyName,
			gateway:     "2001:db8::1",
			wantErr:     true,
			errContains: "invalid IPV4 gateway",
		},
		{
			name:        "ipv4 addr as ipv6 gateway",
			family:      common.IPv6FamilyName,
			gateway:     "192.168.1.1",
			wantErr:     true,
			errContains: "invalid IPV6 gateway",
		},
		{
			name:        "unsupported family with gateway only",
			family:      "ipv5",
			gateway:     "192.168.1.1",
			wantErr:     true,
			errContains: "unsupported IP family",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateIPFamilyConfig(tc.family, tc.addr, tc.networkCIDR, tc.gateway)
			if tc.wantErr {
				assert.Error(t, err)
				if tc.errContains != "" {
					assert.ErrorContains(t, err, tc.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
