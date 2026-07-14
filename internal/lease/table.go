package lease

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// lockState is the per-name record. lastToken outlives any individual
// lease: it is the fencing floor and must never decrease, even while the
// lock is free.
type lockState struct {
	lastToken uint64
	lease     *Lease // nil when free; may be stale (expired) until pruned
}

// Table is the in-memory lock table. All methods are safe for concurrent
// use. If a persist hook is set, every successful mutation is made
// durable before the result is returned to the caller — in particular a
// fencing token is never handed out unless the incremented counter has
// already been persisted, so a crash cannot cause token reuse.
type Table struct {
	mu      sync.Mutex
	now     Clock
	minTTL  time.Duration
	maxTTL  time.Duration
	locks   map[string]*lockState
	persist func(*Snapshot) error
}

// NewTable builds an empty table using the given clock (nil means
// time.Now) and the default TTL bounds.
func NewTable(now Clock) *Table {
	if now == nil {
		now = time.Now
	}
	return &Table{
		now:    now,
		minTTL: DefaultMinTTL,
		maxTTL: DefaultMaxTTL,
		locks:  map[string]*lockState{},
	}
}

// SetTTLBounds overrides the accepted TTL range.
func (t *Table) SetTTLBounds(min, max time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.minTTL, t.maxTTL = min, max
}

// SetPersist installs the durability hook. It is called with a snapshot
// of the whole table, under the table lock, before a mutation is
// acknowledged. A nil hook disables persistence (tests, ephemeral mode).
func (t *Table) SetPersist(fn func(*Snapshot) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.persist = fn
}

func (t *Table) checkTTL(ttl time.Duration) error {
	if ttl < t.minTTL || ttl > t.maxTTL {
		return fmt.Errorf("ttl %s out of range [%s, %s]", ttl, t.minTTL, t.maxTTL)
	}
	return nil
}

// Acquire grants a lease on name if the lock is free or its current
// lease has expired. It returns *HeldError when the lock is validly
// held — including by the same holder: acquire is not reentrant, renew
// is the way to extend.
//
// On success the fencing token is st.lastToken+1 and has been persisted
// before this call returns. If persistence fails, the token is burned
// (the in-memory counter keeps the increment) and the grant is refused,
// so a later retry gets a fresh, still-monotonic token.
func (t *Table) Acquire(name, holder string, ttl time.Duration) (Lease, error) {
	if err := ValidateName(name); err != nil {
		return Lease{}, err
	}
	if err := ValidateHolder(holder); err != nil {
		return Lease{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.checkTTL(ttl); err != nil {
		return Lease{}, err
	}
	now := t.now()
	st := t.locks[name]
	if st == nil {
		st = &lockState{}
		t.locks[name] = st
	}
	t.pruneLocked(st, now)
	if st.lease != nil {
		return Lease{}, &HeldError{Name: name, Holder: st.lease.Holder, ExpiresAt: st.lease.ExpiresAt}
	}
	st.lastToken++
	l := Lease{
		Name:       name,
		Holder:     holder,
		Token:      st.lastToken,
		TTL:        ttl,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
	}
	st.lease = &l
	if err := t.persistLocked(); err != nil {
		// Deliberately keep the counter increment: the token is burned,
		// never reissued, and the next successful persist records the
		// higher floor.
		st.lease = nil
		return Lease{}, fmt.Errorf("lease not granted, state not durable: %w", err)
	}
	return l, nil
}

// Renew extends the caller's live lease by ttl from now. The token never
// changes. Any mismatch — expired, released, wrong holder, stale token,
// or re-granted to someone else — is *GoneError: the caller has lost the
// lock and must stop.
//
// If the persist hook fails the in-memory extension is kept (a shorter
// on-disk deadline only makes the lease expire sooner after a crash,
// which is the safe direction) but an error is returned so the caller
// knows durability is degraded.
func (t *Table) Renew(name, holder string, token uint64, ttl time.Duration) (Lease, error) {
	if err := ValidateName(name); err != nil {
		return Lease{}, err
	}
	if err := ValidateHolder(holder); err != nil {
		return Lease{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.checkTTL(ttl); err != nil {
		return Lease{}, err
	}
	now := t.now()
	st := t.locks[name]
	if st != nil {
		t.pruneLocked(st, now)
	}
	if st == nil || st.lease == nil {
		return Lease{}, &GoneError{Name: name, Reason: "lease expired or was released"}
	}
	if st.lease.Holder != holder || st.lease.Token != token {
		return Lease{}, &GoneError{Name: name, Reason: "lease is no longer yours (holder or token mismatch)"}
	}
	st.lease.TTL = ttl
	st.lease.ExpiresAt = now.Add(ttl)
	l := *st.lease
	if err := t.persistLocked(); err != nil {
		return Lease{}, fmt.Errorf("renewed in memory but state not durable: %w", err)
	}
	return l, nil
}

// Release frees the lock if — and only if — it is still held by exactly
// this holder and token. A stale caller gets *GoneError instead of
// silently unlocking someone else's lease.
func (t *Table) Release(name, holder string, token uint64) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := ValidateHolder(holder); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	st := t.locks[name]
	if st != nil {
		t.pruneLocked(st, now)
	}
	if st == nil || st.lease == nil {
		return &GoneError{Name: name, Reason: "lease expired or was already released"}
	}
	if st.lease.Holder != holder || st.lease.Token != token {
		return &GoneError{Name: name, Reason: "lease is no longer yours (holder or token mismatch)"}
	}
	st.lease = nil
	if err := t.persistLocked(); err != nil {
		return fmt.Errorf("released in memory but state not durable: %w", err)
	}
	return nil
}

// Status is the observable state of one lock name.
type Status struct {
	Name      string
	Held      bool
	LastToken uint64
	Lease     Lease // zero value when not held
}

// Get reports the state of one lock. Unknown names are simply free with
// a zero token floor.
func (t *Table) Get(name string) (Status, error) {
	if err := ValidateName(name); err != nil {
		return Status{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.locks[name]
	if st == nil {
		return Status{Name: name}, nil
	}
	t.pruneLocked(st, t.now())
	s := Status{Name: name, LastToken: st.lastToken}
	if st.lease != nil {
		s.Held = true
		s.Lease = *st.lease
	}
	return s, nil
}

// List returns the currently held locks, sorted by name for
// deterministic output.
func (t *Table) List() []Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var out []Status
	for name, st := range t.locks {
		t.pruneLocked(st, now)
		if st.lease != nil {
			out = append(out, Status{Name: name, Held: true, LastToken: st.lastToken, Lease: *st.lease})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pruneLocked lazily clears an expired lease. The token floor stays.
func (t *Table) pruneLocked(st *lockState, now time.Time) {
	if st.lease != nil && st.lease.Expired(now) {
		st.lease = nil
	}
}

func (t *Table) persistLocked() error {
	if t.persist == nil {
		return nil
	}
	return t.persist(t.snapshotLocked())
}
