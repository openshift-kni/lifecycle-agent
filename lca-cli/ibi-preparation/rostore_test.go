package ibi_preparation

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/go-logr/logr"
	preinstallUtils "github.com/rh-ecosystem-edge/preinstall-utils/pkg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagetypes "go.podman.io/storage/types"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/api/ibiconfig"
	"github.com/openshift-kni/lifecycle-agent/internal/common"
	"github.com/openshift-kni/lifecycle-agent/internal/ostreeclient"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/localimages"
	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
	rpmostreeclient "github.com/openshift-kni/lifecycle-agent/lca-cli/ostreeclient"
)

// writeFakeOverlayStore creates a directory shaped like a containers/storage overlay store --
// the overlay graph-driver dir, an overlay-images/images.json listing numImages images, and the
// overlay-layers/layers.json metadata -- plus a layer file with a distinctive mode, so the
// orchestration tests exercise copy/validation/mode-preservation without needing root or a real
// store. The full real-store fidelity is covered by the create-usb integration tests.
func writeFakeOverlayStore(t *testing.T, dir string, numImages int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, roStoreOverlayGraphDir), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "overlay-images"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "overlay-layers"), 0o755))

	images := make([]map[string]string, numImages)
	for i := range images {
		images[i] = map[string]string{"id": fmt.Sprintf("image-%d", i)}
	}
	data, err := json.Marshal(images)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, roStoreOverlayImagesJSON), data, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, roStoreOverlayLayersJSON), []byte("[]"), 0o644))
	// A layer file with a non-default mode, to prove the copy preserves original file modes and
	// does not blanket-rewrite them.
	require.NoError(t, os.WriteFile(filepath.Join(dir, roStoreOverlayGraphDir, "layerfile"), []byte("layer"), 0o640))
}

// plainCopyTree is an unprivileged mode-preserving tree copy used by the orchestration unit
// tests. The production copier (defaultCopyReadOnlyStore / CopyWithTar) also preserves numeric
// ownership via chown, which needs root; that copier is covered by the root-gated integration
// test, while these tests stub in this plain copy to exercise the surrounding orchestration.
func plainCopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(p) //nolint:gosec // test-only copy of a fixture under a temp dir
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// withDeployRoot points common.OstreeDeployPathPrefix (the ostree deploy path prefix) at a temp
// directory for the duration of the test, so GetStaterootPath resolves to a writable location.
func withDeployRoot(t *testing.T) {
	t.Helper()
	orig := common.OstreeDeployPathPrefix
	common.OstreeDeployPathPrefix = t.TempDir()
	t.Cleanup(func() { common.OstreeDeployPathPrefix = orig })
}

// newSelfContainedPrepare builds an IBIPrepare configured for a self-contained disconnected
// prepare reading media from mediaRoot.
func newSelfContainedPrepare(mockOps ops.Ops, ostreeMock ostreeclient.IClient, mediaRoot, seedVersion string) *IBIPrepare {
	return NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, nil, &ibiconfig.IBIPrepareConfig{
		Disconnected:    true,
		Profile:         ibiconfig.ProfileSelfContained,
		LocalImagesPath: mediaRoot,
		SeedVersion:     seedVersion,
	})
}

func TestValidateOverlayStoreFormat(t *testing.T) {
	t.Run("valid overlay store passes", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeOverlayStore(t, dir, 1)
		assert.NoError(t, validateOverlayStoreFormat(dir))
	})

	t.Run("missing overlay graph dir fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeOverlayStore(t, dir, 1)
		require.NoError(t, os.RemoveAll(filepath.Join(dir, roStoreOverlayGraphDir)))
		assert.ErrorContains(t, validateOverlayStoreFormat(dir), "overlay layer format")
	})

	t.Run("store with zero images fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeOverlayStore(t, dir, 0)
		assert.ErrorContains(t, validateOverlayStoreFormat(dir), "records no images")
	})

	t.Run("missing layer metadata fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeOverlayStore(t, dir, 1)
		require.NoError(t, os.Remove(filepath.Join(dir, roStoreOverlayLayersJSON)))
		assert.ErrorContains(t, validateOverlayStoreFormat(dir), "overlay layer metadata")
	})

	t.Run("missing images.json fails", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeOverlayStore(t, dir, 1)
		require.NoError(t, os.Remove(filepath.Join(dir, roStoreOverlayImagesJSON)))
		assert.ErrorContains(t, validateOverlayStoreFormat(dir), "reading store image metadata")
	})
}

