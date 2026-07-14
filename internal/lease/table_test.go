// Table tests: the lock state machine under an injected clock — grants,
// conflicts, expiry, and above all the fencing-token invariants that
// make leasepin safe to build on.
package lease

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock; no test in this package ever
// sleeps.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestTable pairs a table with its clock.
func newTestTable(t *testing.T) (*Table, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	return NewTable(clk.now), clk
}

func mustAcquire(t *testing.T, tbl *Table, name, holder string, ttl time.Duration) Lease {
	t.Helper()
	l, err := tbl.Acquire(name, holder, ttl)
	if err != nil {
		t.Fatalf("acquire %s for %s: %v", name, holder, err)
	}
	return l
}

func TestAcquireGrantsFirstLeaseWithTokenOneAndTTLDeadline(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", 90*time.Second)
	if l.Token != 1 {
		t.Fatalf("first token = %d, want 1", l.Token)
	}
	if l.Name != "deploy" || l.Holder != "ci-1" {
		t.Fatalf("lease identity wrong: %+v", l)
	}
	if want := clk.now().Add(90 * time.Second); !l.ExpiresAt.Equal(want) {
		t.Fatalf("expires at %v, want %v", l.ExpiresAt, want)
	}
	if !l.AcquiredAt.Equal(clk.now()) {
		t.Fatalf("acquired at %v, want %v", l.AcquiredAt, clk.now())
	}
}

// A held lock conflicts for everyone — including the same holder:
// acquire is documented as non-reentrant, renew is the way to extend.
func TestAcquireHeldLockConflictsForAnyHolder(t *testing.T) {
	tbl, _ := newTestTable(t)
	first := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	for _, contender := range []string{"ci-2", "ci-1"} {
		_, err := tbl.Acquire("deploy", contender, time.Minute)
		var held *HeldError
		if !errors.As(err, &held) {
			t.Fatalf("acquire by %s: want *HeldError, got %v", contender, err)
		}
		if held.Holder != "ci-1" || !held.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatalf("held error carries wrong lease: %+v", held)
		}
	}
}

func TestAcquireAfterExpirySucceeds(t *testing.T) {
	tbl, clk := newTestTable(t)
	mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	clk.advance(time.Minute) // expiry is inclusive: exactly at deadline is gone
	l := mustAcquire(t, tbl, "deploy", "ci-2", time.Minute)
	if l.Holder != "ci-2" {
		t.Fatalf("holder = %s, want ci-2", l.Holder)
	}
}

