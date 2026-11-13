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

package createusb

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/openshift-kni/lifecycle-agent/api/seedreconfig"
)

// fakeCredentialGenerator is a test double for CredentialGenerator: it records the inputs it was
// called with and returns canned results, so the generate-if-absent orchestration is tested
// without the openshift-install binary.
type fakeCredentialGenerator struct {
	called    bool
	gotInputs CredentialInputs
	result    *GeneratedCredentials
	err       error
}

func (f *fakeCredentialGenerator) Generate(_ context.Context, _ *logrus.Logger, in CredentialInputs) (*GeneratedCredentials, error) {
	f.called = true
	f.gotInputs = in
	return f.result, f.err
}

// discardLogger returns a logger whose output is discarded (used when the test does not inspect
// the log).
func discardLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// generatedCreds returns a valid generated bundle: the shared valid hash + crypto retention so a
// post-generation Validate() passes, plus distinctive secret bytes for hygiene assertions.
func generatedCreds() *GeneratedCredentials {
	return &GeneratedCredentials{
		KubeadminPasswordHash:     validKubeadminPasswordHash,
		KubeconfigCryptoRetention: validKubeconfigCryptoRetention(),
		Kubeconfig:                []byte("SECRET-KUBECONFIG-CONTENT"),
		KubeadminPassword:         []byte("SECRET-PASSWORD-VALUE"),
	}
}

// selfContainedWithoutCredentials returns a valid self-contained manifest with the credential
// fields cleared, i.e. one that needs generation.
func selfContainedWithoutCredentials() *Manifest {
	m := validSelfContained()
	m.SiteConfig.KubeadminPasswordHash = ""
	m.SiteConfig.KubeconfigCryptoRetention = seedreconfig.KubeConfigCryptoRetention{}
	return m
}

func TestNeedsCredentialGeneration(t *testing.T) {
	t.Run("self-contained without credentials needs generation", func(t *testing.T) {
		assert.True(t, NeedsCredentialGeneration(selfContainedWithoutCredentials()))
	})
	t.Run("self-contained with full credentials does not", func(t *testing.T) {
		assert.False(t, NeedsCredentialGeneration(validSelfContained()))
	})
	t.Run("self-contained with only the hash does not (partial, rejected elsewhere)", func(t *testing.T) {
		m := selfContainedWithoutCredentials()
		m.SiteConfig.KubeadminPasswordHash = validKubeadminPasswordHash
		assert.False(t, NeedsCredentialGeneration(m))
	})
	t.Run("self-contained with only retention does not", func(t *testing.T) {
		m := selfContainedWithoutCredentials()
		m.SiteConfig.KubeconfigCryptoRetention = validKubeconfigCryptoRetention()
		assert.False(t, NeedsCredentialGeneration(m))
	})
	t.Run("install-only never needs generation", func(t *testing.T) {
		assert.False(t, NeedsCredentialGeneration(validInstallOnly()))
	})
	t.Run("nil siteConfig never needs generation", func(t *testing.T) {
		m := validSelfContained()
		m.SiteConfig = nil
		assert.False(t, NeedsCredentialGeneration(m))
	})
}

func TestGenerateSiteConfigCredentials_GeneratesWhenAbsent(t *testing.T) {
	m := selfContainedWithoutCredentials()
	m.SiteConfig.MachineNetworks = []string{"192.0.2.0/24"}
	m.SiteConfig.RawNMStateConfig = "interfaces: []"
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	creds, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.True(t, fake.called)

	// The generated material is populated into the manifest so the existing render + validators
	// consume it exactly like an operator-supplied override.
	assert.Equal(t, validKubeadminPasswordHash, m.SiteConfig.KubeadminPasswordHash)
	assert.Equal(t, validKubeconfigCryptoRetention(), m.SiteConfig.KubeconfigCryptoRetention)

	// The generator receives the siteConfig-derived inputs.
	assert.Equal(t, "example", fake.gotInputs.ClusterName)
	assert.Equal(t, "example.com", fake.gotInputs.BaseDomain)
	assert.Equal(t, "node0", fake.gotInputs.Hostname)
	assert.Equal(t, []string{"192.0.2.0/24"}, fake.gotInputs.MachineNetworks)
	assert.Equal(t, "interfaces: []", fake.gotInputs.RawNMStateConfig)
	assert.Equal(t, validPullSecret, fake.gotInputs.PullSecret)

	// The populated manifest now passes full validation.
	assert.NoError(t, m.Validate())
}