func TestValidateSourceStoreFailsClosedWhenAbsent(t *testing.T) {
	// A non-existent store path.
	assert.ErrorContains(t, validateSourceStore(filepath.Join(t.TempDir(), "store")), "not found")

	// A path that exists but is a file, not a store directory.
	f := filepath.Join(t.TempDir(), "store")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o600))
	assert.ErrorContains(t, validateSourceStore(f), "not a directory")
}

// TestPersistReadOnlyStoreHappyPath proves the full handoff: the pre-built store is copied onto
// the installed disk (modes preserved), validated as an overlay store, verified against the
// digest manifest, SELinux-labelled, atomically sealed at the post-pivot ro-store path, and
// registered via a storage.conf.d drop-in using append=true -- in that order, with the store
// verified at its staging path before the move.
func TestPersistReadOnlyStoreHappyPath(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 2)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	staging := destStore + roStoreStagingSuffix
	require.NoError(t, os.MkdirAll(filepath.Dir(destStore), 0o755))

	deploymentDir := filepath.Join(common.GetStaterootPath(osname), "deploy", "abc123.0")
	ostreeMock.EXPECT().GetDeploymentDir(osname).Return(deploymentDir, nil).Times(1)
	// The store is labelled at its staging path (before the atomic move into place).
	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, staging).Return("", nil).Times(1)

	var verifiedPath string
	verifyPersistedStore = func(_ context.Context, _ *logrus.Logger, storePath string, _ *localimages.DigestManifest) error {
		verifiedPath = storePath
		// The store must still be staged (not yet moved into place) when it is verified.
		assert.DirExists(t, staging)
		assert.NoDirExists(t, destStore)
		return nil
	}

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	require.NoError(t, ibi.persistReadOnlyStore(context.Background(), testManifest()))

	assert.Equal(t, staging, verifiedPath, "the copied store must be verified at its staging path")
	assert.DirExists(t, destStore, "the store must be sealed at the ro-store destination")
	assert.NoDirExists(t, staging, "the staging dir must be gone after the atomic move")

	// Original file modes are preserved by the copy (no blanket mode rewrite).
	fi, err := os.Stat(filepath.Join(destStore, roStoreOverlayGraphDir, "layerfile"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm())

	// The drop-in registers the store at the post-pivot absolute path, with append=true, on the
	// deployment's /etc.
	dropIn := filepath.Join(deploymentDir, common.StorageConfDropInDir, common.RODurabilityStoreDropInName)
	data, err := os.ReadFile(dropIn)
	require.NoError(t, err)
	assert.Contains(t, string(data), `additionalimagestores = ["`+common.RODurabilityStorePath+`", {append = true}]`)
	assert.Contains(t, string(data), "[storage.options]")
}

// TestPersistReadOnlyStoreFailsClosedOnMissingStore proves self-contained media without the
// pre-built store is a hard failure (never a silent no-op), and nothing is written to disk.
func TestPersistReadOnlyStoreFailsClosedOnMissingStore(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	// No ops or ostree calls are expected: the failure is detected before any copy/label/register.
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir() // no <media>/store
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error {
		t.Fatal("verify must not run when the source store is missing")
		return nil
	}

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, "4.15.0")
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "missing or incomplete")

	osname := common.GetStaterootName("4.15.0")
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	assert.NoDirExists(t, destStore)
}

// TestPersistReadOnlyStoreFailsClosedOnIncompleteStore proves that when the copied store fails
// digest-manifest validation, prepare fails and leaves no sealed store and no drop-in.
func TestPersistReadOnlyStoreFailsClosedOnIncompleteStore(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)
	copyReadOnlyStore = plainCopyTree

	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error {
		return fmt.Errorf("image quay.io/x@sha256:deadbeef not resolvable")
	}

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, "4.15.0")
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "digest-manifest validation")

	osname := common.GetStaterootName("4.15.0")
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	assert.NoDirExists(t, destStore, "no store may be sealed when validation fails")
	assert.NoDirExists(t, destStore+roStoreStagingSuffix, "the staging dir must be cleaned up on failure")
}

