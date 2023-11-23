package utils

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestIsIpv6(t *testing.T) {
	testcases := []struct {
		name         string
		ip           string
		expected     bool
		validateFunc func(t *testing.T)
	}{
		{
			name:     "ipv6 - true",
			ip:       "2620:52:0:198::10",
			expected: true,
			validateFunc: func(t *testing.T) {
			},
		},
		{
			name:     "ipv6 - false",
			ip:       "192,168.127.10",
			expected: false,
			validateFunc: func(t *testing.T) {
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, IsIpv6(tc.ip), tc.expected)
		})
	}
}
