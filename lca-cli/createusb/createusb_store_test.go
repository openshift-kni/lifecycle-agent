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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
)

const storeTestDigest = "sha256:" + validSHA256

// fakeStoreImporter is a storeImporter stub. On a successful ImportAll it writes a minimal,
// valid overlay store layout at graphRoot (set by the factory from the StoreConfig) so the
// real validateOverlayStore/dirSize run against a store shaped like containers/storage would
// produce.
type fakeStoreImporter struct {
	t           *testing.T
	importErr   error
	closeErr    error
	createValid bool
	imageCount  int

	graphRoot      string
	importedRoot   string
	importedImages int
	importCalls    int
	closeCalls     int
}

func (f *fakeStoreImporter) ImportAll(_ context.Context, mediaRoot string, m *localimages.DigestManifest) error {
	f.importCalls++
	f.importedRoot = mediaRoot
	if m != nil {
		f.importedImages = len(m.Images)
	}
	if f.importErr != nil {
		return f.importErr
	}
	if f.createValid {
		writeFakeOverlayStore(f.t, f.graphRoot, f.imageCount)
	}
	return nil
}

func (f *fakeStoreImporter) Close() error {
	f.closeCalls++
	return f.closeErr
}

// writeFakeOverlayStore writes the overlay driver directories and metadata files
// containers/storage produces for an overlay store with the given number of images.
func writeFakeOverlayStore(t *testing.T, graphRoot string, images int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(graphRoot, overlayGraphDirName), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(graphRoot, "overlay-images"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(graphRoot, "overlay-layers"), 0o750))

	records := make([]map[string]string, images)
	for i := range records {
		records[i] = map[string]string{"id": fmt.Sprintf("image-%d", i)}
	}
	data, err := json.Marshal(records)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(graphRoot, overlayImagesJSONPath), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(graphRoot, overlayLayersJSONPath), []byte("[]"), 0o600))
}

// storeTestDeps wires the injectable seams around a fake importer and a fixed parsed
// manifest. capturedCfg receives the StoreConfig the core opens the store with.
func storeTestDeps(fake *fakeStoreImporter, manifest *localimages.DigestManifest, parseErr, newErr error, capturedCfg *localimages.StoreConfig) storeBuildDeps {
	return storeBuildDeps{
		parseManifest: func(string) (*localimages.DigestManifest, error) {
			if parseErr != nil {
				return nil, parseErr
			}
			return manifest, nil
		},
		newImporter: func(_ *logrus.Logger, cfg localimages.StoreConfig) (storeImporter, error) {
			if capturedCfg != nil {
				*capturedCfg = cfg
			}
			if newErr != nil {
				return nil, newErr
			}
			fake.graphRoot = cfg.GraphRoot
			return fake, nil
		},
	}
}

func testManifest(images int) *localimages.DigestManifest {
	entries := make([]localimages.ImageEntry, images)
	for i := range entries {
		entries[i] = localimages.ImageEntry{
			Name:   fmt.Sprintf("quay.io/example/image-%d", i),
			Digest: storeTestDigest,
			OCITag: fmt.Sprintf("image-%d", i),
		}
	}
	return &localimages.DigestManifest{Version: digestManifestVersion, OCILayoutPath: "oci", Images: entries}
}

const testRuntimeStorePath = "/var/mnt/ibi-images/store"

// testTargetArch is the explicit target node architecture the store build selects image
// instances for (see localimages.StoreConfig.Architecture).
const testTargetArch = "amd64"