// TestPersistReadOnlyStorePartialCopyLeavesNothing proves an interrupted/failed copy leaves no
// partial ro-store and no storage.conf entry.
func TestPersistReadOnlyStorePartialCopyLeavesNothing(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)

	osname := common.GetStaterootName("4.15.0")
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	staging := destStore + roStoreStagingSuffix
	require.NoError(t, os.MkdirAll(filepath.Dir(destStore), 0o755))

	copyReadOnlyStore = func(_, dst string) error {
		// Simulate a partial copy: create the staging dir, then fail.
		require.NoError(t, os.MkdirAll(dst, 0o755))
		return fmt.Errorf("copy interrupted")
	}

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, "4.15.0")
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "failed to copy read-only store")

	assert.NoDirExists(t, staging, "a partial copy must be cleaned up")
	assert.NoDirExists(t, destStore, "no store may be sealed after a failed copy")
}

// TestPersistReadOnlyStoreRollsBackOnDropInFailure proves the store and its config are atomic as
// a pair: if the drop-in write fails, the already-moved store is rolled back so prepare never
// leaves a store without its registration.
func TestPersistReadOnlyStoreRollsBackOnDropInFailure(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	require.NoError(t, os.MkdirAll(filepath.Dir(destStore), 0o755))

	// Point the deployment dir at a regular file so creating the storage.conf.d dir under it fails.
	badDeploymentDir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badDeploymentDir, []byte("x"), 0o600))
	ostreeMock.EXPECT().GetDeploymentDir(osname).Return(badDeploymentDir, nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, destStore+roStoreStagingSuffix).Return("", nil).Times(1)

	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "failed to register read-only store")
	assert.NoDirExists(t, destStore, "the sealed store must be rolled back when registration fails")
}

// TestPersistReadOnlyStoreRollbackPreservesPreviousStore proves a re-run never destroys a
// previously valid store: when publishing the replacement gets as far as registration and that
// fails, the store is rolled back to the previous copy (swapped back into place with a second
// RENAME_EXCHANGE), not left empty with a dangling registration. This is the "registration failure"
// side of the atomic swap the Developer of Record asked to cover.
func TestPersistReadOnlyStoreRollbackPreservesPreviousStore(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 2)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	// A previously published, valid store carrying a sentinel the media store does not have.
	require.NoError(t, os.MkdirAll(destStore, 0o755))
	sentinel := filepath.Join(destStore, "previous-store-marker")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep me"), 0o600))

	// Registration fails: point the deployment dir at a regular file so the dir creation under it fails.
	badDeploymentDir := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(badDeploymentDir, []byte("x"), 0o600))
	ostreeMock.EXPECT().GetDeploymentDir(osname).Return(badDeploymentDir, nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, destStore+roStoreStagingSuffix).Return("", nil).Times(1)
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "failed to register read-only store")

	// The previous store is restored intact; no staging is left behind.
	assert.FileExists(t, sentinel, "the previously valid store must be restored when registration fails")
	assert.NoDirExists(t, destStore+roStoreStagingSuffix, "the staging dir must be cleaned up")
}

// TestPersistReadOnlyStoreReplacesExistingStore proves an idempotent re-run replaces a previously
// published store with the freshly copied one -- via the atomic swap -- and leaves no staging behind.
func TestPersistReadOnlyStoreReplacesExistingStore(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 2)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	require.NoError(t, os.MkdirAll(destStore, 0o755))
	staleMarker := filepath.Join(destStore, "stale-marker")
	require.NoError(t, os.WriteFile(staleMarker, []byte("old"), 0o600))

	deploymentDir := filepath.Join(common.GetStaterootPath(osname), "deploy", "abc123.0")
	ostreeMock.EXPECT().GetDeploymentDir(osname).Return(deploymentDir, nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, destStore+roStoreStagingSuffix).Return("", nil).Times(1)
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	require.NoError(t, ibi.persistReadOnlyStore(context.Background(), testManifest()))

	// The stale store is replaced by the freshly copied one; no staging remains.
	assert.NoFileExists(t, staleMarker, "the previous store must be replaced on a successful re-run")
	assert.FileExists(t, filepath.Join(destStore, roStoreOverlayImagesJSON), "the new store must be in place")
	assert.NoDirExists(t, destStore+roStoreStagingSuffix, "the superseded store parked at staging must be removed after a successful publish")
}

