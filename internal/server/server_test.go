// Server tests: the JSON API over httptest (loopback, in-process) —
// status codes, response shapes, the no-token-leak rule for conflicts,
// and token-floor persistence across a simulated restart.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/store"
	"github.com/JaydenCJ/leasepin/internal/version"
)

// fakeClock mirrors the lease-package test clock so server tests can
// force expiry without sleeping.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestServer(t *testing.T) (*httptest.Server, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	ts := httptest.NewServer(New(lease.NewTable(clk.now)).Handler())
	t.Cleanup(ts.Close)
	return ts, clk
}

// call performs one JSON request and decodes the body into a map.
func call(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("response is not JSON (%d): %s", resp.StatusCode, data)
		}
	}
	return resp.StatusCode, out
}

func acquireBody(holder string, ttl time.Duration) map[string]any {
	return map[string]any{"holder": holder, "ttl_ms": ttl.Milliseconds()}
}

func TestHealthzReportsOKAndVersion(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, ts.URL+"/v1/healthz", nil)
	if code != http.StatusOK || body["ok"] != true || body["version"] != version.Version {
		t.Fatalf("healthz = %d %v", code, body)
	}
}

func TestAcquireReturnsLeaseJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", 30*time.Second))
	if code != http.StatusOK {
		t.Fatalf("acquire = %d %v", code, body)
	}
	if body["name"] != "deploy" || body["holder"] != "ci-1" || body["token"] != float64(1) {
		t.Fatalf("lease json wrong: %v", body)
	}
	if body["ttl_ms"] != float64(30000) {
		t.Fatalf("ttl_ms = %v, want 30000", body["ttl_ms"])
	}
	if _, ok := body["expires_at_unix_ms"]; !ok {
		t.Fatalf("expires_at_unix_ms missing: %v", body)
	}
}

// Conflict responses carry the holder and deadline but never the live
// token: tokens are only ever returned to the holder they were granted
// to.
func TestAcquireConflictIs409WithHolderAndNoTokenLeak(t *testing.T) {
	ts, _ := newTestServer(t)
	call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-2", time.Minute))
	if code != http.StatusConflict {
		t.Fatalf("second acquire = %d, want 409", code)
	}
	if body["error"] != "held" || body["holder"] != "ci-1" {
		t.Fatalf("conflict body wrong: %v", body)
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("conflict response leaked the token: %v", body)
	}
	if _, ok := body["expires_at_unix_ms"]; !ok {
		t.Fatalf("conflict should tell callers when to retry: %v", body)
	}
}

// Every malformed acquire is a 400: bad lock name, missing holder,
// broken JSON, an unknown field (catches ttl-vs-ttl_ms typos), and an
// out-of-range TTL.
func TestAcquireBadRequestsAre400(t *testing.T) {
	ts, _ := newTestServer(t)
	jsonCases := []struct {
		name string
		url  string
		body any
	}{
		{"invalid lock name", ts.URL + "/v1/locks/bad%20name/acquire", acquireBody("ci-1", time.Minute)},
		{"missing holder", ts.URL + "/v1/locks/deploy/acquire", map[string]any{"ttl_ms": 60000}},
		{"unknown field", ts.URL + "/v1/locks/deploy/acquire", map[string]any{"holder": "ci-1", "ttl_ms": 60000, "ttl": "30s"}},
		{"ttl below minimum", ts.URL + "/v1/locks/deploy/acquire", acquireBody("ci-1", time.Millisecond)},
	}
	for _, tc := range jsonCases {
		if code, body := call(t, http.MethodPost, tc.url, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", tc.name, code, body)
		}
	}
	resp, err := http.Post(ts.URL+"/v1/locks/deploy/acquire", "application/json", strings.NewReader("{nope"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed json = %d, want 400", resp.StatusCode)
	}
}

func TestRenewExtendsLease(t *testing.T) {
	ts, clk := newTestServer(t)
	_, l := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	clk.advance(30 * time.Second)
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/renew",
		map[string]any{"holder": "ci-1", "token": l["token"], "ttl_ms": 60000})
	if code != http.StatusOK {
		t.Fatalf("renew = %d %v", code, body)
	}
	if body["expires_at_unix_ms"].(float64) <= l["expires_at_unix_ms"].(float64) {
		t.Fatal("renew did not push the deadline forward")
	}
	if body["token"] != l["token"] {
		t.Fatalf("renew changed the token: %v -> %v", l["token"], body["token"])
	}
}

