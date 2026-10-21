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

	"github.com/pelletier/go-toml"
	"go.podman.io/image/v5/oci/layout"
	istorage "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	cstorage "go.podman.io/storage"

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

// TestDurabilityStoreReadableAsAdditionalImageStoreIntegration exercises the consumer side
// of the §6.6 compatibility gate: it builds a real overlay durability store and then opens
// it the way the installed node will -- as a read-only additional image store declared by
// the rendered storage.conf drop-in -- and asserts the imported image's canonical
// name@digest resolves from that read-only store and that its image (manifest + config blob)
// is actually readable through it. TestBuildDurabilityStoreIntegration covers the producer
// side (the on-disk overlay driver/layer format); this covers that a node can consume what
// was produced, using the rendered drop-in rather than hand-built store options.
//
// Like the build test it is root-gated and skips when overlay is unavailable; past the
// overlay gate, any build or read failure is a genuine regression and fails the test.
func TestDurabilityStoreReadableAsAdditionalImageStoreIntegration(t *testing.T) {
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

	// Register the store at the absolute path it is actually committed to on this media
	// (<mediaRoot>/store), so the rendered storage.conf's additionalimagestores entry points
	// at the real on-disk store and can be used to open it below -- exactly as the installed
	// node opens the store once p2 is mounted at that path. (The build always commits to
	// <p2Root>/store, so this equals res.StorePath.)
	runtimeStorePath := filepath.Join(mediaRoot, durabilityStoreDirName)

	// Building and then opening an overlay containers/storage store needs root (or a user
	// namespace) for the layer chown, so guard it like the other store-backed tests.
	if os.Geteuid() != 0 {
		t.Skip("building and opening an overlay containers-storage store requires root (or a user namespace)")
	}
	// Probe overlay availability up front, so a subsequent build or read error is a genuine
	// regression -- not an environment limitation silently swallowed by a skip.
	requireOverlayStore(t)

	// Build the store; with overlay confirmed available, a build error is a real regression.
	res, err := BuildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, mediaRoot, runtimeStorePath, runtime.GOARCH)
	require.NoError(t, err, "overlay is available, so a build error is a real regression")
	require.NotNil(t, res)
	require.Equal(t, runtimeStorePath, res.StorePath)

	// Read the rendered storage.conf the way the node does: it must declare the overlay driver
	// and register the committed store at its final path as an additional (read-only) image
	// store. Driving the open from the parsed drop-in (not hand-built options) is what makes
	// this the consumer-side gate.
	driver, additionalStores := parseRenderedStorageConf(t, res.StorageConfPath)
	require.Equal(t, durabilityStoreDriver, driver)
	require.Equal(t, []string{res.StorePath}, additionalStores, "the drop-in must register the committed store at its final path")

	// Open a throwaway, empty primary overlay store with the committed store attached as a
	// read-only additional image store (containers/storage maps additionalimagestores to the
	// overlay driver's imagestore option), mirroring how the node consumes the media.
	store, err := cstorage.GetStore(cstorage.StoreOptions{
		GraphDriverName:    driver,
		GraphRoot:          filepath.Join(t.TempDir(), "node-graph"),
		RunRoot:            filepath.Join(t.TempDir(), "node-run"),
		GraphDriverOptions: []string{"imagestore=" + additionalStores[0]},
	})
	require.NoError(t, err, "opening the committed store as a read-only additional image store is the consumer-side compatibility gate")
	defer func() {
		if _, err := store.Shutdown(false); err != nil {
			t.Logf("shutting down read-only store: %v", err)
		}
	}()

	// The imported image resolves from the read-only additional store under its canonical
	// name@digest -- the exact reference the node asks for offline.
	canonical := integrationImageName + "@" + recordedDigest
	storeRef, err := istorage.Transport.ParseStoreReference(store, canonical)
	require.NoError(t, err)
	_, storedImage, err := istorage.ResolveReference(storeRef)
	require.NoError(t, err, "canonical name@digest must resolve from the read-only additional image store")
	require.NotNil(t, storedImage)

	// ...and its image is genuinely readable through that store, not just indexed: open it and
	// read the manifest and config blob back out of the additional store.
	img, err := storeRef.NewImage(context.Background(), &types.SystemContext{})
	require.NoError(t, err, "the imported image must be openable from the read-only additional image store")
	defer img.Close()
	manifestBytes, _, err := img.Manifest(context.Background())
	require.NoError(t, err, "the imported image's manifest must be readable from the additional image store")
	require.NotEmpty(t, manifestBytes)
	configBlob, err := img.ConfigBlob(context.Background())
	require.NoError(t, err, "the imported image's config must be readable from the additional image store")
	require.NotEmpty(t, configBlob)
}

// parseRenderedStorageConf reads the rendered storage.conf drop-in and returns the graph
// driver and the additional image stores it registers, so a test can open the store exactly
// as the drop-in declares it rather than from hand-built options.
func parseRenderedStorageConf(t *testing.T, confPath string) (string, []string) {
	t.Helper()
	data, err := os.ReadFile(confPath)
	require.NoError(t, err)
	var conf struct {
		Storage struct {
			Driver  string `toml:"driver"`
			Options struct {
				AdditionalImageStores []string `toml:"additionalimagestores"`
			} `toml:"options"`
		} `toml:"storage"`
	}
	require.NoError(t, toml.Unmarshal(data, &conf))
	return conf.Storage.Driver, conf.Storage.Options.AdditionalImageStores
}
