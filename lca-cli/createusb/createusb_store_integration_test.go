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

// Integration coverage for the real durability-store build path (BuildDurabilityStore):
// a genuine OCI layout + digests.json are staged under a media root (exactly as
// BuildImageLayout produces), then BuildDurabilityStore imports the closure into a real
// overlay containers/storage store at <p2Root>/store through localimages, validates it
// against the overlay driver/layer format, and renders the storage.conf drop-in.
//
// Building an overlay store materialises layers into containers/storage, which needs root
// (or a user namespace) for the layer chown and overlay support in the environment, so the
// end-to-end build is root-gated and skips when overlay is unavailable -- matching the
// localimages and image-layout integration tests. TestMain (reexec.Init) is defined in
// createusb_images_integration_test.go for the whole package.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.podman.io/image/v5/oci/layout"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

// requireOverlayStore skips the test when an overlay containers/storage store cannot be opened
// in this environment (containers/storage probes real overlay support at open time). With
// overlay confirmed available, a later BuildDurabilityStore error is a genuine build regression
// rather than an environment limitation, so the caller can assert NoError on the build itself.
func requireOverlayStore(t *testing.T) {
	t.Helper()
	im, err := localimages.NewImporter(testLogger(), localimages.StoreConfig{
		GraphDriverName: durabilityStoreDriver,
		GraphRoot:       filepath.Join(t.TempDir(), "overlay-probe"),
		RunRoot:         filepath.Join(t.TempDir(), "overlay-probe-run"),
	})
	if err != nil {
		t.Skipf("overlay containers-storage unavailable in this environment: %v", err)
	}
	require.NoError(t, im.Close())
}

// TestBuildDurabilityStoreIntegration builds a real overlay durability store from a staged
// OCI layout for self-contained media and asserts the store, its overlay layer format, and
// the storage.conf drop-in are produced on the media.
func TestBuildDurabilityStoreIntegration(t *testing.T) {
	// Stage a real OCI layout + digests.json under the media root, as BuildImageLayout does.
	mediaRoot := t.TempDir()
	srcDir := buildMultiArchSourceLayout(t, []string{runtime.GOARCH}, "src")
	srcRef, err := layout.NewReference(srcDir, "src")
	require.NoError(t, err)

	destLayout := filepath.Join(mediaRoot, integrationLayoutDir)
	recordedDigest, err := copyIntoLayout(context.Background(), srcRef, destLayout, integrationOCITag, "")
	require.NoError(t, err)

	entry := localimages.ImageEntry{Name: integrationImageName, Digest: recordedDigest, OCITag: integrationOCITag}
	manifest := localimages.DigestManifest{Version: digestManifestVersion, OCILayoutPath: integrationLayoutDir, Images: []localimages.ImageEntry{entry}}
	writeJSON(t, filepath.Join(mediaRoot, digestManifestFileName), manifest)

	// Importing layers into an overlay containers/storage store needs root (or a user
	// namespace) for the layer chown, so guard it like the localimages store-backed tests.
	if os.Geteuid() != 0 {
		t.Skip("building an overlay containers-storage store requires root (or a user namespace)")
	}
	// Probe overlay availability (e.g. no overlay-on-overlay) up front, so a subsequent
	// BuildDurabilityStore error is a genuine build regression -- not an environment limitation
	// silently swallowed by a skip.
	requireOverlayStore(t)

	// The layout carries the runtime.GOARCH instance, so the build selects that architecture.
	res, err := BuildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, mediaRoot, testRuntimeStorePath, runtime.GOARCH)
	require.NoError(t, err, "overlay is available, so a build error is a real regression")
	require.NotNil(t, res)

	// The store was staged at <p2Root>/store in overlay format, and reports the imported image.
	assert.Equal(t, filepath.Join(mediaRoot, durabilityStoreDirName), res.StorePath)
	assert.Equal(t, 1, res.ImportedImages)
	assert.Positive(t, res.StoreBytes)
	assert.DirExists(t, filepath.Join(res.StorePath, overlayGraphDirName))
	require.NoError(t, validateOverlayStore(res.StorePath))

	// images.json records the imported image under its canonical name@digest.
	imagesData, err := os.ReadFile(filepath.Join(res.StorePath, overlayImagesJSONPath))
	require.NoError(t, err)
	var images []struct {
		Names  []string `json:"names"`
		Digest string   `json:"digest"`
	}
	require.NoError(t, json.Unmarshal(imagesData, &images))
	require.Len(t, images, 1)
	assert.Contains(t, images[0].Names, integrationImageName+"@"+recordedDigest)

	// The storage.conf drop-in was rendered beside the store, registering it at the runtime path.
	assert.Equal(t, filepath.Join(mediaRoot, storageConfDropInName), res.StorageConfPath)
	conf, err := os.ReadFile(res.StorageConfPath)
	require.NoError(t, err)
	assert.Contains(t, string(conf), testRuntimeStorePath)
}