// TestBuildDurabilityStoreInstallOnlyEmitsNothing proves install-only ships no durability
// store: the stage is a no-op (nil result, nil error) that never opens an importer and
// writes nothing to the media root.
func TestBuildDurabilityStoreInstallOnlyEmitsNothing(t *testing.T) {
	p2Root := t.TempDir()
	fake := &fakeStoreImporter{t: t}
	deps := storeTestDeps(fake, testManifest(3), nil, nil, nil)

	res, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeInstallOnly}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.NoError(t, err)
	assert.Nil(t, res)

	assert.Equal(t, 0, fake.importCalls, "install-only must not open/import a store")
	assert.NoFileExists(t, filepath.Join(p2Root, storageConfDropInName))
	assert.NoDirExists(t, filepath.Join(p2Root, durabilityStoreDirName))
}

// TestBuildDurabilityStoreSelfContained covers the self-contained happy path: the closure is
// imported into an overlay store at <p2Root>/store, the store is validated and sized, and the
// storage.conf drop-in is rendered beside it.
func TestBuildDurabilityStoreSelfContained(t *testing.T) {
	p2Root := t.TempDir()
	manifest := testManifest(3)
	fake := &fakeStoreImporter{t: t, createValid: true, imageCount: 3}
	var cfg localimages.StoreConfig
	deps := storeTestDeps(fake, manifest, nil, nil, &cfg)

	res, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.NoError(t, err)
	require.NotNil(t, res)

	// The store was built with the overlay driver into a build-unique staging directory under
	// p2 (never store/ directly), then committed by rename to store/.
	wantStorePath := filepath.Join(p2Root, durabilityStoreDirName)
	assert.Equal(t, wantStorePath, res.StorePath)
	assert.Equal(t, durabilityStoreDriver, cfg.GraphDriverName, "the store must be built with the overlay driver (§6.6 gate)")
	assert.Equal(t, p2Root, filepath.Dir(cfg.GraphRoot), "the store is built directly under the p2 root")
	assert.True(t, strings.HasPrefix(filepath.Base(cfg.GraphRoot), storeStagingDirPrefix), "the store is built into a staging dir, not store/ directly")
	assert.NotEmpty(t, cfg.RunRoot)
	assert.NotEqual(t, p2Root, filepath.Dir(cfg.RunRoot), "the runroot must live outside the p2 media tree")
	assert.Equal(t, testTargetArch, cfg.Architecture, "the target node architecture must be selected explicitly, not the build host's")
	assert.Equal(t, 1, fake.importCalls)
	assert.Equal(t, p2Root, fake.importedRoot, "the layout is resolved under the p2 media root")
	assert.Equal(t, 3, fake.importedImages)
	assert.GreaterOrEqual(t, fake.closeCalls, 1, "the store must be shut down after import")

	// The committed store/ is the validated overlay store, and no staging dir is left behind.
	assert.DirExists(t, filepath.Join(wantStorePath, overlayGraphDirName))
	assertNoStagingDirs(t, p2Root)

	// The storage.conf drop-in was rendered beside the store and the result reports both paths.
	wantConfPath := filepath.Join(p2Root, storageConfDropInName)
	assert.Equal(t, wantConfPath, res.StorageConfPath)
	assert.FileExists(t, wantConfPath)
	assert.Equal(t, 3, res.ImportedImages)
	assert.Positive(t, res.StoreBytes)
}

// assertNoStagingDirs asserts p2Root carries no leftover store staging directories.
func assertNoStagingDirs(t *testing.T, p2Root string) {
	t.Helper()
	entries, err := os.ReadDir(p2Root)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), storeStagingDirPrefix), "leftover staging dir %q", e.Name())
	}
}

