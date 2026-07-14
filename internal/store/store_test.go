// Store tests: atomic saves, deterministic bytes, and the load-time
// refusal to start from a corrupt state file (which would reset fencing
// floors).
package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/JaydenCJ/leasepin/internal/lease"
)

func sampleSnapshot() *lease.Snapshot {
	return &lease.Snapshot{
		SchemaVersion: lease.SchemaVersion,
		Locks: map[string]lease.LockRecord{
			"deploy": {
				LastToken:    42,
				Holder:       "ci-7",
				Token:        42,
				TTLMS:        30000,
				AcquiredAtMS: 1780000000000,
				ExpiresAtMS:  1780000030000,
			},
			"cron.backup": {LastToken: 9}, // free lock, floor only
		},
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := sampleSnapshot()
	if err := Save(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.SchemaVersion != want.SchemaVersion || len(got.Locks) != len(want.Locks) {
		t.Fatalf("roundtrip shape wrong: %+v", got)
	}
	if got.Locks["deploy"] != want.Locks["deploy"] {
		t.Fatalf("deploy record = %+v, want %+v", got.Locks["deploy"], want.Locks["deploy"])
	}
	if got.Locks["cron.backup"] != want.Locks["cron.backup"] {
		t.Fatalf("floor-only record = %+v, want %+v", got.Locks["cron.backup"], want.Locks["cron.backup"])
	}
}

func TestLoadMissingFileYieldsEmptySnapshot(t *testing.T) {
	snap, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("missing file should not be an error, got %v", err)
	}
	if snap.SchemaVersion != lease.SchemaVersion || len(snap.Locks) != 0 {
		t.Fatalf("want empty schema-1 snapshot, got %+v", snap)
	}
}

// A present-but-unusable file must fail loudly: silently starting fresh
// would reset every fencing floor. The empty file is the classic crash
// artifact of non-atomic writers — leasepin's own writes can't produce
// it, but load must still refuse it.
func TestLoadRejectsUnusableFiles(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
	}{
		{"corrupt json", []byte("{ this is not json")},
		{"truncated empty file", nil},
		{"future schema version", []byte(`{"schema_version": 2, "locks": {}}`)},
	}
	for _, tc := range cases {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, tc.content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%s: Load accepted it", tc.name)
		}
	}
}

func TestSaveOverwritesInPlaceAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Save(path, sampleSnapshot()); err != nil {
		t.Fatalf("save: %v", err)
	}
	second := &lease.Snapshot{SchemaVersion: lease.SchemaVersion, Locks: map[string]lease.LockRecord{
		"deploy": {LastToken: 43},
	}}
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Locks["deploy"].LastToken != 43 || got.Locks["deploy"].Holder != "" {
		t.Fatalf("overwrite did not take: %+v", got.Locks["deploy"])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory not clean after saves: %v", names)
	}
}

func TestSaveCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")
	if err := Save(path, sampleSnapshot()); err != nil {
		t.Fatalf("save into missing dirs: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("load after nested save: %v", err)
	}
}

// Identical state must produce byte-identical files: the file may be
// checked into debugging archives and diffed across incidents.
func TestSaveIsDeterministicBytes(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.json")
	p2 := filepath.Join(dir, "b.json")
	if err := Save(p1, sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	if err := Save(p2, sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(p1)
	b2, _ := os.ReadFile(p2)
	if !bytes.Equal(b1, b2) {
		t.Fatal("same snapshot produced different bytes")
	}
	if len(b1) == 0 || b1[len(b1)-1] != '\n' {
		t.Fatal("state file should end with a newline")
	}
}

func TestSaveToUnwritableDirectoryFails(t *testing.T) {
	// A regular file where the parent directory should be blocks the save
	// for every euid (unlike chmod 0500, which root ignores).
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(blocker, "state.json"), sampleSnapshot()); err == nil {
		t.Fatal("save under a non-directory parent should fail")
	}
}

func TestSavedFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state file mode %o is group/world accessible; tokens are capabilities", perm)
	}
}
