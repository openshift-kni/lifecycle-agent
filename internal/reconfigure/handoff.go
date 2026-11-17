// Package reconfigure holds the durable handoff the self-contained post-pivot writes for the
// reconfigure result-writer (CNF-27173). The handoff is the cross-process contract between
// post-pivot (installation-configuration.service, the writer) and the result-writer
// (lca-reconfigure-result-writer.service, the reader): it carries the explicit media profile
// and the convergence-timeout anchor (the post-pivot start time) that the result-writer uses
// to decide the reconfigure verdict.
//
// It lives in its own package, depending only on the standard library, so both post-pivot and
// the init-monitor result-writer can share one struct and one durable read/write
// implementation without an import cycle. It is deliberately separate from
// internal/ibistatus, which owns the p4 result file: the handoff is a local file under
// /var/lib/lca (no mount), written once at post-pivot start, whereas ibistatus owns the
// mounted p4 partition and the terminal verdict.
package reconfigure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Handoff is the write-once durable handoff post-pivot writes for the reconfigure
// result-writer. All timing is anchored on PostPivotStartTime, captured on the first
// post-pivot run so installation-configuration.service Restart=on-failure retries never reset
// the deadline.
type Handoff struct {
	// Profile is the explicit media profile (api/seedreconfig.ProfileSelfContained /
	// ProfileInstallOnly). The result-writer runs only for the self-contained profile.
	Profile string `json:"profile"`
	// ConvergenceTimeout is the raw configured timeout string (a Go duration, e.g. "90m") or
	// empty for the default. The result-writer resolves it with
	// api/seedreconfig.ParseReconfigureConvergenceTimeout, so an empty or unparseable value
	// falls back to the default with a warning.
	ConvergenceTimeout string `json:"convergenceTimeout,omitempty"`
	// PostPivotStartTime is the RFC3339 UTC timestamp of the first post-pivot run -- the
	// convergence-timeout anchor. deadline = PostPivotStartTime + resolved timeout.
	PostPivotStartTime string `json:"postPivotStartTime"`
	// SeedVersion is recorded verbatim into the reconfigure result for off-box diagnostics.
	SeedVersion string `json:"seedVersion,omitempty"`
}

// WriteHandoffOnce writes h durably to path only if no handoff already exists there. An
// existing handoff is preserved untouched (write-once), so the PostPivotStartTime anchor is
// captured from the first post-pivot run and survives Restart=on-failure retries. The parent
// directory is created if needed.
func WriteHandoffOnce(path string, h *Handoff) error {
	if _, err := os.Stat(path); err == nil {
		// A handoff already exists; preserve it so the original anchor is kept.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check reconfigure handoff %s: %w", path, err)
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal reconfigure handoff: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create reconfigure handoff directory %s: %w", dir, err)
	}
	if err := writeFileDurable(path, data); err != nil {
		return fmt.Errorf("failed to write reconfigure handoff %s: %w", path, err)
	}
	return nil
}

// WriteCompletionMarker durably records that post-pivot reconfiguration has finished. The
// reconfigure result-writer gates on this marker, not merely the handoff (which is written early,
// right after the clock gate and before recert, to anchor the deadline): if
// installation-configuration.service fails after the handoff is written but before recert
// completes, the After=-ordered writer could otherwise start and record a false success against
// the still-healthy seed cluster. It is idempotent -- an existing marker is left untouched -- and
// the parent directory is created if needed. The content is a human-readable note for a
// technician inspecting the node; only the file's existence is load-bearing.
func WriteCompletionMarker(path string) error {
	if _, err := os.Stat(path); err == nil {
		// Reconfiguration already marked complete; nothing to do.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check reconfigure completion marker %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create reconfigure completion marker directory %s: %w", dir, err)
	}
	if err := writeFileDurable(path, []byte("post-pivot reconfiguration completed\n")); err != nil {
		return fmt.Errorf("failed to write reconfigure completion marker %s: %w", path, err)
	}
	return nil
}

// WritePendingSuccessMarker durably records that the reconfigure result-writer has observed the
// convergence criterion, written BEFORE it attempts to persist the terminal success verdict to the
// p4 partition. If the p4 write then fails and the node reboots past the write-once convergence
// deadline, a later boot re-publishes this already-made success decision rather than re-deciding
// against possibly-transient live state (which could record a false failure for a node that already
// converged). It is idempotent -- an existing marker is left untouched -- and the parent directory
// is created if needed. The content is a human-readable note for a technician; only the file's
// existence is load-bearing.
func WritePendingSuccessMarker(path string) error {
	if _, err := os.Stat(path); err == nil {
		// A pending-success decision is already recorded; keep it.
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to check reconfigure pending-success marker %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create reconfigure pending-success marker directory %s: %w", dir, err)
	}
	if err := writeFileDurable(path, []byte("reconfigure convergence observed; success verdict pending\n")); err != nil {
		return fmt.Errorf("failed to write reconfigure pending-success marker %s: %w", path, err)
	}
	return nil
}

// ReadHandoff reads and parses the handoff at path. An absent file returns (nil, nil) -- a
// clean "not the self-contained reconfigure path" signal. An IO error or malformed JSON is
// returned as an error so the caller can fail safe (no-op) rather than acting on a partial
// handoff.
func ReadHandoff(path string) (*Handoff, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the fixed LCAConfigDir-rooted handoff the caller controls
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read reconfigure handoff %s: %w", path, err)
	}
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("failed to parse reconfigure handoff %s: %w", path, err)
	}
	return &h, nil
}

// writeFileDurable atomically and durably writes data to path: it writes a temp file in the
// same directory, fsyncs it, renames it into place, and fsyncs the parent directory so the
// published file survives a power loss (not merely a process exit).
func writeFileDurable(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".reconfigure-handoff-")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file for %q: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file for %q: %w", path, err)
	}
	if err = os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("setting permissions on %q: %w", path, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing %q: %w", path, err)
	}
	renamed = true
	if err = fsyncDir(dir); err != nil {
		return fmt.Errorf("syncing directory for %q: %w", path, err)
	}
	return nil
}

func fsyncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // dir is the LCAConfigDir the caller controls
	if err != nil {
		return fmt.Errorf("opening %q to sync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", dir, err)
	}
	return nil
}
