package seedrestoration

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/openshift-kni/lifecycle-agent/lca-cli/ops"
)

const (
	testContainerRegistry = "quay.io/seed:latest"
	testRecertImage       = "quay.io/recert:latest"
	testAuthFile          = "/var/tmp/auth.json"
)

// testPaths holds the temporary directories the restoration workflow is
// redirected to.
type testPaths struct {
	servicesDir string
	systemdDir  string
	recertDone  string
	removalDir  string
}

// redirectPaths points every host path the workflow touches at a temporary
// directory and restores the originals once the test ends.
func redirectPaths(t *testing.T) testPaths {
	t.Helper()

	root := t.TempDir()
	paths := testPaths{
		servicesDir: filepath.Join(root, "services"),
		systemdDir:  filepath.Join(root, "systemd"),
		recertDone:  filepath.Join(root, "checks", "recert.done"),
		removalDir:  filepath.Join(root, "removable"),
	}

	origServiceUnitsDir, origSystemdSystemDir := serviceUnitsDir, systemdSystemDir
	origRecertDoneFile, origFoldersToRemove := recertDoneFile, foldersToRemove
	t.Cleanup(func() {
		serviceUnitsDir = origServiceUnitsDir
		systemdSystemDir = origSystemdSystemDir
		recertDoneFile = origRecertDoneFile
		foldersToRemove = origFoldersToRemove
	})

	serviceUnitsDir = paths.servicesDir
	systemdSystemDir = paths.systemdDir
	recertDoneFile = paths.recertDone
	foldersToRemove = []string{paths.removalDir}

	return paths
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("unit"), 0o600))
}

func newTestSeedRestoration(t *testing.T, mockOps *ops.MockOps, recertSkipValidation bool) *SeedRestoration {
	t.Helper()
	log := logrus.New()
	log.SetOutput(io.Discard)
	return NewSeedRestoration(log, mockOps, "/var/backup",
		testContainerRegistry, testAuthFile, testRecertImage, recertSkipValidation)
}

func TestNewSeedRestoration(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)
	log := logrus.New()

	r := NewSeedRestoration(log, mockOps, "/var/backup",
		testContainerRegistry, testAuthFile, testRecertImage, true)

	assert.Equal(t, log, r.log)
	assert.Equal(t, mockOps, r.ops)
	assert.Equal(t, "/var/backup", r.backupDir)
	assert.Equal(t, testContainerRegistry, r.containerRegistry)
	assert.Equal(t, testAuthFile, r.authFile)
	assert.Equal(t, testRecertImage, r.recertContainerImage)
	assert.True(t, r.recertSkipValidation)
}

func TestRestoreSeedCluster(t *testing.T) {
	tests := []struct {
		name string
		// recertSkipValidation selects whether the crypto restore is skipped.
		recertSkipValidation bool
		// recertDoneCreated controls whether the recert.done marker exists.
		recertDoneCreated bool

		rmiErr             error
		restoreCryptoErr   error
		kubeletErr         error
		missingServicesDir bool

		wantErr       bool
		wantNumErrors int
	}{
		{
			name:                 "success skips crypto restore when validation is skipped",
			recertSkipValidation: true,
		},
		{
			name:              "success restores crypto when recert marker exists",
			recertDoneCreated: true,
		},
		{
			name: "success skips crypto restore when recert marker is missing",
		},
		{
			name:                 "reports seed image removal failure",
			recertSkipValidation: true,
			rmiErr:               errors.New("image not found"),
			wantErr:              true,
			wantNumErrors:        1,
		},
		{
			name:              "reports crypto restore failure",
			recertDoneCreated: true,
			restoreCryptoErr:  errors.New("recert failed"),
			wantErr:           true,
			wantNumErrors:     1,
		},
		{
			name:                 "reports kubelet enable failure",
			recertSkipValidation: true,
			kubeletErr:           errors.New("systemctl failed"),
			wantErr:              true,
			wantNumErrors:        1,
		},
		{
			name:                 "reports service unit cleanup failure",
			recertSkipValidation: true,
			missingServicesDir:   true,
			wantErr:              true,
			wantNumErrors:        1,
		},
		{
			name:               "aggregates every restoration failure",
			recertDoneCreated:  true,
			rmiErr:             errors.New("image not found"),
			restoreCryptoErr:   errors.New("recert failed"),
			kubeletErr:         errors.New("systemctl failed"),
			missingServicesDir: true,
			wantErr:            true,
			wantNumErrors:      4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := redirectPaths(t)
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)

			require.NoError(t, os.MkdirAll(paths.removalDir, 0o755))
			writeFile(t, filepath.Join(paths.removalDir, "backup.tar"))

			if !tc.missingServicesDir {
				require.NoError(t, os.MkdirAll(paths.servicesDir, 0o755))
			}
			if tc.recertDoneCreated {
				writeFile(t, paths.recertDone)
			}

			mockOps.EXPECT().RunInHostNamespace("podman", "rmi", testContainerRegistry).
				Return("", tc.rmiErr)
			mockOps.EXPECT().SystemctlAction("enable", "kubelet.service", "--now").
				Return("", tc.kubeletErr)
			if tc.recertDoneCreated && !tc.recertSkipValidation {
				mockOps.EXPECT().RestoreOriginalSeedCrypto(testRecertImage, testAuthFile).
					Return(tc.restoreCryptoErr)
			}

			r := newTestSeedRestoration(t, mockOps, tc.recertSkipValidation)
			err := r.RestoreSeedCluster()

			if !tc.wantErr {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "encountered")
				assert.Contains(t, err.Error(), fmt.Sprintf("%d errors", tc.wantNumErrors))
			}

			// The folder removal step runs last regardless of earlier failures, so
			// the directory must be gone in every case.
			_, statErr := os.Stat(paths.removalDir)
			assert.True(t, os.IsNotExist(statErr), "removal dir should have been deleted")
		})
	}
}