// TestGenerateSiteConfigCredentials_PassesSSHKey confirms the optional sshKey flows to the
// generator (checked separately so the fixture need not carry a validation-passing key).
func TestGenerateSiteConfigCredentials_PassesSSHKey(t *testing.T) {
	m := selfContainedWithoutCredentials()
	m.SiteConfig.SSHKey = "ssh-ed25519 AAAAexample operator@host"
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.NoError(t, err)
	assert.Equal(t, "ssh-ed25519 AAAAexample operator@host", fake.gotInputs.SSHKey)
}

func TestGenerateSiteConfigCredentials_SkipsWhenSupplied(t *testing.T) {
	m := validSelfContained()
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	creds, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.NoError(t, err)
	assert.Nil(t, creds)
	assert.False(t, fake.called, "generator must not run when credentials are supplied")
}

func TestGenerateSiteConfigCredentials_RejectsPartial(t *testing.T) {
	t.Run("only hash supplied", func(t *testing.T) {
		m := selfContainedWithoutCredentials()
		m.SiteConfig.KubeadminPasswordHash = validKubeadminPasswordHash
		fake := &fakeCredentialGenerator{result: generatedCreds()}

		_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be supplied together")
		assert.False(t, fake.called)
	})
	t.Run("only retention supplied", func(t *testing.T) {
		m := selfContainedWithoutCredentials()
		m.SiteConfig.KubeconfigCryptoRetention = validKubeconfigCryptoRetention()
		fake := &fakeCredentialGenerator{result: generatedCreds()}

		_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be supplied together")
		assert.False(t, fake.called)
	})
}

func TestGenerateSiteConfigCredentials_InstallOnly(t *testing.T) {
	m := validInstallOnly()
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	creds, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.NoError(t, err)
	assert.Nil(t, creds)
	assert.False(t, fake.called)
}

func TestGenerateSiteConfigCredentials_NilGenerator(t *testing.T) {
	m := selfContainedWithoutCredentials()

	_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credential generator is configured")
}

func TestGenerateSiteConfigCredentials_ResolvesPullSecretFileReference(t *testing.T) {
	psPath := filepath.Join(t.TempDir(), "pull-secret.json")
	require.NoError(t, os.WriteFile(psPath, []byte(validPullSecret), 0o600))

	m := selfContainedWithoutCredentials()
	m.SiteConfig.PullSecret = pullSecretFilePrefix + psPath
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.NoError(t, err)
	assert.Equal(t, validPullSecret, fake.gotInputs.PullSecret, "generator must receive the resolved pull secret")
}

func TestGenerateSiteConfigCredentials_GeneratorError(t *testing.T) {
	m := selfContainedWithoutCredentials()
	fake := &fakeCredentialGenerator{err: assert.AnError}

	_, err := GenerateSiteConfigCredentials(context.Background(), discardLogger(), m, fake)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generating kubeconfig identity")
}

// TestGenerateSiteConfigCredentials_DoesNotLogSecrets asserts the orchestration never writes the
// secret kubeconfig or kubeadmin password to the log.
func TestGenerateSiteConfigCredentials_DoesNotLogSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := logrus.New()
	log.SetOutput(&buf)

	m := selfContainedWithoutCredentials()
	fake := &fakeCredentialGenerator{result: generatedCreds()}

	_, err := GenerateSiteConfigCredentials(context.Background(), log, m, fake)
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "SECRET-KUBECONFIG-CONTENT")
	assert.NotContains(t, buf.String(), "SECRET-PASSWORD-VALUE")
}

