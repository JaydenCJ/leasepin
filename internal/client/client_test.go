// Client tests: round-trips against an in-process httptest server, and
// the typed-error mapping (409 -> BusyError, 410 -> GoneError) that the
// CLI and withlock branch on.
package client

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/server"
	"github.com/JaydenCJ/leasepin/internal/version"
)

func newTestPair(t *testing.T) *Client {
	t.Helper()
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	t.Cleanup(ts.Close)
	return New(ts.URL)
}

func TestAcquireRoundTrip(t *testing.T) {
	cl := newTestPair(t)
	l, err := cl.Acquire("deploy", "ci-1", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if l.Name != "deploy" || l.Holder != "ci-1" || l.Token != 1 || l.TTL != 30*time.Second {
		t.Fatalf("lease = %+v", l)
	}
	if !l.ExpiresAt.After(l.AcquiredAt) {
		t.Fatalf("deadline not after acquisition: %+v", l)
	}
}

func TestAcquireBusyMapsToBusyError(t *testing.T) {
	cl := newTestPair(t)
	if _, err := cl.Acquire("deploy", "ci-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err := cl.Acquire("deploy", "ci-2", time.Minute)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("want *BusyError, got %v", err)
	}
	if busy.Name != "deploy" || busy.Holder != "ci-1" {
		t.Fatalf("busy error fields wrong: %+v", busy)
	}
}

func TestRenewRoundTripKeepsTokenAndGoneMapsToGoneError(t *testing.T) {
	cl := newTestPair(t)
	l, err := cl.Acquire("deploy", "ci-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	nl, err := cl.Renew("deploy", "ci-1", l.Token, 2*time.Minute)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if nl.Token != l.Token || nl.TTL != 2*time.Minute {
		t.Fatalf("renewed lease = %+v", nl)
	}
	_, err = cl.Renew("deploy", "ci-1", l.Token+99, time.Minute)
	var gone *GoneError
	if !errors.As(err, &gone) {
		t.Fatalf("want *GoneError, got %v", err)
	}
	if gone.Name != "deploy" || gone.Reason == "" {
		t.Fatalf("gone error fields wrong: %+v", gone)
	}
}

func TestReleaseRoundTripAndGoneMapsToGoneError(t *testing.T) {
	cl := newTestPair(t)
	l, err := cl.Acquire("deploy", "ci-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Release("deploy", "ci-1", l.Token); err != nil {
		t.Fatalf("release: %v", err)
	}
	st, err := cl.Get("deploy")
	if err != nil || st.Held {
		t.Fatalf("after release: %+v, %v", st, err)
	}
	var gone *GoneError
	if err := cl.Release("deploy", "ci-1", l.Token); !errors.As(err, &gone) {
		t.Fatalf("double release: want *GoneError, got %v", err)
	}
}

func TestGetReportsHeldState(t *testing.T) {
	cl := newTestPair(t)
	l, err := cl.Acquire("deploy", "ci-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	st, err := cl.Get("deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Held || st.Lease.Token != l.Token || st.LastToken != l.Token {
		t.Fatalf("status = %+v", st)
	}
}

func TestListReturnsSortedHeldLocks(t *testing.T) {
	cl := newTestPair(t)
	if _, err := cl.Acquire("zeta", "h", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Acquire("alpha", "h", time.Minute); err != nil {
		t.Fatal(err)
	}
	locks, err := cl.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || locks[0].Name != "alpha" || locks[1].Name != "zeta" {
		t.Fatalf("list = %+v", locks)
	}
}

func TestHealthReturnsServerVersion(t *testing.T) {
	cl := newTestPair(t)
	v, err := cl.Health()
	if err != nil {
		t.Fatal(err)
	}
	if v != version.Version {
		t.Fatalf("health version = %q, want %q", v, version.Version)
	}
}

func TestValidationErrorSurfacesStatusAndMessage(t *testing.T) {
	cl := newTestPair(t)
	_, err := cl.Acquire("deploy", "", time.Minute)
	if err == nil {
		t.Fatal("empty holder should fail")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "holder") {
		t.Fatalf("error should carry status and server message, got: %v", err)
	}
}

// A closed server port fails fast with an "unreachable" error that names
// the base URL — the message operators see when they forgot to start
// `leasepin serve`.
func TestUnreachableServerErrorNamesTheBaseURL(t *testing.T) {
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	url := ts.URL
	ts.Close() // port is now closed: connection refused, no timeout
	cl := New(url)
	_, err := cl.Acquire("deploy", "ci-1", time.Minute)
	if err == nil {
		t.Fatal("acquire against a closed port should fail")
	}
	if !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), url) {
		t.Fatalf("error should say unreachable and name the URL: %v", err)
	}
}

func TestClientToleratesTrailingSlashInBaseURL(t *testing.T) {
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	t.Cleanup(ts.Close)
	cl := New(ts.URL + "/")
	if _, err := cl.Acquire("deploy", "ci-1", time.Minute); err != nil {
		t.Fatalf("trailing slash base should work: %v", err)
	}
}