func TestCleanupServiceUnits(t *testing.T) {
	tests := []struct {
		name string
		// units are created directly under the services dir.
		units []string
		// nestedUnits are created one level deeper and must still be processed.
		nestedUnits []string
		// skipSystemdDir leaves the systemd dir absent, so removal reports
		// IsNotExist and must be tolerated.
		skipSystemdDir bool
		// systemdDirIsFile places a regular file where the systemd dir belongs,
		// making removal fail with a non-IsNotExist error.
		systemdDirIsFile bool
		// disableErr makes every systemctl disable call fail.
		disableErr bool

		wantErr       bool
		wantErrSubstr string
		wantRemoved   bool
	}{
		{
			name:        "empty services dir is a no-op",
			wantRemoved: true,
		},
		{
			name:        "disables and removes every service unit",
			units:       []string{"a.service", "b.service"},
			wantRemoved: true,
		},
		{
			name:        "processes units in subdirectories",
			nestedUnits: []string{"c.service"},
			wantRemoved: true,
		},
		{
			name:           "tolerates units missing from the systemd dir",
			units:          []string{"a.service"},
			skipSystemdDir: true,
		},
		{
			name:        "continues when systemctl disable fails",
			units:       []string{"a.service"},
			disableErr:  true,
			wantRemoved: true,
		},
		{
			name:             "returns removal failures",
			units:            []string{"a.service"},
			systemdDirIsFile: true,
			wantErr:          true,
			wantErrSubstr:    "error removing a.service file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paths := redirectPaths(t)
			ctrl := gomock.NewController(t)
			mockOps := ops.NewMockOps(ctrl)

			require.NoError(t, os.MkdirAll(paths.servicesDir, 0o755))
			for _, unit := range tc.units {
				writeFile(t, filepath.Join(paths.servicesDir, unit))
			}
			for _, unit := range tc.nestedUnits {
				writeFile(t, filepath.Join(paths.servicesDir, "nested", unit))
			}

			allUnits := append(append([]string{}, tc.units...), tc.nestedUnits...)

			switch {
			case tc.systemdDirIsFile:
				writeFile(t, paths.systemdDir)
			case !tc.skipSystemdDir:
				require.NoError(t, os.MkdirAll(paths.systemdDir, 0o755))
				for _, unit := range allUnits {
					writeFile(t, filepath.Join(paths.systemdDir, unit))
				}
			}

			for _, unit := range allUnits {
				var err error
				if tc.disableErr {
					err = errors.New("systemctl failed")
				}
				mockOps.EXPECT().SystemctlAction("disable", unit, "--now").Return("", err)
			}

			r := newTestSeedRestoration(t, mockOps, true)
			err := r.cleanupServiceUnits()

			if !tc.wantErr {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to cleanup service units")
				assert.Contains(t, err.Error(), tc.wantErrSubstr)
			}

			if tc.wantRemoved {
				for _, unit := range allUnits {
					_, statErr := os.Stat(filepath.Join(paths.systemdDir, unit))
					assert.True(t, os.IsNotExist(statErr), "%s should have been removed", unit)
				}
			}
		})
	}
}

// TestCleanupServiceUnitsMissingServicesDir guards against the Walk callback
// dereferencing a nil FileInfo: Walk calls it with a nil info and a non-nil
// error when the services directory cannot be read, which used to panic.
func TestCleanupServiceUnitsMissingServicesDir(t *testing.T) {
	paths := redirectPaths(t)
	ctrl := gomock.NewController(t)
	mockOps := ops.NewMockOps(ctrl)

	require.NoError(t, os.MkdirAll(filepath.Dir(paths.servicesDir), 0o755))

	r := newTestSeedRestoration(t, mockOps, true)

	var err error
	require.NotPanics(t, func() { err = r.cleanupServiceUnits() })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to cleanup service units")
}
