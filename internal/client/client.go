// Package client is the Go HTTP client for a leasepin server. It maps
// the two lock-protocol outcomes that callers must branch on — the lock
// is busy (retry later) and the lease is gone (stop working) — to typed
// errors, so CLI code and the withlock runner never parse status codes.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultServer is where the CLI looks for a server when neither
// --server nor LEASEPIN_SERVER says otherwise.
const DefaultServer = "http://127.0.0.1:7420"

// Lease is a granted lease as reported by the server.
type Lease struct {
	Name       string
	Holder     string
	Token      uint64
	TTL        time.Duration
	AcquiredAt time.Time
	ExpiresAt  time.Time
}

// Status is the state of one lock.
type Status struct {
	Name      string
	Held      bool
	LastToken uint64
	Lease     Lease // zero value when free
}

// BusyError: the lock is validly held by someone else. Retry later.
type BusyError struct {
	Name      string
	Holder    string
	ExpiresAt time.Time
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("lock %q is held by %q until %s", e.Name, e.Holder, e.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoneError: the caller's lease is lost — expired, released, or granted
// to someone else. The caller must stop treating the resource as owned.
type GoneError struct {
	Name   string
	Reason string
}

func (e *GoneError) Error() string {
	return fmt.Sprintf("lease on %q is gone: %s", e.Name, e.Reason)
}

// Client talks to one leasepin server.
type Client struct {
	base string
	http *http.Client
}

// New builds a client for base (e.g. "http://127.0.0.1:7420"). A short
// timeout is set: every leasepin call is a single small round-trip on a
// local or LAN link, so anything slow is already a failure.
func New(base string) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

type wireLease struct {
	Name         string `json:"name"`
	Holder       string `json:"holder"`
	Token        uint64 `json:"token"`
	TTLMS        int64  `json:"ttl_ms"`
	AcquiredAtMS int64  `json:"acquired_at_unix_ms"`
	ExpiresAtMS  int64  `json:"expires_at_unix_ms"`
}

func (w wireLease) lease() Lease {
	return Lease{
		Name:       w.Name,
		Holder:     w.Holder,
		Token:      w.Token,
		TTL:        time.Duration(w.TTLMS) * time.Millisecond,
		AcquiredAt: time.UnixMilli(w.AcquiredAtMS),
		ExpiresAt:  time.UnixMilli(w.ExpiresAtMS),
	}
}

type wireError struct {
	Error       string `json:"error"`
	Name        string `json:"name"`
	Holder      string `json:"holder"`
	Reason      string `json:"reason"`
	ExpiresAtMS int64  `json:"expires_at_unix_ms"`
}

// do performs one JSON round-trip and decodes 409/410 into typed errors.
func (c *Client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("leasepin server unreachable at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	var we wireError
	_ = json.Unmarshal(data, &we)
	switch resp.StatusCode {
	case http.StatusConflict:
		return &BusyError{Name: we.Name, Holder: we.Holder, ExpiresAt: time.UnixMilli(we.ExpiresAtMS)}
	case http.StatusGone:
		return &GoneError{Name: we.Name, Reason: we.Reason}
	default:
		msg := we.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, msg)
	}
}

// Acquire requests a lease on name. Returns *BusyError when held.
func (c *Client) Acquire(name, holder string, ttl time.Duration) (Lease, error) {
	var w wireLease
	err := c.do(http.MethodPost, "/v1/locks/"+name+"/acquire",
		map[string]any{"holder": holder, "ttl_ms": ttl.Milliseconds()}, &w)
	if err != nil {
		return Lease{}, err
	}
	return w.lease(), nil
}

// Renew extends a lease. Returns *GoneError when the lease is lost.
func (c *Client) Renew(name, holder string, token uint64, ttl time.Duration) (Lease, error) {
	var w wireLease
	err := c.do(http.MethodPost, "/v1/locks/"+name+"/renew",
		map[string]any{"holder": holder, "token": token, "ttl_ms": ttl.Milliseconds()}, &w)
	if err != nil {
		return Lease{}, err
	}
	return w.lease(), nil
}

// Release frees a lease. Returns *GoneError when it was already lost.
func (c *Client) Release(name, holder string, token uint64) error {
	return c.do(http.MethodPost, "/v1/locks/"+name+"/release",
		map[string]any{"holder": holder, "token": token}, nil)
}

type wireStatus struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`
	LastToken uint64     `json:"last_token"`
	Lease     *wireLease `json:"lease"`
}

func (w wireStatus) status() Status {
	s := Status{Name: w.Name, Held: w.State == "held", LastToken: w.LastToken}
	if w.Lease != nil {
		s.Lease = w.Lease.lease()
	}
	return s
}

// Get reports the state of one lock.
func (c *Client) Get(name string) (Status, error) {
	var w wireStatus
	if err := c.do(http.MethodGet, "/v1/locks/"+name, nil, &w); err != nil {
		return Status{}, err
	}
	return w.status(), nil
}

// List returns all currently held locks, sorted by name.
func (c *Client) List() ([]Status, error) {
	var w struct {
		Locks []wireStatus `json:"locks"`
	}
	if err := c.do(http.MethodGet, "/v1/locks", nil, &w); err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(w.Locks))
	for _, ws := range w.Locks {
		out = append(out, ws.status())
	}
	return out, nil
}

// Health checks the server and returns its reported version.
func (c *Client) Health() (string, error) {
	var w struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := c.do(http.MethodGet, "/v1/healthz", nil, &w); err != nil {
		return "", err
	}
	if !w.OK {
		return "", fmt.Errorf("server reports not ok")
	}
	return w.Version, nil
}
