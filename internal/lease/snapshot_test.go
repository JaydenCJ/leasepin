// Snapshot/Restore tests: the persistence view must carry live leases
// and — critically — the fencing-token floors of free locks, or a
// restart would reset monotonicity.
package lease

import (
	"testing"
	"time"
)

func TestSnapshotRoundTripRestoresLiveLease(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)

	tbl2 := NewTable(clk.now)
	if err := tbl2.Restore(tbl.Snapshot()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := tbl2.Get("deploy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !st.Held || st.Lease.Holder != "ci-1" || st.Lease.Token != l.Token {
		t.Fatalf("restored lease wrong: %+v", st)
	}
	if !st.Lease.ExpiresAt.Equal(l.ExpiresAt.Truncate(time.Millisecond)) {
		t.Fatalf("restored expiry %v, want %v (ms precision)", st.Lease.ExpiresAt, l.ExpiresAt)
	}
}

// The reason snapshots keep free locks: the floor must survive a
// release + restart, so the next grant is still strictly higher.
func TestSnapshotKeepsTokenFloorOfFreeLocks(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	if err := tbl.Release("deploy", "ci-1", l.Token); err != nil {
		t.Fatalf("release: %v", err)
	}

	tbl2 := NewTable(clk.now)
	if err := tbl2.Restore(tbl.Snapshot()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	l2 := mustAcquire(t, tbl2, "deploy", "ci-2", time.Minute)
	if l2.Token <= l.Token {
		t.Fatalf("token after restart %d not above pre-restart %d", l2.Token, l.Token)
	}
}

func TestSnapshotPrunesExpiredLeaseButKeepsFloor(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	clk.advance(2 * time.Minute)
	snap := tbl.Snapshot()
	rec, ok := snap.Locks["deploy"]
	if !ok {
		t.Fatal("expired lock's floor missing from snapshot")
	}
	if rec.Holder != "" || rec.Token != 0 {
		t.Fatalf("expired lease leaked into snapshot: %+v", rec)
	}
	if rec.LastToken != l.Token {
		t.Fatalf("floor = %d, want %d", rec.LastToken, l.Token)
	}
}

// A lease that was live at save time but is past its deadline at load
// time must come back already dead — restart must not resurrect it.
func TestRestoreOfPastDeadlineLeaseReadsAsFree(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	snap := tbl.Snapshot() // lease is live here

	clk.advance(time.Hour) // "server was down for an hour"
	tbl2 := NewTable(clk.now)
	if err := tbl2.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, _ := tbl2.Get("deploy")
	if st.Held {
		t.Fatal("restart resurrected an expired lease")
	}
	if st.LastToken != l.Token {
		t.Fatalf("floor after dead-lease load = %d, want %d", st.LastToken, l.Token)
	}
}

func TestRestoreRejectsUnusableSnapshots(t *testing.T) {
	cases := []struct {
		name string
		snap *Snapshot
	}{
		{"nil snapshot", nil},
		{"wrong schema version", &Snapshot{SchemaVersion: 99, Locks: map[string]LockRecord{}}},
		{"invalid lock name", &Snapshot{SchemaVersion: SchemaVersion, Locks: map[string]LockRecord{
			"bad/name": {LastToken: 1},
		}}},
	}
	for _, tc := range cases {
		tbl, _ := newTestTable(t)
		if err := tbl.Restore(tc.snap); err == nil {
			t.Errorf("%s: Restore accepted it", tc.name)
		}
	}
}

// Self-healing: a hand-edited file where the live token is above its own
// floor must raise the floor rather than let the next grant go backward.
func TestRestoreSelfHealsFloorBelowLiveToken(t *testing.T) {
	tbl, clk := newTestTable(t)
	snap := &Snapshot{SchemaVersion: SchemaVersion, Locks: map[string]LockRecord{
		"deploy": {
			LastToken:    3,
			Holder:       "ci-1",
			Token:        7, // above the recorded floor
			TTLMS:        60000,
			AcquiredAtMS: clk.now().UnixMilli(),
			ExpiresAtMS:  clk.now().Add(time.Minute).UnixMilli(),
		},
	}}
	if err := tbl.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	l := mustAcquire(t, tbl, "deploy2", "x", time.Minute) // unrelated, table works
	_ = l
	if err := tbl.Release("deploy", "ci-1", 7); err != nil {
		t.Fatalf("release restored lease: %v", err)
	}
	next := mustAcquire(t, tbl, "deploy", "ci-2", time.Minute)
	if next.Token <= 7 {
		t.Fatalf("floor did not self-heal: next token %d, want > 7", next.Token)
	}
}

func TestHeldCountCountsOnlyLiveLeases(t *testing.T) {
	tbl, clk := newTestTable(t)
	mustAcquire(t, tbl, "a", "h", time.Minute)
	mustAcquire(t, tbl, "b", "h", time.Second)
	clk.advance(30 * time.Second)
	if n := tbl.HeldCount(); n != 1 {
		t.Fatalf("held count = %d, want 1 (b expired)", n)
	}
}