// TestPersistReadOnlyStorePreservesPreviousStoreOnPrePublishFailure covers the "before the exchange"
// side the Developer of Record asked to test: a failure that happens after a store is already
// published but before the swap (here, the copy of the replacement fails) must leave the previously
// published store -- and therefore its still-current registration -- completely untouched, because
// destStore is only ever modified by the atomic exchange, which never runs.
func TestPersistReadOnlyStorePreservesPreviousStoreOnPrePublishFailure(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	// No ops or ostree calls: the copy fails before any label/exchange/register.
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	// A previously published, valid store.
	require.NoError(t, os.MkdirAll(destStore, 0o755))
	sentinel := filepath.Join(destStore, "previous-store-marker")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep me"), 0o600))

	copyReadOnlyStore = func(_, dst string) error {
		require.NoError(t, os.MkdirAll(dst, 0o755))
		return fmt.Errorf("copy interrupted")
	}
	exchangeStoreDirs = func(string, string) error {
		t.Fatal("the exchange must not run when the replacement copy fails")
		return nil
	}

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "failed to copy read-only store")

	// The previously valid store is untouched and the partial staging copy is cleaned up.
	assert.FileExists(t, sentinel, "a pre-publish failure must never touch the previously published store")
	assert.NoDirExists(t, destStore+roStoreStagingSuffix, "the partial staging copy must be cleaned up")
}

// TestPersistReadOnlyStoreFailsClosedWhenExchangeUnsupported proves the Developer of Record's
// explicit constraint: if the target filesystem does not support RENAME_EXCHANGE, the prepare fails
// without moving the old store aside, and never falls back to a non-atomic move-aside-then-rename
// that would reopen the crash window.
func TestPersistReadOnlyStoreFailsClosedWhenExchangeUnsupported(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	// A previously published, valid store that must survive an unsupported-exchange failure.
	require.NoError(t, os.MkdirAll(destStore, 0o755))
	sentinel := filepath.Join(destStore, "previous-store-marker")
	require.NoError(t, os.WriteFile(sentinel, []byte("keep me"), 0o600))

	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, destStore+roStoreStagingSuffix).Return("", nil).Times(1)
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }
	// Registration must never be reached when the exchange fails.
	ostreeMock.EXPECT().GetDeploymentDir(gomock.Any()).Times(0)
	exchangeStoreDirs = func(string, string) error { return fmt.Errorf("simulated unsupported exchange") }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "RENAME_EXCHANGE")

	// The previous store is left exactly where it was -- not moved aside -- and staging is cleaned up.
	assert.FileExists(t, sentinel, "an unsupported exchange must not move or destroy the previous store")
	assert.NoDirExists(t, destStore+roStoreStagingSuffix, "the staging copy must be cleaned up after a failed exchange")
}

// TestPersistReadOnlyStoreRecoversLeftoverStagingFromInterruptedSwap proves the recovery property on
// both sides of the exchange: whether a prior run crashed before the exchange (leaving a half-staged
// replacement) or after it (leaving the superseded store parked at staging), the configured destStore
// always still holds a valid store, and the next prepare discards the leftover and republishes
// cleanly. A leftover staging tree plus an existing store models exactly that interrupted state.
func TestPersistReadOnlyStoreRecoversLeftoverStagingFromInterruptedSwap(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 2)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	// The configured store is present and valid -- the exchange never leaves it absent.
	require.NoError(t, os.MkdirAll(destStore, 0o755))
	staleMarker := filepath.Join(destStore, "stale-marker")
	require.NoError(t, os.WriteFile(staleMarker, []byte("old"), 0o600))
	// A leftover staging tree an interrupted run parked behind, which the re-run must discard.
	staging := destStore + roStoreStagingSuffix
	require.NoError(t, os.MkdirAll(staging, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "leftover"), []byte("junk"), 0o600))

	deploymentDir := filepath.Join(common.GetStaterootPath(osname), "deploy", "abc123.0")
	ostreeMock.EXPECT().GetDeploymentDir(osname).Return(deploymentDir, nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, staging).Return("", nil).Times(1)
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	require.NoError(t, ibi.persistReadOnlyStore(context.Background(), testManifest()))

	// The leftover is discarded, the fresh store is published, and nothing is parked at staging.
	assert.NoFileExists(t, staleMarker, "the previous store must be replaced on the re-run")
	assert.NoFileExists(t, filepath.Join(destStore, "leftover"), "the leftover staging tree must not leak into the published store")
	assert.FileExists(t, filepath.Join(destStore, roStoreOverlayImagesJSON), "the freshly copied store must be in place")
	assert.NoDirExists(t, staging, "no store may be left parked at staging after a successful re-run")
}