func TestRenewStaleTokenIs410(t *testing.T) {
	ts, _ := newTestServer(t)
	call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/renew",
		map[string]any{"holder": "ci-1", "token": 999, "ttl_ms": 60000})
	if code != http.StatusGone || body["error"] != "gone" {
		t.Fatalf("stale renew = %d %v, want 410 gone", code, body)
	}
}

func TestReleaseFreesLock(t *testing.T) {
	ts, _ := newTestServer(t)
	_, l := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/release",
		map[string]any{"holder": "ci-1", "token": l["token"]})
	if code != http.StatusOK || body["released"] != true {
		t.Fatalf("release = %d %v", code, body)
	}
	_, st := call(t, http.MethodGet, ts.URL+"/v1/locks/deploy", nil)
	if st["state"] != "free" {
		t.Fatalf("after release state = %v", st["state"])
	}
}

func TestReleaseStaleIs410(t *testing.T) {
	ts, _ := newTestServer(t)
	code, _ := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/release",
		map[string]any{"holder": "nobody", "token": 1})
	if code != http.StatusGone {
		t.Fatalf("release of unheld lock = %d, want 410", code)
	}
}

func TestGetReportsFreeAndHeldShapes(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, ts.URL+"/v1/locks/never-used", nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d", code)
	}
	if body["state"] != "free" || body["last_token"] != float64(0) {
		t.Fatalf("free shape wrong: %v", body)
	}
	if _, ok := body["lease"]; ok {
		t.Fatalf("free lock should omit lease: %v", body)
	}
	call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	_, body = call(t, http.MethodGet, ts.URL+"/v1/locks/deploy", nil)
	if body["state"] != "held" || body["last_token"] != float64(1) {
		t.Fatalf("held shape wrong: %v", body)
	}
	l, ok := body["lease"].(map[string]any)
	if !ok || l["holder"] != "ci-1" || l["token"] != float64(1) {
		t.Fatalf("held lease wrong: %v", body["lease"])
	}
}

func TestListReturnsHeldLocksSorted(t *testing.T) {
	ts, _ := newTestServer(t)
	call(t, http.MethodPost, ts.URL+"/v1/locks/zeta/acquire", acquireBody("h", time.Minute))
	call(t, http.MethodPost, ts.URL+"/v1/locks/alpha/acquire", acquireBody("h", time.Minute))
	code, body := call(t, http.MethodGet, ts.URL+"/v1/locks", nil)
	if code != http.StatusOK || body["count"] != float64(2) {
		t.Fatalf("list = %d %v", code, body)
	}
	locks := body["locks"].([]any)
	first := locks[0].(map[string]any)
	second := locks[1].(map[string]any)
	if first["name"] != "alpha" || second["name"] != "zeta" {
		t.Fatalf("list order wrong: %v", locks)
	}
}

func TestUnknownRouteIs404JSONAndWrongMethodIs405(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := call(t, http.MethodGet, ts.URL+"/v2/whatever", nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", code)
	}
	if _, ok := body["error"]; !ok {
		t.Fatalf("404 should be JSON with an error field: %v", body)
	}
	resp, err := http.Get(ts.URL + "/v1/locks/deploy/acquire")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on acquire = %d, want 405", resp.StatusCode)
	}
	if resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("405 should set Allow: POST, got %q", resp.Header.Get("Allow"))
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	big := fmt.Sprintf(`{"holder": %q, "ttl_ms": 60000}`, strings.Repeat("x", 1<<17))
	resp, err := http.Post(ts.URL+"/v1/locks/deploy/acquire", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body = %d, want 400", resp.StatusCode)
	}
}