// TestBuildDurabilityStoreStorageConfContents proves the rendered drop-in registers the store
// as a read-only additional image store under overlay at the runtime path, and never
// references a registry (AC: no IDMS/registry in the primary model).
func TestBuildDurabilityStoreStorageConfContents(t *testing.T) {
	p2Root := t.TempDir()
	fake := &fakeStoreImporter{t: t, createValid: true, imageCount: 1}
	deps := storeTestDeps(fake, testManifest(1), nil, nil, nil)

	res, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.NoError(t, err)

	data, err := os.ReadFile(res.StorageConfPath)
	require.NoError(t, err)
	conf := string(data)

	assert.Contains(t, conf, `driver = "overlay"`)
	assert.Contains(t, conf, "additionalimagestores")
	assert.Contains(t, conf, testRuntimeStorePath)

	// The primary model carries no registry: the actual config directives (the non-comment
	// lines) must declare no registry/mirror. The comment lines legitimately mention the
	// documented-only fallback, so only the directives are checked.
	var directives []string
	for _, line := range strings.Split(conf, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			directives = append(directives, trimmed)
		}
	}
	config := strings.Join(directives, "\n")
	assert.NotContains(t, config, "localhost:5000")
	assert.NotContains(t, config, "registry")
	assert.NotContains(t, config, "mirror")
}

// TestBuildDurabilityStoreRejectsBadRuntimePath covers the guard on the runtime store path:
// an empty or relative path cannot form a valid additionalimagestores entry, so the build
// fails without opening a store.
func TestBuildDurabilityStoreRejectsBadRuntimePath(t *testing.T) {
	for name, path := range map[string]string{
		"empty":    "",
		"blank":    "   ",
		"relative": "var/mnt/store",
	} {
		t.Run(name, func(t *testing.T) {
			p2Root := t.TempDir()
			fake := &fakeStoreImporter{t: t}
			deps := storeTestDeps(fake, testManifest(1), nil, nil, nil)

			_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, path, testTargetArch, deps)
			require.Error(t, err)
			assert.Equal(t, 0, fake.importCalls, "no store must be opened when the runtime path is invalid")
		})
	}
}

// TestBuildDurabilityStoreRejectsEmptyTargetArch covers the guard on the target architecture:
// a missing/blank target arch would let the import default to the build host's architecture
// (copy.CopySystemImage), so the build fails without opening a store.
func TestBuildDurabilityStoreRejectsEmptyTargetArch(t *testing.T) {
	for name, arch := range map[string]string{
		"empty": "",
		"blank": "   ",
	} {
		t.Run(name, func(t *testing.T) {
			p2Root := t.TempDir()
			fake := &fakeStoreImporter{t: t}
			deps := storeTestDeps(fake, testManifest(1), nil, nil, nil)

			_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, arch, deps)
			require.Error(t, err)
			assert.ErrorContains(t, err, "target architecture")
			assert.Equal(t, 0, fake.importCalls, "no store must be opened when the target architecture is missing")
		})
	}
}

// TestBuildDurabilityStoreManifestError covers a missing/unreadable on-media digest manifest:
// the build fails before opening a store (the image-layout stage must run first).
func TestBuildDurabilityStoreManifestError(t *testing.T) {
	p2Root := t.TempDir()
	fake := &fakeStoreImporter{t: t}
	deps := storeTestDeps(fake, nil, assert.AnError, nil, nil)

	_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.Error(t, err)
	assert.ErrorContains(t, err, digestManifestFileName)
	assert.Equal(t, 0, fake.importCalls)
}

// TestBuildDurabilityStoreImportError covers an import failure: the error is surfaced and no
// half-built store is left on the media.
func TestBuildDurabilityStoreImportError(t *testing.T) {
	p2Root := t.TempDir()
	fake := &fakeStoreImporter{t: t, importErr: assert.AnError}
	deps := storeTestDeps(fake, testManifest(2), nil, nil, nil)

	_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.Error(t, err)
	assert.ErrorContains(t, err, "importing image closure")
	assert.NoDirExists(t, filepath.Join(p2Root, durabilityStoreDirName), "a failed import must leave no store on the media")
	assert.NoFileExists(t, filepath.Join(p2Root, storageConfDropInName))
	assertNoStagingDirs(t, p2Root)
}

