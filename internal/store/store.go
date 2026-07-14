// Package store persists lease.Snapshot values to a single JSON state
// file. Writes are atomic (temp file in the same directory, fsync,
// rename, directory fsync) so a crash mid-write can never leave a
// truncated file — the previous state simply survives. The file is
// created 0600: it contains live fencing tokens, which are capabilities.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JaydenCJ/leasepin/internal/lease"
)

// Save atomically writes snap to path, creating parent directories as
// needed. Output is deterministic: indented JSON with sorted keys and a
// trailing newline, so identical state produces byte-identical files.
func Save(path string, snap *lease.Snapshot) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".leasepin-state-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	// Make the rename itself durable. Some filesystems don't support
	// fsync on directories; that is a best-effort improvement, not a
	// correctness requirement, so errors here are ignored.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Load reads a snapshot from path. A missing file is not an error — it
// yields an empty schema-1 snapshot, which is the correct state for a
// first run. A present-but-unreadable file (truncated, corrupt JSON,
// wrong schema) is an error: silently starting from empty would reset
// every fencing-token floor, exactly the failure fencing exists to
// prevent.
func Load(path string) (*lease.Snapshot, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &lease.Snapshot{SchemaVersion: lease.SchemaVersion, Locks: map[string]lease.LockRecord{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	var snap lease.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("state file %s is corrupt: %w", path, err)
	}
	if snap.SchemaVersion != lease.SchemaVersion {
		return nil, fmt.Errorf("state file %s has schema_version %d, this build supports %d", path, snap.SchemaVersion, lease.SchemaVersion)
	}
	if snap.Locks == nil {
		snap.Locks = map[string]lease.LockRecord{}
	}
	return &snap, nil
}