// The fencing invariant: tokens strictly increase across release cycles.
func TestTokensStrictlyIncreaseAcrossReleaseAndReacquire(t *testing.T) {
	tbl, _ := newTestTable(t)
	var last uint64
	for i := 0; i < 5; i++ {
		l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
		if l.Token <= last {
			t.Fatalf("token %d not above previous %d", l.Token, last)
		}
		last = l.Token
		if err := tbl.Release("deploy", "ci-1", l.Token); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	if last != 5 {
		t.Fatalf("after 5 cycles last token = %d, want 5", last)
	}
}

// The fencing invariant survives expiry too: a lease that dies of old
// age must never see its token reused by the next grant.
func TestTokensStrictlyIncreaseAcrossExpiry(t *testing.T) {
	tbl, clk := newTestTable(t)
	l1 := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	clk.advance(2 * time.Minute)
	l2 := mustAcquire(t, tbl, "deploy", "ci-2", time.Minute)
	if l2.Token <= l1.Token {
		t.Fatalf("token after expiry %d not above %d", l2.Token, l1.Token)
	}
}

func TestTokensArePerLockNotGlobal(t *testing.T) {
	tbl, _ := newTestTable(t)
	a := mustAcquire(t, tbl, "lock-a", "h", time.Minute)
	b := mustAcquire(t, tbl, "lock-b", "h", time.Minute)
	if a.Token != 1 || b.Token != 1 {
		t.Fatalf("independent locks should each start at 1, got %d and %d", a.Token, b.Token)
	}
}

// Renewal is a heartbeat, not a new acquisition: the deadline moves, the
// token must not.
func TestRenewExtendsExpiryButKeepsToken(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	clk.advance(40 * time.Second)
	nl, err := tbl.Renew("deploy", "ci-1", l.Token, time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if want := clk.now().Add(time.Minute); !nl.ExpiresAt.Equal(want) {
		t.Fatalf("renewed expiry %v, want %v", nl.ExpiresAt, want)
	}
	if nl.Token != l.Token {
		t.Fatalf("renew changed token %d -> %d", l.Token, nl.Token)
	}
}

// Every way a renew can be stale — wrong token, wrong holder, expired
// lease — must come back GoneError, never a silent re-grant.
func TestRenewGoneCases(t *testing.T) {
	cases := []struct {
		name  string
		renew func(tbl *Table, clk *fakeClock, l Lease) error
	}{
		{"stale token", func(tbl *Table, _ *fakeClock, l Lease) error {
			_, err := tbl.Renew("deploy", "ci-1", l.Token+1, time.Minute)
			return err
		}},
		{"wrong holder", func(tbl *Table, _ *fakeClock, l Lease) error {
			_, err := tbl.Renew("deploy", "ci-2", l.Token, time.Minute)
			return err
		}},
		{"after expiry", func(tbl *Table, clk *fakeClock, l Lease) error {
			clk.advance(2 * time.Minute)
			_, err := tbl.Renew("deploy", "ci-1", l.Token, time.Minute)
			return err
		}},
	}
	for _, tc := range cases {
		tbl, clk := newTestTable(t)
		l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
		err := tc.renew(tbl, clk, l)
		var gone *GoneError
		if !errors.As(err, &gone) {
			t.Fatalf("%s: want *GoneError, got %v", tc.name, err)
		}
	}
}

// The classic GC-pause scenario: holder A stalls, its lease expires, B
// acquires, then A wakes up and heartbeats. A must be told it lost.
func TestRenewAfterReacquireByOtherIsGone(t *testing.T) {
	tbl, clk := newTestTable(t)
	la := mustAcquire(t, tbl, "deploy", "worker-a", time.Minute)
	clk.advance(2 * time.Minute)
	lb := mustAcquire(t, tbl, "deploy", "worker-b", time.Minute)
	_, err := tbl.Renew("deploy", "worker-a", la.Token, time.Minute)
	var gone *GoneError
	if !errors.As(err, &gone) {
		t.Fatalf("zombie renew: want *GoneError, got %v", err)
	}
	// And B's lease is untouched by A's failed attempt.
	if _, err := tbl.Renew("deploy", "worker-b", lb.Token, time.Minute); err != nil {
		t.Fatalf("live holder's renew broken by zombie: %v", err)
	}
}

func TestReleaseFreesLock(t *testing.T) {
	tbl, _ := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	if err := tbl.Release("deploy", "ci-1", l.Token); err != nil {
		t.Fatalf("release: %v", err)
	}
	st, err := tbl.Get("deploy")
	if err != nil || st.Held {
		t.Fatalf("lock should be free after release: %+v, %v", st, err)
	}
}

// A stale release — wrong token, wrong holder, or expired — is Gone and
// must never unlock someone else's live lease.
func TestReleaseGoneCases(t *testing.T) {
	cases := []struct {
		name    string
		release func(tbl *Table, clk *fakeClock, l Lease) error
	}{
		{"stale token", func(tbl *Table, _ *fakeClock, l Lease) error {
			return tbl.Release("deploy", "ci-1", l.Token+1)
		}},
		{"wrong holder", func(tbl *Table, _ *fakeClock, l Lease) error {
			return tbl.Release("deploy", "intruder", l.Token)
		}},
		{"after expiry", func(tbl *Table, clk *fakeClock, l Lease) error {
			clk.advance(2 * time.Minute)
			return tbl.Release("deploy", "ci-1", l.Token)
		}},
	}
	for _, tc := range cases {
		tbl, clk := newTestTable(t)
		l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
		err := tc.release(tbl, clk, l)
		var gone *GoneError
		if !errors.As(err, &gone) {
			t.Fatalf("%s: want *GoneError, got %v", tc.name, err)
		}
		if tc.name != "after expiry" {
			// The live lease must survive the bad release attempt.
			if st, _ := tbl.Get("deploy"); !st.Held {
				t.Fatalf("%s: stale release freed the live lease", tc.name)
			}
		}
	}
}

func TestGetReportsUnknownFreeAndHeldStates(t *testing.T) {
	tbl, _ := newTestTable(t)
	st, err := tbl.Get("never-seen")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Held || st.LastToken != 0 {
		t.Fatalf("unknown lock should be free with floor 0: %+v", st)
	}
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	st, err = tbl.Get("deploy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !st.Held || st.Lease.Token != l.Token || st.Lease.Holder != "ci-1" || st.LastToken != l.Token {
		t.Fatalf("get lost lease fields: %+v", st)
	}
}

// After expiry the lock reads as free, but the token floor stays: that
// floor is what the next grant builds on.
func TestGetAfterExpiryReportsFreeWithTokenFloor(t *testing.T) {
	tbl, clk := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	clk.advance(2 * time.Minute)
	st, err := tbl.Get("deploy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Held {
		t.Fatal("expired lease should read as free")
	}
	if st.LastToken != l.Token {
		t.Fatalf("token floor lost on expiry: %d, want %d", st.LastToken, l.Token)
	}
}

func TestListReturnsOnlyHeldLocksSortedByName(t *testing.T) {
	tbl, clk := newTestTable(t)
	mustAcquire(t, tbl, "zeta", "h1", time.Minute)
	mustAcquire(t, tbl, "alpha", "h2", time.Minute)
	expired := mustAcquire(t, tbl, "beta", "h3", time.Second)
	_ = expired
	clk.advance(30 * time.Second) // beta (1s TTL) expires; the others live
	got := tbl.List()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("list = %+v, want [alpha zeta]", got)
	}
}

func TestValidateNameAcceptsGoodAndRejectsBad(t *testing.T) {
	bad := []string{
		"",                       // empty
		strings.Repeat("a", 129), // too long
		"has space",              // whitespace
		"path/traversal",         // slash would break URLs and files
		"ロック",                    // non-ASCII
		"semi;colon",             // shell metacharacter
	}
	for _, name := range bad {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) accepted a bad name", name)
		}
	}
	good := []string{"deploy", "cron.nightly-backup", "team_a-job.2", strings.Repeat("x", 128)}
	for _, name := range good {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q): %v", name, err)
		}
	}
}