// TestPersistReadOnlyStoreSELinuxFailureLeavesNothing proves a labelling failure aborts the
// handoff before the store is sealed or registered.
func TestPersistReadOnlyStoreSELinuxFailureLeavesNothing(t *testing.T) {
	saveSeams(t)
	withDeployRoot(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)

	media := t.TempDir()
	writeFakeOverlayStore(t, filepath.Join(media, common.LocalImagesStoreDirName), 1)
	copyReadOnlyStore = plainCopyTree

	const seedVersion = "4.15.0"
	osname := common.GetStaterootName(seedVersion)
	destStore := filepath.Join(common.GetStaterootPath(osname), common.ContainerStoragePath, common.RODurabilityStoreDirName)
	require.NoError(t, os.MkdirAll(filepath.Dir(destStore), 0o755))

	mockOps.EXPECT().RunInHostNamespace("chcon", "-R", "-t", roStoreSELinuxType, destStore+roStoreStagingSuffix).
		Return("", fmt.Errorf("chcon failed")).Times(1)
	verifyPersistedStore = func(context.Context, *logrus.Logger, string, *localimages.DigestManifest) error { return nil }

	ibi := newSelfContainedPrepare(mockOps, ostreeMock, media, seedVersion)
	err := ibi.persistReadOnlyStore(context.Background(), testManifest())
	assert.ErrorContains(t, err, "SELinux label")

	assert.NoDirExists(t, destStore)
	assert.NoDirExists(t, destStore+roStoreStagingSuffix)
}

// TestReadOnlyStoreDropInMergesPreservingExistingEntries proves the rendered drop-in registers
// the ro-store in the effective storage.conf *without clobbering* a pre-existing
// additionalimagestores entry: decoding a base storage.conf and then the drop-in into the same
// struct (as the conf.d loader does) yields both entries, because the drop-in uses append=true.
func TestReadOnlyStoreDropInMergesPreservingExistingEntries(t *testing.T) {
	const baseStorageConf = `[storage]
driver = "overlay"
[storage.options]
additionalimagestores = ["/var/lib/containers/pre-existing-store"]
`
	dropIn := fmt.Sprintf(roStoreStorageConfDropIn, common.RODurabilityStorePath)

	// Decode the base then the drop-in into the same struct, mirroring the conf.d merge order.
	var cfg storagetypes.TomlConfig
	_, err := toml.Decode(baseStorageConf, &cfg)
	require.NoError(t, err)
	_, err = toml.Decode(dropIn, &cfg)
	require.NoError(t, err)

	assert.Equal(t,
		[]string{"/var/lib/containers/pre-existing-store", common.RODurabilityStorePath},
		cfg.Storage.Options.AdditionalImageStores.Values,
		"the drop-in must append the ro-store, preserving the existing additionalimagestores entry")
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.conf")
	require.NoError(t, writeFileAtomic(path, []byte("hello"), 0o644))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	fi, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())

	// No leftover temp files in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