func TestWriteCredentialArtifacts(t *testing.T) {
	t.Run("writes owner-only artifacts", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		creds := generatedCreds()

		require.NoError(t, WriteCredentialArtifacts(discardLogger(), dir, creds))

		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, credentialDirMode, info.Mode().Perm())

		kubeconfig := filepath.Join(dir, KubeconfigArtifactName)
		password := filepath.Join(dir, KubeadminPasswordArtifactName)

		data, err := os.ReadFile(kubeconfig)
		require.NoError(t, err)
		assert.Equal(t, creds.Kubeconfig, data)
		data, err = os.ReadFile(password)
		require.NoError(t, err)
		assert.Equal(t, creds.KubeadminPassword, data)

		for _, p := range []string{kubeconfig, password} {
			info, err := os.Stat(p)
			require.NoError(t, err)
			assert.Equal(t, credentialFileMode, info.Mode().Perm(), p)
		}
	})

	t.Run("nil credentials is a no-op", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "creds")
		require.NoError(t, WriteCredentialArtifacts(discardLogger(), dir, nil))
		_, err := os.Stat(dir)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("overwrite is surfaced loudly", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, KubeconfigArtifactName), []byte("old"), 0o600))

		var buf bytes.Buffer
		log := logrus.New()
		log.SetOutput(&buf)

		require.NoError(t, WriteCredentialArtifacts(log, dir, generatedCreds()))
		assert.Contains(t, buf.String(), "overwriting existing credential artifact")

		data, err := os.ReadFile(filepath.Join(dir, KubeconfigArtifactName))
		require.NoError(t, err)
		assert.Equal(t, []byte("SECRET-KUBECONFIG-CONTENT"), data)
	})
}

func TestRenderInstallConfig(t *testing.T) {
	in := CredentialInputs{
		ClusterName:     "example",
		BaseDomain:      "example.com",
		SSHKey:          "ssh-ed25519 AAAAexample",
		PullSecret:      validPullSecret,
		MachineNetworks: []string{"192.0.2.0/24", "2001:db8::/64"},
	}
	data, err := renderInstallConfig(in)
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))

	assert.Equal(t, installConfigVersion, doc["apiVersion"])
	assert.Equal(t, "example.com", doc["baseDomain"])
	assert.Equal(t, "example", doc["metadata"].(map[string]interface{})["name"])
	assert.Equal(t, validPullSecret, doc["pullSecret"])
	assert.Equal(t, "ssh-ed25519 AAAAexample", doc["sshKey"])

	networking := doc["networking"].(map[string]interface{})
	assert.Equal(t, ovnNetworkType, networking["networkType"])
	networks := networking["machineNetwork"].([]interface{})
	require.Len(t, networks, 2)
	assert.Equal(t, "192.0.2.0/24", networks[0].(map[string]interface{})["cidr"])
	assert.Equal(t, "2001:db8::/64", networks[1].(map[string]interface{})["cidr"])

	// SNO topology: one control-plane replica, zero compute, platform none.
	assert.EqualValues(t, 1, doc["controlPlane"].(map[string]interface{})["replicas"])
	compute := doc["compute"].([]interface{})
	require.Len(t, compute, 1)
	assert.EqualValues(t, 0, compute[0].(map[string]interface{})["replicas"])
	_, hasNone := doc["platform"].(map[string]interface{})["none"]
	assert.True(t, hasNone, "platform.none must be present")
}

func TestRenderInstallConfig_OmitsEmptyOptionalFields(t *testing.T) {
	data, err := renderInstallConfig(CredentialInputs{ClusterName: "c", BaseDomain: "d", PullSecret: validPullSecret})
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	_, hasSSH := doc["sshKey"]
	assert.False(t, hasSSH, "empty sshKey must be omitted")
	_, hasNetworks := doc["networking"].(map[string]interface{})["machineNetwork"]
	assert.False(t, hasNetworks, "empty machineNetwork must be omitted")
}

func TestRenderImageBasedConfig(t *testing.T) {
	in := CredentialInputs{
		ClusterName:      "example",
		Hostname:         "node0",
		RawNMStateConfig: "interfaces:\n- name: eth0\n  type: ethernet\n",
	}
	data, err := renderImageBasedConfig(in)
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	assert.Equal(t, imageBasedConfigVersion, doc["apiVersion"])
	assert.Equal(t, imageBasedConfigKind, doc["kind"])
	assert.Equal(t, "example", doc["metadata"].(map[string]interface{})["name"])
	assert.Equal(t, "node0", doc["hostname"])

	// networkConfig is embedded as a nested mapping, not a YAML string.
	nc, ok := doc["networkConfig"].(map[string]interface{})
	require.True(t, ok, "networkConfig must be a nested mapping")
	interfaces := nc["interfaces"].([]interface{})
	require.Len(t, interfaces, 1)
	assert.Equal(t, "eth0", interfaces[0].(map[string]interface{})["name"])
}

func TestRenderImageBasedConfig_OmitsEmptyNetworkConfig(t *testing.T) {
	data, err := renderImageBasedConfig(CredentialInputs{ClusterName: "c", Hostname: "h"})
	require.NoError(t, err)
	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	_, has := doc["networkConfig"]
	assert.False(t, has, "empty networkConfig must be omitted")
}

