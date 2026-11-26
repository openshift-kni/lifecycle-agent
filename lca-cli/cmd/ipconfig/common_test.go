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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubKubeconfigPath points the client builder at path, restoring the previous
// value when the test finishes.
func stubKubeconfigPath(t *testing.T, path string) {
	t.Helper()

	old := kubeconfigPath
	kubeconfigPath = path
	t.Cleanup(func() { kubeconfigPath = old })
}

// stubMissingKubeconfig points the client builder at a path that does not exist
// so the kubeconfig load fails deterministically, regardless of the environment.
func stubMissingKubeconfig(t *testing.T) {
	t.Helper()

	stubKubeconfigPath(t, filepath.Join(t.TempDir(), "missing.kubeconfig"))
}

// stubParsableKubeconfig points the client builder at a syntactically valid
// kubeconfig whose server is never contacted: controller-runtime builds a lazy
// REST mapper, so client.New does not dial.
func stubParsableKubeconfig(t *testing.T) {
	t.Helper()

	const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
    insecure-skip-tls-verify: true
  name: test
contexts:
- context:
    cluster: test
    user: test
  name: test
current-context: test
users:
- name: test
  user:
    token: dummy
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(kubeconfig), 0o600))

	stubKubeconfigPath(t, path)
}

func TestBuildOpsAndK8sClient_KubeconfigError(t *testing.T) {
	setUpPrePivotTest(t)
	stubMissingKubeconfig(t)

	opsInterface, executor, client, err := buildOpsAndK8sClient(ipPrePivotScheme)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create k8s config")
	assert.Nil(t, opsInterface)
	assert.Nil(t, executor)
	assert.Nil(t, client)
}

func TestBuildOpsAndK8sClient_Success(t *testing.T) {
	setUpPrePivotTest(t)
	stubParsableKubeconfig(t)

	opsInterface, executor, client, err := buildOpsAndK8sClient(ipPrePivotScheme)
	require.NoError(t, err)
	assert.NotNil(t, opsInterface)
	assert.NotNil(t, executor)
	assert.NotNil(t, client)
}

func TestRunIPConfigPrePivot_ClientError(t *testing.T) {
	setUpPrePivotTest(t)
	stubMissingKubeconfig(t)

	err := runIPConfigPrePivot()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create k8s config")
}

// TestRunIPConfigPrePivot_ValidationError drives the flow far enough to reach
// flag validation and no further: validation runs before any host command, so
// no executor is invoked.
func TestRunIPConfigPrePivot_ValidationError(t *testing.T) {
	setUpPrePivotTest(t)
	stubParsableKubeconfig(t)

	err := runIPConfigPrePivot()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ip config pre-pivot failed")
	assert.Contains(t, err.Error(), "new-stateroot-name is required")
}

func TestRunIPConfigPostPivot_ClientError(t *testing.T) {
	setUpPrePivotTest(t)
	stubMissingKubeconfig(t)

	err := runIPConfigPostPivot()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create k8s config")
}

// TestRunIPConfigPrePivot_FlagsFile covers the controller-written config file
// that replaces the command-line flags. Each case stops at flag validation, so
// no host command is ever executed.
func TestRunIPConfigPrePivot_FlagsFile(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{
			name:     "config file replaces the flags",
			contents: `{"new-stateroot-name":"new-root"}`,
			wantErr:  "at least one of IPv4 or IPv6 must be provided",
		},
		{
			name:     "malformed config file is ignored",
			contents: `{not json`,
			wantErr:  "new-stateroot-name is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setUpPrePivotTest(t)
			stubParsableKubeconfig(t)

			path := filepath.Join(t.TempDir(), "ip-config-pre-pivot.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.contents), 0o600))
			stubPrePivotFlagsFile(t, path)

			err := runIPConfigPrePivot()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
