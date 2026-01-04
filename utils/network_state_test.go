package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNmstate_Success(t *testing.T) {
	jsonInput := `{
		"interfaces": [
			{
				"name": "br-ex",
				"type": "ovs-bridge",
				"ipv4": { "enabled": true, "address": [ { "ip": "192.0.2.10", "prefix-length": 24 } ] },
				"ipv6": { "enabled": false, "address": [] }
			}
		],
		"routes": { "running": [], "config": [] },
		"dns-resolver": {
			"running": { "server": ["1.1.1.1"] },
			"config": { "server": [] }
		}
	}`

	state, err := ParseNmstate(jsonInput)
	assert.NoError(t, err)
	assert.Len(t, state.Interfaces, 1)
	assert.Equal(t, "br-ex", state.Interfaces[0].Name)
}

func TestParseNmstate_InvalidJSON(t *testing.T) {
	_, err := ParseNmstate("{invalid json")
	assert.Error(t, err)
}

func TestExtractDNSServers_ReturnsRunningOnly(t *testing.T) {
	state := NmState{
		DNSResolver: NmDNS{
			Running: NmDNSList{
				Server: []string{"1.1.1.1", "2001:db8::1"},
			},
			Config: NmDNSList{
				Server: []string{"8.8.8.8", "2001:db8::2"},
			},
		},
	}

	got := ExtractDNSServers(state)
	assert.Equal(t, []string{"1.1.1.1", "2001:db8::1"}, got)

	state.DNSResolver.Running.Server = nil
	got = ExtractDNSServers(state)
	assert.Empty(t, got)
}

func TestFindDefaultGateways(t *testing.T) {
	state := NmState{
		Routes: NmRoutes{
			Running: []NmRoute{
				{Destination: "0.0.0.0/0", NextHopAddress: "192.0.2.1", NextHopInterface: "br-ex"},
				{Destination: "::/0", NextHopAddress: "2001:db8::1", NextHopInterface: "br-ex"},
			},
			Config: []NmRoute{
				{Destination: "0.0.0.0/0", NextHopAddress: "198.51.100.1", NextHopInterface: "other"},
			},
		},
	}

	gw4, gw6 := FindDefaultGateways(state, "br-ex", "0.0.0.0/0", "::/0")
	assert.Equal(t, "192.0.2.1", gw4)
	assert.Equal(t, "2001:db8::1", gw6)
}

func TestExtractBrExUplinkName_Success(t *testing.T) {
	state := NmState{
		Interfaces: []NmIf{
			{
				Name: "br-ex",
				Type: "ovs-bridge",
				Bridge: NmBridge{
					Port: []struct {
						Name string `json:"name"`
					}{
						{Name: "br-ex"},
						{Name: "ens3"},
					},
				},
			},
		},
	}

	uplink, err := ExtractBrExUplinkName(state, "br-ex")
	assert.NoError(t, err)
	assert.Equal(t, "ens3", uplink)
}

func TestExtractBrExUplinkName_NotFound(t *testing.T) {
	state := NmState{}
	_, err := ExtractBrExUplinkName(state, "br-ex")
	assert.Error(t, err)
}

func TestExtractBrExVLANID_Found(t *testing.T) {
	state := NmState{
		Interfaces: []NmIf{
			{
				Name: "br-ex",
				Type: "ovs-bridge",
				Bridge: NmBridge{
					Port: []struct {
						Name string `json:"name"`
					}{
						{Name: "br-ex"},
						{Name: "vlan123"},
					},
				},
			},
			{
				Name: "vlan123",
				Type: "vlan",
				VLAN: &NmVLAN{
					BaseIface: "ens3",
					ID:        123,
				},
			},
		},
	}

	id, err := ExtractBrExVLANID(state, "br-ex")
	assert.NoError(t, err)
	if assert.NotNil(t, id) {
		assert.Equal(t, 123, *id)
	}
}

func TestExtractBrExVLANID_NoVLAN(t *testing.T) {
	state := NmState{
		Interfaces: []NmIf{
			{
				Name: "br-ex",
				Type: "ovs-bridge",
				Bridge: NmBridge{
					Port: []struct {
						Name string `json:"name"`
					}{
						{Name: "br-ex"},
						{Name: "ens3"},
					},
				},
			},
			{
				Name: "ens3",
				Type: "ethernet",
			},
		},
	}

	id, err := ExtractBrExVLANID(state, "br-ex")
	assert.NoError(t, err)
	assert.Nil(t, id)
}

func TestFindMatchingCIDR(t *testing.T) {
	cidrs := []string{
		"192.0.2.0/24",
		"10.0.0.0/8",
		"2001:db8::/64",
		"not-a-cidr",
	}

	assert.Equal(t, "192.0.2.0/24", FindMatchingCIDR("192.0.2.10", cidrs))
	assert.Equal(t, "2001:db8::/64", FindMatchingCIDR("2001:db8::10", cidrs))
	// Family match does not require containment
	assert.Equal(t, "10.0.0.0/8", FindMatchingCIDR("203.0.113.10", []string{"10.0.0.0/8"}))
	// Mismatched family should return empty string
	assert.Equal(t, "", FindMatchingCIDR("192.0.2.10", []string{"2001:db8::/64"}))
	// Invalid IP should return empty string
	assert.Equal(t, "", FindMatchingCIDR("not-an-ip", cidrs))
}
