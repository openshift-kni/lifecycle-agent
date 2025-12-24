package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateNMState_IPv4Only(t *testing.T) {
	nm, err := GenerateNMState(
		"eth0",
		[]string{"192.0.2.10"},
		[]string{"192.0.2.0/24"},
		"192.0.2.1",
		"",
		"8.8.8.8",
		"",
		0,
		"",
		"",
	)
	assert.NoError(t, err)
	assert.Contains(t, nm, "192.0.2.10")
	assert.Contains(t, nm, "192.0.2.1")
	assert.Contains(t, nm, "8.8.8.8")
	assert.Contains(t, nm, "next-hop-interface: eth0")
	assert.NotContains(t, nm, "br-ex")
}

func TestGenerateNMState_IPv6Only(t *testing.T) {
	nm, err := GenerateNMState(
		"eth0",
		[]string{"2001:db8::10"},
		[]string{"2001:db8::/64"},
		"",
		"2001:db8::1",
		"",
		"2001:4860:4860::8888",
		100,
		"",
		"",
	)
	assert.NoError(t, err)
	assert.Contains(t, nm, "2001:db8::10")
	assert.Contains(t, nm, "2001:db8::1")
	assert.Contains(t, nm, "2001:4860:4860::8888")
	assert.Contains(t, nm, "autoconf: false")
	assert.NotContains(t, nm, "br-ex")
}

func TestGenerateNMState_DualStack(t *testing.T) {
	nm, err := GenerateNMState(
		"eth0",
		[]string{"192.0.2.10", "2001:db8::10"},
		[]string{"192.0.2.0/24", "2001:db8::/64"},
		"192.0.2.1",
		"2001:db8::1",
		"8.8.8.8",
		"2001:4860:4860::8888",
		0,
		"",
		"",
	)
	assert.NoError(t, err)
	assert.Contains(t, nm, "192.0.2.10")
	assert.Contains(t, nm, "2001:db8::10")
	assert.Contains(t, nm, "autoconf: false")
	assert.NotContains(t, nm, "br-ex")
}

func TestGenerateNMState_Errors(t *testing.T) {
	_, err := GenerateNMState(
		"eth0",
		[]string{},
		[]string{},
		"",
		"",
		"",
		"",
		0,
		"",
		"",
	)
	assert.Error(t, err)

	_, err = GenerateNMState(
		"eth0",
		[]string{"192.0.2.10"},
		[]string{"192.0.2.0/24", "10.0.0.0/8"},
		"",
		"",
		"",
		"",
		0,
		"",
		"",
	)
	assert.Error(t, err)

	_, err = GenerateNMState(
		"eth0",
		[]string{"192.0.2.10"},
		[]string{"not-a-cidr"},
		"",
		"",
		"",
		"",
		0,
		"",
		"",
	)
	assert.Error(t, err)
}