func TestRenderImageBasedConfig_InvalidNMState(t *testing.T) {
	_, err := renderImageBasedConfig(CredentialInputs{ClusterName: "c", RawNMStateConfig: "\tnot: [valid"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rawNMStateConfig")
}

// writeGeneratedManifest marshals a SeedReconfiguration carrying the given hash + retention to a
// JSON file at <dir>/cluster-configuration/manifest.json, mirroring the installer's output.
func writeGeneratedManifest(t *testing.T, dir, hash string, retention seedreconfig.KubeConfigCryptoRetention) {
	t.Helper()
	sr := seedreconfig.SeedReconfiguration{
		APIVersion:                seedreconfig.SeedReconfigurationVersion,
		KubeadminPasswordHash:     hash,
		KubeconfigCryptoRetention: retention,
	}
	data, err := json.Marshal(sr) // the installer writes manifest.json as JSON
	require.NoError(t, err)
	ccDir := filepath.Join(dir, "cluster-configuration")
	require.NoError(t, os.MkdirAll(ccDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ccDir, "manifest.json"), data, 0o600))
}

func TestReadGeneratedCredentials_FromWorkDir(t *testing.T) {
	workDir := t.TempDir()
	authDir := filepath.Join(workDir, "auth")
	require.NoError(t, os.MkdirAll(authDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, KubeconfigArtifactName), []byte("kc"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, KubeadminPasswordArtifactName), []byte("pw"), 0o600))
	writeGeneratedManifest(t, workDir, validKubeadminPasswordHash, validKubeconfigCryptoRetention())

	creds, err := readGeneratedCredentials(workDir)
	require.NoError(t, err)
	assert.Equal(t, validKubeadminPasswordHash, creds.KubeadminPasswordHash)
	assert.Equal(t, validKubeconfigCryptoRetention(), creds.KubeconfigCryptoRetention)
	assert.Equal(t, []byte("kc"), creds.Kubeconfig)
	assert.Equal(t, []byte("pw"), creds.KubeadminPassword)
}

func TestReadGeneratedCredentials_MissingAuth(t *testing.T) {
	_, err := readGeneratedCredentials(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading generated kubeconfig")
}

func TestReadGeneratedCredentials_MissingHash(t *testing.T) {
	workDir := t.TempDir()
	authDir := filepath.Join(workDir, "auth")
	require.NoError(t, os.MkdirAll(authDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, KubeconfigArtifactName), []byte("kc"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(authDir, KubeadminPasswordArtifactName), []byte("pw"), 0o600))
	writeGeneratedManifest(t, workDir, "", seedreconfig.KubeConfigCryptoRetention{})

	_, err := readGeneratedCredentials(workDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no kubeadmin_password_hash")
}

// TestReadClusterConfigManifest_FromISO proves the ISO fallback path: when the manifest is not on
// disk, it is extracted from the generated config-image ISO.
func TestReadClusterConfigManifest_FromISO(t *testing.T) {
	workDir := t.TempDir()
	payload := []byte(`{"kubeadmin_password_hash":"from-iso"}`)
	makeConfigImageISO(t, filepath.Join(workDir, configImageISOName), payload)

	data, err := readClusterConfigManifest(workDir)
	require.NoError(t, err)
	assert.JSONEq(t, string(payload), string(data))
}

// makeConfigImageISO writes an ISO9660 image at isoPath containing
// cluster-configuration/manifest.json with the given contents, mimicking the installer's
// config-image.
func makeConfigImageISO(t *testing.T, isoPath string, manifest []byte) {
	t.Helper()
	d, err := diskfs.Create(isoPath, 10*1024*1024, diskfs.Raw, diskfs.SectorSize(2048))
	require.NoError(t, err)

	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, WorkDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, fs.Mkdir("/cluster-configuration"))
	f, err := fs.OpenFile("/cluster-configuration/manifest.json", os.O_CREATE|os.O_RDWR)
	require.NoError(t, err)
	_, err = f.Write(manifest)
	require.NoError(t, err)

	iso, ok := fs.(*iso9660.FileSystem)
	require.True(t, ok)
	require.NoError(t, iso.Finalize(iso9660.FinalizeOptions{RockRidge: true}))
	require.NoError(t, d.File.Close())
}