// TestBuildDurabilityStoreOpenError covers a store-open failure (the §6.6 gate firing when
// the build environment cannot produce an overlay store): the error names the overlay driver.
func TestBuildDurabilityStoreOpenError(t *testing.T) {
	p2Root := t.TempDir()
	fake := &fakeStoreImporter{t: t}
	deps := storeTestDeps(fake, testManifest(1), nil, assert.AnError, nil)

	_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.Error(t, err)
	assert.ErrorContains(t, err, durabilityStoreDriver)
	assert.NoDirExists(t, filepath.Join(p2Root, durabilityStoreDirName))
	assertNoStagingDirs(t, p2Root)
}

// TestBuildDurabilityStoreValidationError covers the §6.6 gate: an import that leaves a store
// not in overlay format fails validation, the error is surfaced, and the store is removed.
func TestBuildDurabilityStoreValidationError(t *testing.T) {
	p2Root := t.TempDir()
	// createValid is false: ImportAll writes nothing, so the store directory stays empty and
	// validateOverlayStore rejects it.
	fake := &fakeStoreImporter{t: t, createValid: false}
	deps := storeTestDeps(fake, testManifest(1), nil, nil, nil)

	_, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.Error(t, err)
	assert.ErrorContains(t, err, "validating durability store")
	assert.NoDirExists(t, filepath.Join(p2Root, durabilityStoreDirName))
	assert.NoFileExists(t, filepath.Join(p2Root, storageConfDropInName))
	assertNoStagingDirs(t, p2Root)
}

// TestBuildDurabilityStoreReplacesExistingStore proves a rebuild is idempotent: a complete
// store/ from a previous build is replaced (not rejected and not merged), so a re-run after
// an interrupted assembly succeeds.
func TestBuildDurabilityStoreReplacesExistingStore(t *testing.T) {
	p2Root := t.TempDir()
	storePath := filepath.Join(p2Root, durabilityStoreDirName)
	// A complete previous store with a sentinel that must not survive the replacement.
	writeFakeOverlayStore(t, storePath, 1)
	require.NoError(t, os.WriteFile(filepath.Join(storePath, "stale-marker"), []byte("x"), 0o600))

	fake := &fakeStoreImporter{t: t, createValid: true, imageCount: 2}
	deps := storeTestDeps(fake, testManifest(2), nil, nil, nil)

	res, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, fake.importCalls)
	assert.NoFileExists(t, filepath.Join(storePath, "stale-marker"), "the previous store must be replaced, not merged")
	assert.DirExists(t, filepath.Join(storePath, overlayGraphDirName))
	assertNoStagingDirs(t, p2Root)
}

// TestCommitStoreAndConfRecoversOnFailure proves the recoverable swap preserves the last
// valid store + storage.conf when the commit fails partway: a previous pair is restored intact
// rather than destroyed, and no backup or half-committed artifact is left behind.
func TestCommitStoreAndConfRecoversOnFailure(t *testing.T) {
	p2Root := t.TempDir()
	storePath := filepath.Join(p2Root, durabilityStoreDirName)
	confPath := filepath.Join(p2Root, storageConfDropInName)

	// A complete previous pair with sentinels that must survive a failed commit.
	writeFakeOverlayStore(t, storePath, 1)
	require.NoError(t, os.WriteFile(filepath.Join(storePath, "prev-marker"), []byte("keep"), 0o600))
	require.NoError(t, os.WriteFile(confPath, []byte("PREVIOUS CONF"), 0o600))

	// A validated staging store, but a missing staging storage.conf so the conf swap (the last
	// commit step) fails and triggers the full rollback.
	staging := filepath.Join(p2Root, storeStagingDirPrefix+"new")
	writeFakeOverlayStore(t, staging, 1)
	stagingConf := staging + stagingConfSuffix // deliberately not created

	err := commitStoreAndConf(testLogger(), staging, stagingConf, storePath, confPath)
	require.Error(t, err)

	// The previous store and storage.conf are restored intact -- the last valid store is not lost.
	assert.FileExists(t, filepath.Join(storePath, "prev-marker"))
	data, readErr := os.ReadFile(confPath)
	require.NoError(t, readErr)
	assert.Equal(t, "PREVIOUS CONF", string(data))

	// No backup or staging leftovers survive the rollback.
	entries, err := os.ReadDir(p2Root)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), storeBackupPrefix), "leftover backup %q", e.Name())
		assert.False(t, strings.HasPrefix(e.Name(), storeStagingDirPrefix), "leftover staging artifact %q", e.Name())
	}
}