func TestAcquireAfterExpiryViaClock(t *testing.T) {
	ts, clk := newTestServer(t)
	call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Second))
	clk.advance(2 * time.Second)
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-2", time.Minute))
	if code != http.StatusOK || body["holder"] != "ci-2" {
		t.Fatalf("acquire after expiry = %d %v", code, body)
	}
	if body["token"].(float64) <= 1 {
		t.Fatalf("token after expiry = %v, want > 1", body["token"])
	}
}

// The whole point of the state file: stop a server, start a fresh one on
// the same file, and the fencing floor carries over so tokens keep
// increasing.
func TestRestartOnSameStateFilePreservesTokenFloor(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	clk := newFakeClock()

	// First server lifetime: grant token 1, then release.
	table1 := lease.NewTable(clk.now)
	table1.SetPersist(func(s *lease.Snapshot) error { return store.Save(statePath, s) })
	ts1 := httptest.NewServer(New(table1).Handler())
	_, l := call(t, http.MethodPost, ts1.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	call(t, http.MethodPost, ts1.URL+"/v1/locks/deploy/release", map[string]any{"holder": "ci-1", "token": l["token"]})
	ts1.Close()

	// Second lifetime on the same file.
	snap, err := store.Load(statePath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	table2 := lease.NewTable(clk.now)
	if err := table2.Restore(snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	table2.SetPersist(func(s *lease.Snapshot) error { return store.Save(statePath, s) })
	ts2 := httptest.NewServer(New(table2).Handler())
	defer ts2.Close()

	code, body := call(t, http.MethodPost, ts2.URL+"/v1/locks/deploy/acquire", acquireBody("ci-2", time.Minute))
	if code != http.StatusOK {
		t.Fatalf("acquire after restart = %d %v", code, body)
	}
	if body["token"].(float64) <= l["token"].(float64) {
		t.Fatalf("restart reset the floor: token %v after %v", body["token"], l["token"])
	}
}

// A live lease must survive the restart and still refuse intruders.
func TestRestartOnSameStateFileKeepsLiveLeaseHeld(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	clk := newFakeClock()

	table1 := lease.NewTable(clk.now)
	table1.SetPersist(func(s *lease.Snapshot) error { return store.Save(statePath, s) })
	ts1 := httptest.NewServer(New(table1).Handler())
	call(t, http.MethodPost, ts1.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Hour))
	ts1.Close()

	snap, err := store.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	table2 := lease.NewTable(clk.now)
	if err := table2.Restore(snap); err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(New(table2).Handler())
	defer ts2.Close()

	code, body := call(t, http.MethodPost, ts2.URL+"/v1/locks/deploy/acquire", acquireBody("ci-2", time.Minute))
	if code != http.StatusConflict || body["holder"] != "ci-1" {
		t.Fatalf("lease did not survive restart: %d %v", code, body)
	}
}

// Persist failures must refuse the grant with a 500, not hand out a
// token that would vanish on crash.
func TestPersistFailureRefusesGrantWith500(t *testing.T) {
	clk := newFakeClock()
	table := lease.NewTable(clk.now)
	table.SetPersist(func(*lease.Snapshot) error { return fmt.Errorf("disk full") })
	ts := httptest.NewServer(New(table).Handler())
	defer ts.Close()
	code, body := call(t, http.MethodPost, ts.URL+"/v1/locks/deploy/acquire", acquireBody("ci-1", time.Minute))
	if code != http.StatusInternalServerError {
		t.Fatalf("acquire with broken persist = %d %v, want 500", code, body)
	}
}