// TestRunSelfContainedPersistsBeforeFinalSyncAndUnmount drives the full self-contained
// disconnected happy path and asserts the read-only store handoff runs after the closure
// self-check and before the final sync and the p2 unmount -- the ordering the unmount-p2 comment
// reserves, so additionalimagestores never points at a path that has gone away.
func TestRunSelfContainedPersistsBeforeFinalSyncAndUnmount(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	statusDir := useTempStatusPartition(t)

	var events []string
	parseDigestManifest = func(string) (*localimages.DigestManifest, error) {
		events = append(events, "parseManifest")
		return testManifest(), nil
	}
	ff := &importerFactory{events: &events}
	newImageImporter = ff.new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		events = append(events, "setupStateroot")
		return nil
	}
	persistReadOnlyStoreFn = func(*IBIPrepare, context.Context, *localimages.DigestManifest) error {
		events = append(events, "persistROStore")
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             ibiconfig.ProfileSelfContained,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").DoAndReturn(func(string, ...string) (string, error) {
		events = append(events, "sync")
		return "", nil
	}).Times(2)
	mockOps.EXPECT().Umount("sdb1").DoAndReturn(func(string) error {
		events = append(events, "umountP2")
		return nil
	}).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)

	require.NoError(t, ibi.Run())

	// The handoff runs after the closure self-check, before the first (core) sync, and before p2
	// is unmounted.
	assert.GreaterOrEqual(t, idxOf(events, "persistROStore"), 0, "the read-only store handoff must run")
	assert.Less(t, idxOf(events, "verifyClosure"), idxOf(events, "persistROStore"))
	assert.Less(t, idxOf(events, "persistROStore"), idxOf(events, "sync"))
	assert.Less(t, idxOf(events, "persistROStore"), idxOf(events, "umountP2"))

	result := readPrepareResult(t, statusDir)
	assert.Equal(t, ibiconfig.ProfileSelfContained, result.Profile)
	assert.Equal(t, resultSuccess, result.Result)
}

// TestRunInstallOnlySkipsReadOnlyStorePersist proves install-only media never runs the
// self-contained read-only store handoff.
func TestRunInstallOnlySkipsReadOnlyStorePersist(t *testing.T) {
	saveSeams(t)
	t.Chdir(t.TempDir())
	statusDir := useTempStatusPartition(t)

	parseDigestManifest = func(string) (*localimages.DigestManifest, error) { return testManifest(), nil }
	newImageImporter = (&importerFactory{}).new
	setupStateroot = func(logr.Logger, ops.Ops, ostreeclient.IClient, rpmostreeclient.IClient, string, string, bool) error {
		return nil
	}
	persistReadOnlyStoreFn = func(*IBIPrepare, context.Context, *localimages.DigestManifest) error {
		t.Fatal("install-only media must not run the read-only store handoff")
		return nil
	}

	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	ostreeMock := ostreeclient.NewMockIClient(ctrl)
	cleanupMock := preinstallUtils.NewMockCleanupDevice(ctrl)
	cfg := &ibiconfig.IBIPrepareConfig{
		Disconnected:        true,
		Profile:             ibiconfig.ProfileInstallOnly,
		LocalImagesPath:     "/var/mnt/ibi",
		ExpectedDigestsPath: "/var/mnt/ibi/manifest.json",
		SeedImage:           testSeedImageRef(),
		SeedVersion:         "4.15.0",
		InstallationDisk:    "/dev/sda",
		UseContainersFolder: true,
		PrecacheDisabled:    true,
	}
	ibi := NewIBIPrepare(logrus.New(), mockOps, nil, ostreeMock, cleanupMock, cfg)

	mockOps.EXPECT().ListBlockDevices().Return(statusDevices("sdb1", "sdb2"), nil).Times(2)
	mockOps.EXPECT().RunInHostNamespace("cat", "/proc/self/mounts").Return("", nil).Times(1)
	mockOps.EXPECT().MountReadOnly("sdb1", cfg.LocalImagesPath).Return(nil).Times(1)
	mockOps.EXPECT().Mount("sdb2", statusDir).Return(nil).Times(1)
	cleanupMock.EXPECT().CleanupInstallDevice(cfg.InstallationDisk).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("coreos-installer", []string{"install", cfg.InstallationDisk}).Return("", nil).Times(1)
	mockOps.EXPECT().SetupContainersFolderCommands().Return(nil).Times(1)
	expectPodmanInfo(mockOps)
	ostreeMock.EXPECT().Undeploy(rhcosOstreeIndex).Return(nil).Times(1)
	mockOps.EXPECT().RunInHostNamespace("sync").Return("", nil).Times(2)
	mockOps.EXPECT().Umount("sdb1").Return(nil).Times(1)
	mockOps.EXPECT().Umount("sdb2").Return(nil).Times(1)

	require.NoError(t, ibi.Run())
}
