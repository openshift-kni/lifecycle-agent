/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ipconfigcmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewIPConfigCmd(t *testing.T) {
	log := newTestLogger()

	cmd := NewIPConfigCmd(log)
	require.NotNil(t, cmd)

	assert.Equal(t, "ip-config", cmd.Use)
	assert.Same(t, log, pkgLog)

	tests := []struct {
		name string
		want string
	}{
		{name: "post-pivot subcommand", want: postPivotCmd},
		{name: "pre-pivot subcommand", want: prePivotCmd},
		{name: "rollback subcommand", want: rollbackCmd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, _, err := cmd.Find([]string{tt.want})
			require.NoError(t, err)
			require.NotNil(t, sub)
			assert.Equal(t, tt.want, sub.Use)
		})
	}
}

func TestNewIPConfigCmd_Flags(t *testing.T) {
	log := newTestLogger()

	cmd := NewIPConfigCmd(log)

	prePivot, _, err := cmd.Find([]string{prePivotCmd})
	require.NoError(t, err)
	for _, flag := range []string{
		newStaterootNameFlag,
		ipv4AddressFlag,
		ipv4MachineNetworkFlag,
		ipv6AddressFlag,
		ipv6MachineNetworkFlag,
		ipv4GatewayFlag,
		ipv6GatewayFlag,
		currentIPv4GatewayFlag,
		currentIPv6GatewayFlag,
		dnsServersFlag,
		vlanIDFlag,
		dnsIPFamilyFlag,
		installInitMonitorFlag,
		installIpConfigurationServiceFlag,
		pullSecretRefNameFlag,
	} {
		assert.NotNil(t, prePivot.Flags().Lookup(flag), "pre-pivot should define --%s", flag)
	}

	postPivot, _, err := cmd.Find([]string{postPivotCmd})
	require.NoError(t, err)
	assert.NotNil(t, postPivot.Flags().Lookup(recertImageFlag))

	rollback, _, err := cmd.Find([]string{rollbackCmd})
	require.NoError(t, err)
	stateroot := rollback.Flags().Lookup(staterootFlag)
	require.NotNil(t, stateroot)
	assert.Contains(t, stateroot.Annotations, cobra.BashCompOneRequiredFlag)
}