// TestBuildDurabilityStoreGCsStaleStaging proves a staging directory orphaned by a crashed
// earlier build is garbage-collected, so the build self-heals instead of accumulating
// leftovers.
func TestBuildDurabilityStoreGCsStaleStaging(t *testing.T) {
	p2Root := t.TempDir()
	stale := filepath.Join(p2Root, storeStagingDirPrefix+"crashed")
	require.NoError(t, os.MkdirAll(stale, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "junk"), []byte("x"), 0o600))

	fake := &fakeStoreImporter{t: t, createValid: true, imageCount: 1}
	deps := storeTestDeps(fake, testManifest(1), nil, nil, nil)

	res, err := buildDurabilityStore(context.Background(), testLogger(), &Manifest{Mode: ModeSelfContained}, p2Root, testRuntimeStorePath, testTargetArch, deps)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.NoDirExists(t, stale, "a stale staging directory must be garbage-collected")
	assertNoStagingDirs(t, p2Root)
}

// TestValidateOverlayStore exercises the overlay driver/layer-format gate directly.
func TestValidateOverlayStore(t *testing.T) {
	t.Run("valid overlay store", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 2)
		assert.NoError(t, validateOverlayStore(store))
	})

	t.Run("missing overlay graph dir", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 1)
		require.NoError(t, os.RemoveAll(filepath.Join(store, overlayGraphDirName)))
		err := validateOverlayStore(store)
		require.Error(t, err)
		assert.ErrorContains(t, err, "overlay graph-driver directory")
	})

	t.Run("overlay graph path is a file", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 1)
		require.NoError(t, os.RemoveAll(filepath.Join(store, overlayGraphDirName)))
		require.NoError(t, os.WriteFile(filepath.Join(store, overlayGraphDirName), []byte("x"), 0o600))
		err := validateOverlayStore(store)
		require.Error(t, err)
		assert.ErrorContains(t, err, "not a directory")
	})

	t.Run("missing images metadata", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 1)
		require.NoError(t, os.Remove(filepath.Join(store, overlayImagesJSONPath)))
		err := validateOverlayStore(store)
		require.Error(t, err)
		assert.ErrorContains(t, err, "image metadata")
	})

	t.Run("no images recorded", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 0)
		err := validateOverlayStore(store)
		require.Error(t, err)
		assert.ErrorContains(t, err, "records no images")
	})

	t.Run("missing layer metadata", func(t *testing.T) {
		store := t.TempDir()
		writeFakeOverlayStore(t, store, 1)
		require.NoError(t, os.Remove(filepath.Join(store, overlayLayersJSONPath)))
		err := validateOverlayStore(store)
		require.Error(t, err)
		assert.ErrorContains(t, err, "layer metadata")
	})
}

// TestRenderStorageConfDropIn covers the drop-in renderer directly.
func TestRenderStorageConfDropIn(t *testing.T) {
	confPath := filepath.Join(t.TempDir(), storageConfDropInName)
	require.NoError(t, renderStorageConfDropIn(confPath, testRuntimeStorePath))

	data, err := os.ReadFile(confPath)
	require.NoError(t, err)
	conf := string(data)
	assert.Contains(t, conf, "[storage]")
	assert.Contains(t, conf, `driver = "overlay"`)
	assert.Contains(t, conf, "[storage.options]")
	assert.Contains(t, conf, fmt.Sprintf("%q", testRuntimeStorePath))
}
