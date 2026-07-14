// Package server exposes a lease.Table over a small JSON-over-HTTP API.
//
//	POST /v1/locks/{name}/acquire   {holder, ttl_ms}          -> 200 lease | 409 held
//	POST /v1/locks/{name}/renew     {holder, token, ttl_ms}   -> 200 lease | 410 gone
//	POST /v1/locks/{name}/release   {holder, token}           -> 200      | 410 gone
//	GET  /v1/locks/{name}                                     -> 200 status
//	GET  /v1/locks                                            -> 200 held list
//	GET  /v1/healthz                                          -> 200 {ok, version}
//
// There is no blocking acquire on the server: clients poll (the CLI's
// --wait does exactly that), which keeps the server allocation-free of
// parked connections and trivially restartable.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/version"
)

// maxBodyBytes bounds request bodies; every valid request fits in a few
// hundred bytes.
const maxBodyBytes = 1 << 16

// LeaseJSON is the wire form of a granted lease.
type LeaseJSON struct {
	Name         string `json:"name"`
	Holder       string `json:"holder"`
	Token        uint64 `json:"token"`
	TTLMS        int64  `json:"ttl_ms"`
	AcquiredAtMS int64  `json:"acquired_at_unix_ms"`
	ExpiresAtMS  int64  `json:"expires_at_unix_ms"`
}

func toLeaseJSON(l lease.Lease) LeaseJSON {
	return LeaseJSON{
		Name:         l.Name,
		Holder:       l.Holder,
		Token:        l.Token,
		TTLMS:        l.TTL.Milliseconds(),
		AcquiredAtMS: l.AcquiredAt.UnixMilli(),
		ExpiresAtMS:  l.ExpiresAt.UnixMilli(),
	}
}

// StatusJSON is the wire form of GET /v1/locks/{name}.
type StatusJSON struct {
	Name      string     `json:"name"`
	State     string     `json:"state"` // "held" or "free"
	LastToken uint64     `json:"last_token"`
	Lease     *LeaseJSON `json:"lease,omitempty"`
}

func toStatusJSON(s lease.Status) StatusJSON {
	out := StatusJSON{Name: s.Name, State: "free", LastToken: s.LastToken}
	if s.Held {
		out.State = "held"
		l := toLeaseJSON(s.Lease)
		out.Lease = &l
	}
	return out
}

// Server wires a lease.Table to HTTP handlers.
type Server struct {
	table *lease.Table
}

// New builds a Server around table.
func New(table *lease.Table) *Server {
	return &Server{table: table}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/locks", s.handleList)
	mux.HandleFunc("GET /v1/locks/{name}", s.handleGet)
	mux.HandleFunc("POST /v1/locks/{name}/acquire", s.handleAcquire)
	mux.HandleFunc("POST /v1/locks/{name}/renew", s.handleRenew)
	mux.HandleFunc("POST /v1/locks/{name}/release", s.handleRelease)
	// Catch-all: JSON 404s, and JSON 405s for known paths hit with the
	// wrong method (the mux's own 405 only fires when no other pattern —
	// including this catch-all — matches).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if want := methodFor(r.URL.Path); want != "" && want != r.Method {
			w.Header().Set("Allow", want)
			writeError(w, http.StatusMethodNotAllowed, "%s requires %s, got %s", r.URL.Path, want, r.Method)
			return
		}
		writeError(w, http.StatusNotFound, "no such endpoint: %s %s", r.Method, r.URL.Path)
	})
	return mux
}

// methodFor maps a known route shape to its only accepted method, or ""
// for unknown paths.
func methodFor(path string) string {
	switch {
	case path == "/v1/healthz", path == "/v1/locks":
		return http.MethodGet
	case strings.HasPrefix(path, "/v1/locks/"):
		rest := strings.TrimPrefix(path, "/v1/locks/")
		parts := strings.Split(rest, "/")
		switch {
		case len(parts) == 1 && parts[0] != "":
			return http.MethodGet
		case len(parts) == 2 && (parts[1] == "acquire" || parts[1] == "renew" || parts[1] == "release"):
			return http.MethodPost
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]any{"error": fmt.Sprintf(format, args...)})
}

// decodeBody reads a small JSON body into v, rejecting oversized and
// malformed payloads with a 400.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %v", err)
		return false
	}
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version.Version})
}

type acquireRequest struct {
	Holder string `json:"holder"`
	TTLMS  int64  `json:"ttl_ms"`
}

func (s *Server) handleAcquire(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req acquireRequest
	if !decodeBody(w, r, &req) {
		return
	}
	l, err := s.table.Acquire(name, req.Holder, time.Duration(req.TTLMS)*time.Millisecond)
	if err != nil {
		var held *lease.HeldError
		if errors.As(err, &held) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":              "held",
				"name":               held.Name,
				"holder":             held.Holder,
				"expires_at_unix_ms": held.ExpiresAt.UnixMilli(),
			})
			return
		}
		s.writeTableError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLeaseJSON(l))
}

type renewRequest struct {
	Holder string `json:"holder"`
	Token  uint64 `json:"token"`
	TTLMS  int64  `json:"ttl_ms"`
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req renewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	l, err := s.table.Renew(name, req.Holder, req.Token, time.Duration(req.TTLMS)*time.Millisecond)
	if err != nil {
		s.writeTableError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLeaseJSON(l))
}

type releaseRequest struct {
	Holder string `json:"holder"`
	Token  uint64 `json:"token"`
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req releaseRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.table.Release(name, req.Holder, req.Token); err != nil {
		s.writeTableError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true, "name": name})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	st, err := s.table.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, toStatusJSON(st))
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	held := s.table.List()
	out := make([]StatusJSON, 0, len(held))
	for _, st := range held {
		out = append(out, toStatusJSON(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"locks": out, "count": len(out)})
}

// writeTableError maps table errors to HTTP: GoneError -> 410 (the lease
// is lost, stop working), validation -> 400, persistence -> 500.
func (s *Server) writeTableError(w http.ResponseWriter, err error) {
	var gone *lease.GoneError
	if errors.As(err, &gone) {
		writeJSON(w, http.StatusGone, map[string]any{
			"error":  "gone",
			"name":   gone.Name,
			"reason": gone.Reason,
		})
		return
	}
	var held *lease.HeldError
	if errors.As(err, &held) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":              "held",
			"name":               held.Name,
			"holder":             held.Holder,
			"expires_at_unix_ms": held.ExpiresAt.UnixMilli(),
		})
		return
	}
	if isDurabilityError(err) {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeError(w, http.StatusBadRequest, "%v", err)
}

// isDurabilityError distinguishes persist-hook failures (server-side,
// 500) from caller mistakes (400). Table methods wrap persist failures
// with a fixed marker phrase.
func isDurabilityError(err error) bool {
	return strings.Contains(err.Error(), "state not durable")
}
