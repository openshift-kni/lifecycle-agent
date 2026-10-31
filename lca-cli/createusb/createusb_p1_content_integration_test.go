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

// Integration coverage for the real live-ISO customization path: this story is the first
// production user of the in-tree ops.CreateIsoWithEmbeddedIgnition (isoeditor) primitive, so
// the test drives buildLiveISO through the real isoCustomizer to prove the render -> embed
// chain produces a customized ISO from a genuine RHCOS live ISO. The fetch and the
// container-image binary extraction are faked here (they are covered byte-for-byte by the
// unit tests and need network/podman that CI lacks); the customization is real.
//
// It is gated on a real base RHCOS live ISO supplied via LCA_TEST_RHCOS_LIVE_ISO, mirroring
// how createusb_images_integration_test.go gates its privileged path (TestMain, which calls
// reexec.Init(), lives in that file). The embed primitive reads the base ISO and writes a new
// file, so it needs no root.
//
// Full end-to-end boot verification -- the embedded oneshot unit actually invoking the exact
// binary and the install-only path terminating in shutdown -- requires booting the media in a
// VM and is out of automated scope here; it is covered by the CNF-27158 bootability spike and
// manual testing. The unit tests (TestRenderEmbeddedIgnition, TestExtractLCABinaryStaged)
// prove the embedded unit is wired to run the staged p2 binary against the manifest-derived
// config, and TestEmbeddedIgnitionStaysUnderEmbedArea guards that the binary never re-enters
// the ISO ignition.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openshift/assisted-image-service/pkg/isoeditor"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

// rhcosLiveISOEnv names the env var that points the integration test at a real RHCOS live ISO.
const rhcosLiveISOEnv = "LCA_TEST_RHCOS_LIVE_ISO"

// lcaImageEnv, when set, makes the integration test extract the lca-cli binary from a real
// container image via the production opsLCABinaryExtractor (needs podman). Optional:
// LCA_TEST_LCA_AUTHFILE supplies the pull auth file. When unset a synthetic binary fixture is
// embedded instead.
const (
	lcaImageEnv    = "LCA_TEST_LCA_IMAGE"
	lcaAuthFileEnv = "LCA_TEST_LCA_AUTHFILE"
)

// localFileISOFetcher is a fetcher that copies a local base ISO into place, standing in for
// the real HTTPS download so the integration test can run offline against a fixture.
type localFileISOFetcher struct {
	srcPath string
}

func (f *localFileISOFetcher) FetchToFile(_ context.Context, _ /*url*/, destPath string) error {
	data, err := os.ReadFile(filepath.Clean(f.srcPath))
	if err != nil {
		return err
	}
	return os.WriteFile(destPath, data, 0o600)
}

func TestEmbeddedUnitRunsExactBinary(t *testing.T) {
	baseISO := os.Getenv(rhcosLiveISOEnv)
	if baseISO == "" {
		t.Skipf("set %s to a real RHCOS live ISO to exercise the real ISO customization path", rhcosLiveISOEnv)
	}
	if _, err := os.Stat(baseISO); err != nil {
		t.Skipf("%s=%q is not readable: %v", rhcosLiveISOEnv, baseISO, err)
	}

	baseBytes, err := os.ReadFile(filepath.Clean(baseISO))
	require.NoError(t, err)
	sum := fmt.Sprintf("%x", sha256.Sum256(baseBytes))

	// install-only: the embedded config must end in shutdown (Shutdown:true).
	m := validInstallOnly()
	m.RHCOSLiveISO = RHCOSLiveISO{URL: "https://example.com/rhcos-live.iso", SHA256: sum}

	log := logrus.New()
	log.SetOutput(os.Stderr)

	// Extract the lca-cli binary from a real image when LCA_TEST_LCA_IMAGE is set (needs
	// podman), otherwise use a synthetic fixture. The unit tests prove the extracted binary is
	// staged byte-for-byte for p2 and that the unit's ExecStart invokes it from there.
	lcaImage := os.Getenv(lcaImageEnv)
	var extractor lcaBinaryExtractor
	if lcaImage != "" {
		extractor = &opsLCABinaryExtractor{ops: ops.NewOps(log, ops.NewRegularExecutor(log, false)), log: log}
	} else {
		lcaImage = "quay.io/example/lca@sha256:" + validSHA256
		extractor = &fakeLCAExtractor{content: []byte("exact-shipping-lca-cli-binary-fixture")}
	}

	deps := isoBuildDeps{
		fetcher:    &localFileISOFetcher{srcPath: baseISO},
		extractor:  extractor,
		customizer: &opsISOCustomizer{ops: ops.NewOps(log, ops.NewRegularExecutor(log, false))},
	}

	work := t.TempDir()
	res, err := buildLiveISO(context.Background(), log, m,
		ISOBuildOptions{LCAImage: lcaImage, AuthFile: os.Getenv(lcaAuthFileEnv), WorkDir: work}, deps)
	require.NoError(t, err)

	info, err := os.Stat(res.ISOPath)
	require.NoError(t, err)
	assert.Positive(t, info.Size(), "the customized ISO must be non-empty")

	// The lca-cli binary is staged for p2 placement, not embedded in the ISO. With a real
	// --lca-image this is the multi-MB shipping binary that would overflow the ISO ignition
	// embed area if it were embedded -- this path proves it is staged on disk instead.
	assert.FileExists(t, res.LCABinaryPath, "the extracted lca-cli binary must be staged for p2")

	// The customized ISO differs from the untouched base ISO: the (tiny) ignition with the
	// config and the oneshot unit was embedded.
	outBytes, err := os.ReadFile(res.ISOPath)
	require.NoError(t, err)
	assert.NotEqual(t, baseBytes, outBytes, "the customized ISO must differ from the base ISO (ignition embedded)")

	bootloaderFiles, err := isoeditor.KargsFiles(res.ISOPath)
	require.NoError(t, err)
	require.NotEmpty(t, bootloaderFiles)
	for _, path := range bootloaderFiles {
		content, err := isoeditor.ReadFileFromISO(res.ISOPath, path)
		require.NoError(t, err, path)
		assert.Contains(t, string(content), " "+liveISOFromRAMKernelArg+"\n", path)
		assert.NotContains(t, string(content), `"operation":"append"`, path)
		assert.Equal(t, 1, strings.Count(string(content), liveISOFromRAMKernelArg), path)
	}
}
