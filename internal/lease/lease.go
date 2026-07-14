// Package lease implements the core lock table: named locks held as
// leases with TTLs and per-lock monotonic fencing tokens.
//
// The package is pure state-machine code: the clock is injected, expiry
// is evaluated lazily on access, and persistence happens through a hook
// so the table itself never touches the filesystem. That keeps every
// rule — token monotonicity, expiry, holder checks — unit-testable
// without timers or disk.
package lease

import (
	"fmt"
	"time"
)

// Validation limits and TTL bounds. TTL bounds are defaults; the table
// owner (the server) can override them per instance.
const (
	MaxNameLen    = 128
	MaxHolderLen  = 256
	DefaultMinTTL = 100 * time.Millisecond
	DefaultMaxTTL = 24 * time.Hour
)

// Clock returns the current time. Injected so tests control expiry.
type Clock func() time.Time

// Lease is one granted hold on a named lock.
//
// Token is the fencing token: a per-lock counter that strictly increases
// with every grant and is never reused — not after release, not after
// expiry, and not after a server restart (the counter floor is
// persisted). Renewals extend ExpiresAt but never change Token: the
// token identifies the acquisition, not the heartbeat.
type Lease struct {
	Name       string
	Holder     string
	Token      uint64
	TTL        time.Duration
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// Expired reports whether the lease is no longer valid at now. Expiry is
// inclusive: a lease is gone at exactly its deadline.
func (l Lease) Expired(now time.Time) bool {
	return !now.Before(l.ExpiresAt)
}

// HeldError is returned by Acquire when the lock is validly held by a
// live lease. It deliberately carries the current holder and deadline —
// both observable via the status endpoint anyway — but never the token:
// tokens are only ever handed to the holder they were granted to.
type HeldError struct {
	Name      string
	Holder    string
	ExpiresAt time.Time
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("lock %q is held by %q until %s", e.Name, e.Holder, e.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoneError is returned by Renew and Release when the caller's lease is
// no longer the live lease: it expired, was released, or the lock has
// since been granted to someone else. The caller must stop treating the
// resource as owned.
type GoneError struct {
	Name   string
	Reason string
}

func (e *GoneError) Error() string {
	return fmt.Sprintf("lease on %q is gone: %s", e.Name, e.Reason)
}

// ValidateName checks a lock name: 1..MaxNameLen characters from
// [A-Za-z0-9._-]. The charset keeps names safe in URLs, filenames, and
// log lines without any escaping.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("lock name is empty")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("lock name exceeds %d bytes", MaxNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("lock name contains invalid byte %q (allowed: A-Z a-z 0-9 . _ -)", c)
		}
	}
	return nil
}

// ValidateHolder checks a holder id: 1..MaxHolderLen bytes, printable
// ASCII (0x21..0x7E) plus spaces, so holder ids read cleanly in status
// output and logs.
func ValidateHolder(holder string) error {
	if holder == "" {
		return fmt.Errorf("holder is empty")
	}
	if len(holder) > MaxHolderLen {
		return fmt.Errorf("holder exceeds %d bytes", MaxHolderLen)
	}
	for i := 0; i < len(holder); i++ {
		c := holder[i]
		if c < 0x20 || c > 0x7E {
			return fmt.Errorf("holder contains non-printable byte 0x%02x", c)
		}
	}
	return nil
}