func TestValidateHolderAcceptsGoodAndRejectsBad(t *testing.T) {
	bad := []string{"", strings.Repeat("h", 257), "line\nbreak", "tab\there", "nul\x00"}
	for _, h := range bad {
		if err := ValidateHolder(h); err == nil {
			t.Errorf("ValidateHolder(%q) accepted a bad holder", h)
		}
	}
	good := []string{"ci-runner-7", "web01-4242-deadbeef", "holder with spaces"}
	for _, h := range good {
		if err := ValidateHolder(h); err != nil {
			t.Errorf("ValidateHolder(%q): %v", h, err)
		}
	}
}

// TTL bounds: the defaults reject silly values, and SetTTLBounds widens
// them for deployments that know better.
func TestTTLBoundsRejectOutliersAndAreOverridable(t *testing.T) {
	tbl, _ := newTestTable(t)
	if _, err := tbl.Acquire("deploy", "ci-1", time.Millisecond); err == nil {
		t.Fatal("1ms ttl should be rejected by the default bounds")
	}
	if _, err := tbl.Acquire("deploy", "ci-1", 48*time.Hour); err == nil {
		t.Fatal("48h ttl should be rejected by the default bounds")
	}
	tbl.SetTTLBounds(time.Millisecond, 100*time.Hour)
	if _, err := tbl.Acquire("a", "h", time.Millisecond); err != nil {
		t.Fatalf("1ms should pass custom bounds: %v", err)
	}
	if _, err := tbl.Acquire("b", "h", 99*time.Hour); err != nil {
		t.Fatalf("99h should pass custom bounds: %v", err)
	}
}

// Durability ordering: the persist hook must observe the new token
// before Acquire returns it to the caller.
func TestPersistHookSeesTokenBeforeAcquireReturns(t *testing.T) {
	tbl, _ := newTestTable(t)
	var persisted uint64
	tbl.SetPersist(func(s *Snapshot) error {
		persisted = s.Locks["deploy"].Token
		return nil
	})
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	if persisted != l.Token {
		t.Fatalf("persisted token %d != granted token %d", persisted, l.Token)
	}
}

// If persistence fails the grant is refused, and the failed attempt's
// token is burned: the retry must get a strictly higher token so a crash
// between increment and fsync can never hand the same number out twice.
func TestPersistFailureBurnsTokenAndRefusesGrant(t *testing.T) {
	tbl, _ := newTestTable(t)
	failing := true
	tbl.SetPersist(func(*Snapshot) error {
		if failing {
			return errors.New("disk full")
		}
		return nil
	})
	if _, err := tbl.Acquire("deploy", "ci-1", time.Minute); err == nil {
		t.Fatal("acquire should fail when persist fails")
	}
	st, _ := tbl.Get("deploy")
	if st.Held {
		t.Fatal("failed grant must not leave the lock held")
	}
	failing = false
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	if l.Token != 2 {
		t.Fatalf("retry token = %d, want 2 (token 1 burned by the failed persist)", l.Token)
	}
}

func TestRenewPersistFailureReturnsErrorButKeepsLease(t *testing.T) {
	tbl, _ := newTestTable(t)
	l := mustAcquire(t, tbl, "deploy", "ci-1", time.Minute)
	tbl.SetPersist(func(*Snapshot) error { return errors.New("disk full") })
	if _, err := tbl.Renew("deploy", "ci-1", l.Token, time.Minute); err == nil {
		t.Fatal("renew should surface the persist failure")
	}
	tbl.SetPersist(nil)
	st, _ := tbl.Get("deploy")
	if !st.Held {
		t.Fatal("failed renew persist must not drop the live lease")
	}
}
