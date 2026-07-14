package lease

import (
	"fmt"
	"time"
)

// SchemaVersion is the on-disk state format version.
const SchemaVersion = 1

// Snapshot is the serializable view of the whole table. Free locks with
// a non-zero token floor are included on purpose: the floor is what
// guarantees fencing tokens stay monotonic across restarts, so it must
// survive even when no lease is live. Timestamps are Unix milliseconds
// so the file is portable and diff-friendly.
type Snapshot struct {
	SchemaVersion int                   `json:"schema_version"`
	Locks         map[string]LockRecord `json:"locks"`
}

// LockRecord is one lock name in a snapshot. The lease fields are only
// set while a lease is live at snapshot time.
type LockRecord struct {
	LastToken    uint64 `json:"last_token"`
	Holder       string `json:"holder,omitempty"`
	Token        uint64 `json:"token,omitempty"`
	TTLMS        int64  `json:"ttl_ms,omitempty"`
	AcquiredAtMS int64  `json:"acquired_at_unix_ms,omitempty"`
	ExpiresAtMS  int64  `json:"expires_at_unix_ms,omitempty"`
}

// Snapshot returns a consistent copy of the table state. Expired leases
// are pruned (their floors kept), so a snapshot never resurrects a dead
// lease with more authority than it had in memory.
func (t *Table) Snapshot() *Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Table) snapshotLocked() *Snapshot {
	now := t.now()
	snap := &Snapshot{SchemaVersion: SchemaVersion, Locks: map[string]LockRecord{}}
	for name, st := range t.locks {
		t.pruneLocked(st, now)
		rec := LockRecord{LastToken: st.lastToken}
		if st.lease != nil {
			rec.Holder = st.lease.Holder
			rec.Token = st.lease.Token
			rec.TTLMS = st.lease.TTL.Milliseconds()
			rec.AcquiredAtMS = st.lease.AcquiredAt.UnixMilli()
			rec.ExpiresAtMS = st.lease.ExpiresAt.UnixMilli()
		}
		if rec.LastToken == 0 && st.lease == nil {
			continue // never granted: nothing worth persisting
		}
		snap.Locks[name] = rec
	}
	return snap
}

// Restore replaces the table contents from a snapshot, typically at
// server start. Leases whose deadline is already in the past are loaded
// and then lazily pruned like any other expired lease. A record whose
// live token is above its own floor self-heals by raising the floor —
// monotonicity always wins over a malformed file.
func (t *Table) Restore(snap *Snapshot) error {
	if snap == nil {
		return fmt.Errorf("nil snapshot")
	}
	if snap.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported state schema_version %d (want %d)", snap.SchemaVersion, SchemaVersion)
	}
	locks := map[string]*lockState{}
	for name, rec := range snap.Locks {
		if err := ValidateName(name); err != nil {
			return fmt.Errorf("state file lock %q: %w", name, err)
		}
		st := &lockState{lastToken: rec.LastToken}
		if rec.Holder != "" {
			if err := ValidateHolder(rec.Holder); err != nil {
				return fmt.Errorf("state file lock %q: %w", name, err)
			}
			if rec.Token > st.lastToken {
				st.lastToken = rec.Token
			}
			l := Lease{
				Name:       name,
				Holder:     rec.Holder,
				Token:      rec.Token,
				TTL:        time.Duration(rec.TTLMS) * time.Millisecond,
				AcquiredAt: time.UnixMilli(rec.AcquiredAtMS),
				ExpiresAt:  time.UnixMilli(rec.ExpiresAtMS),
			}
			st.lease = &l
		}
		locks[name] = st
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.locks = locks
	return nil
}

// HeldCount reports how many live leases the table currently has; used
// by the serve banner after a restore.
func (t *Table) HeldCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	n := 0
	for _, st := range t.locks {
		t.pruneLocked(st, now)
		if st.lease != nil {
			n++
		}
	}
	return n
}
