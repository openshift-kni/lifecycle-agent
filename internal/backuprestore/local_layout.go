/*
Copyright 2023.

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

package backuprestore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/go-logr/logr"
	"github.com/openshift-kni/lifecycle-agent/version"
)

// The local backup layout contract
// --------------------------------
//
// Every local backup written by StartBackup carries a root-level version
// manifest, backup-layout.json, next to checksums.sha256. It describes the
// on-disk layout the restore side reads and is a contract that is versioned
// independently of SeedFormatVersion: the seed image format and the local
// backup layout evolve separately, so a change to one must never force a bump
// of the other.
//
// The manifest is covered by checksums.sha256 like every other backup file, so
// its integrity is verified before it is parsed. It lives at the backup root
// (not inside a restore-<n> group directory), so the resource loader never
// mistakes it for a resource file.
//
// layoutVersion follows semantic versioning and starts at 1.0.0. The rule for
// what a given LCA accepts is intentionally permissive within a major version
// so that more than one layout version can be read at once as the layout
// evolves (see isAcceptedLayoutVersion):
//
//   - a new, backward-compatible addition bumps the MINOR version (1.0.0 -> 1.1.0);
//     an LCA that understands 1.1.0 keeps reading 1.0.0 backups;
//   - a breaking change to the layout bumps the MAJOR version (1.x -> 2.0.0) and
//     is rejected by older LCAs;
//   - a backup whose layoutVersion is newer than the current one (or carries an
//     unknown major) is rejected as a future/unknown layout rather than being
//     read on a best-effort basis.
//
// A missing manifest is never treated as a legacy (pre-manifest) layout: the
// restore fails terminally. Restore-time incompatibilities are surfaced as
// NewBRFailedError("Restore", ...) so the Upgrade fails instead of silently
// restoring an unverified or incompatible backup.
const (
	// backupLayoutFileName is the root-level version manifest written beside
	// checksums.sha256 for every local backup.
	backupLayoutFileName = "backup-layout.json"

	// currentLayoutVersion is the semver of the local backup layout this LCA
	// writes. Bump the minor for backward-compatible additions and the major for
	// breaking changes (see the contract documented above).
	currentLayoutVersion = "1.0.0"
)

// backupLayoutManifest is the content of backup-layout.json. It records the
// metadata needed to validate a local backup before it is restored.
type backupLayoutManifest struct {
	// LayoutVersion is the semver of the on-disk layout (see currentLayoutVersion).
	LayoutVersion string `json:"layoutVersion"`
	// LCAVersion is the canonical version of the LCA that wrote the backup.
	LCAVersion string `json:"lcaVersion"`
	// Timestamp is the backup creation time, in UTC RFC3339 (human readable and
	// machine parseable).
	Timestamp string `json:"timestamp"`
	// ResourceCount is the total number of resource files in the backup.
	ResourceCount int `json:"resourceCount"`
	// BackupSpecNames lists the names of the backup specs that contributed to the
	// backup.
	BackupSpecNames []string `json:"backupSpecNames"`
}

// isAcceptedLayoutVersion reports whether a backup written with the given
// layoutVersion can be read by this LCA. The rule is permissive within a major
// version so that several layout versions can be accepted at once (see the
// contract documented above):
//
//   - the string must be a valid semver;
//   - it must share the major version of the current layout (currentLayoutVersion);
//   - it must not be newer than the current layout version, so a backup written
//     by a newer LCA whose layout this code does not understand is rejected as a
//     future/unknown version.
func isAcceptedLayoutVersion(layoutVersion string) bool {
	v, err := semver.NewVersion(layoutVersion)
	if err != nil {
		return false
	}
	// currentLayoutVersion is a compile-time constant we control, so it always
	// parses; semver.New panics only on a malformed string.
	current := semver.New(currentLayoutVersion)
	if v.Major != current.Major {
		return false
	}
	return v.Compare(*current) <= 0
}

// writeBackupLayoutManifest writes the root-level backup-layout.json beside
// checksums.sha256 and returns its checksum manifest line so the caller can
// include the manifest in checksums.sha256. resourceCount is the total number of
// resource files written, and specNames the names of the contributing backup
// specs. A write failure is returned as a terminal BRFailedError so the Upgrade
// fails rather than recording an incomplete backup as successful.
func writeBackupLayoutManifest(backupDir string, resourceCount int, specNames []string) (string, error) {
	manifest := backupLayoutManifest{
		LayoutVersion:   currentLayoutVersion,
		LCAVersion:      version.Version,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		ResourceCount:   resourceCount,
		BackupSpecNames: specNames,
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("failed to marshal backup layout manifest: %w", err)
	}

	manifestPath := filepath.Join(backupDir, backupLayoutFileName)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		return "", NewBRFailedError("Backup",
			fmt.Sprintf("failed to write backup layout manifest %s: %s", manifestPath, err.Error()))
	}

	hash := sha256.Sum256(data)
	return fmt.Sprintf("%s  %s", hex.EncodeToString(hash[:]), backupLayoutFileName), nil
}

// loadAndValidateBackupLayout reads, checksum-gates, parses and validates the
// root-level backup-layout.json. It must be called after validateChecksums (which
// verifies the manifest's checksum) and before loading resources. A local backup
// is always expected to carry the manifest: a missing, unreadable, malformed, or
// unknown/future-version manifest is rejected with a terminal BRFailedError
// ("Restore"), and a missing manifest is never treated as a legacy layout. All of
// the manifest metadata is logged on success.
func loadAndValidateBackupLayout(backupPath string, validatedFiles map[string]bool, log logr.Logger) (*backupLayoutManifest, error) {
	manifestPath := filepath.Join(backupPath, backupLayoutFileName)
	data, err := os.ReadFile(manifestPath) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return nil, NewBRFailedError("Restore",
				fmt.Sprintf("backup layout manifest %s is required but missing; refusing to restore", backupLayoutFileName))
		}
		return nil, NewBRFailedError("Restore",
			fmt.Sprintf("failed to read backup layout manifest %s: %s", backupLayoutFileName, err.Error()))
	}

	// The manifest is covered by checksums.sha256, so validateChecksums has
	// already verified its checksum and listed it here. A manifest present on disk
	// but absent from the validated set means the backup is inconsistent.
	if !validatedFiles[backupLayoutFileName] {
		return nil, NewBRFailedError("Restore",
			fmt.Sprintf("backup layout manifest %s is not listed in the checksum manifest; refusing to restore", backupLayoutFileName))
	}

	manifest := &backupLayoutManifest{}
	if err := json.Unmarshal(data, manifest); err != nil {
		return nil, NewBRFailedError("Restore",
			fmt.Sprintf("backup layout manifest %s is malformed: %s", backupLayoutFileName, err.Error()))
	}

	if !isAcceptedLayoutVersion(manifest.LayoutVersion) {
		return nil, NewBRFailedError("Restore",
			fmt.Sprintf("backup layout version %q is not supported by this LCA (current layout version %q); refusing to restore",
				manifest.LayoutVersion, currentLayoutVersion))
	}

	log.Info("Backup layout manifest validated",
		"layoutVersion", manifest.LayoutVersion,
		"lcaVersion", manifest.LCAVersion,
		"timestamp", manifest.Timestamp,
		"resourceCount", manifest.ResourceCount,
		"backupSpecNames", manifest.BackupSpecNames)

	return manifest, nil
}
