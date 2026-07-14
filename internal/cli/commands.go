package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/JaydenCJ/leasepin/internal/client"
	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/server"
	"github.com/JaydenCJ/leasepin/internal/store"
	"github.com/JaydenCJ/leasepin/internal/version"
)

// runServe starts the lock server: load state, wire the persist hook,
// listen (loopback by default), and shut down cleanly on SIGINT/SIGTERM.
func runServe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	addr := fs.String("addr", "127.0.0.1:7420", "listen address (keep it loopback unless you know why)")
	statePath := fs.String("state", "leasepin.state.json", "state file for leases and fencing-token floors")
	minTTL := fs.Duration("min-ttl", lease.DefaultMinTTL, "smallest accepted lease TTL")
	maxTTL := fs.Duration("max-ttl", lease.DefaultMaxTTL, "largest accepted lease TTL")
	quiet := fs.Bool("quiet", false, "do not log requests")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "leasepin serve: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	snap, err := store.Load(*statePath)
	if err != nil {
		fmt.Fprintf(stderr, "leasepin serve: %v\n", err)
		fmt.Fprintf(stderr, "refusing to start from an unreadable state file: that would reset fencing-token floors\n")
		return ExitRuntime
	}
	table := lease.NewTable(nil)
	table.SetTTLBounds(*minTTL, *maxTTL)
	if err := table.Restore(snap); err != nil {
		fmt.Fprintf(stderr, "leasepin serve: %v\n", err)
		return ExitRuntime
	}
	table.SetPersist(func(s *lease.Snapshot) error { return store.Save(*statePath, s) })

	handler := server.New(table).Handler()
	if !*quiet {
		handler = logRequests(handler, stderr)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "leasepin serve: %v\n", err)
		return ExitRuntime
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	held := table.HeldCount()
	noun := "leases"
	if held == 1 {
		noun = "lease"
	}
	fmt.Fprintf(stdout, "leasepin %s serving on http://%s (state: %s, %d live %s restored)\n",
		version.Version, ln.Addr(), *statePath, held, noun)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case sig := <-stop:
		fmt.Fprintf(stdout, "leasepin serve: received %s, shutting down\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		return ExitOK
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "leasepin serve: %v\n", err)
			return ExitRuntime
		}
		return ExitOK
	}
}

// logRequests is the serve-mode access log: method, path, status.
func logRequests(next http.Handler, w io.Writer) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		lw := &statusWriter{ResponseWriter: rw, code: http.StatusOK}
		next.ServeHTTP(lw, r)
		fmt.Fprintf(w, "%s %s -> %d\n", r.Method, r.URL.Path, lw.code)
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// leaseText is the human-readable one-liner shared by acquire and renew.
func leaseText(verb string, l client.Lease) string {
	return fmt.Sprintf("%s %s: token %d, holder %s, expires in %s\n",
		verb, l.Name, l.Token, l.Holder, remaining(l.ExpiresAt, time.Now()))
}

func printLease(w io.Writer, format, verb string, l client.Lease) {
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"name":               l.Name,
			"holder":             l.Holder,
			"token":              l.Token,
			"ttl_ms":             l.TTL.Milliseconds(),
			"expires_at_unix_ms": l.ExpiresAt.UnixMilli(),
		})
		return
	}
	fmt.Fprint(w, leaseText(verb, l))
}

func runAcquire(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("acquire", stderr)
	name := fs.String("name", "", "lock name (required)")
	holder := fs.String("holder", "", "holder id (default: host-pid-random)")
	ttl := fs.Duration("ttl", 30*time.Second, "lease TTL")
	wait := fs.Duration("wait", 0, "keep retrying a busy lock for up to this long (0 = fail fast)")
	poll := fs.Duration("poll", time.Second, "retry interval while waiting")
	format := fs.String("format", "text", "output format: text or json")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" {
		fmt.Fprintf(stderr, "leasepin acquire: --name is required\n")
		return ExitUsage
	}
	if *poll <= 0 {
		fmt.Fprintf(stderr, "leasepin acquire: --poll must be positive (got %s)\n", *poll)
		return ExitUsage
	}
	if *holder == "" {
		*holder = defaultHolder()
	}
	cl := client.New(*serverURL)
	l, err := waitLoop(*wait, *poll, time.Now, time.Sleep, func() (client.Lease, error) {
		return cl.Acquire(*name, *holder, *ttl)
	})
	if err != nil {
		var busy *client.BusyError
		if errors.As(err, &busy) {
			fmt.Fprintf(stderr, "leasepin acquire: %v\n", err)
			return ExitBusy
		}
		fmt.Fprintf(stderr, "leasepin acquire: %v\n", err)
		return ExitRuntime
	}
	printLease(stdout, *format, "acquired", l)
	return ExitOK
}

func runRenew(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("renew", stderr)
	name := fs.String("name", "", "lock name (required)")
	holder := fs.String("holder", "", "holder id the lease was granted to (required)")
	token := fs.Uint64("token", 0, "fencing token of the lease (required)")
	ttl := fs.Duration("ttl", 30*time.Second, "new TTL from now")
	format := fs.String("format", "text", "output format: text or json")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" || *holder == "" || *token == 0 {
		fmt.Fprintf(stderr, "leasepin renew: --name, --holder and --token are required\n")
		return ExitUsage
	}
	l, err := client.New(*serverURL).Renew(*name, *holder, *token, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "leasepin renew: %v\n", err)
		var gone *client.GoneError
		if errors.As(err, &gone) {
			return ExitLost
		}
		return ExitRuntime
	}
	printLease(stdout, *format, "renewed", l)
	return ExitOK
}

func runRelease(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("release", stderr)
	name := fs.String("name", "", "lock name (required)")
	holder := fs.String("holder", "", "holder id the lease was granted to (required)")
	token := fs.Uint64("token", 0, "fencing token of the lease (required)")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" || *holder == "" || *token == 0 {
		fmt.Fprintf(stderr, "leasepin release: --name, --holder and --token are required\n")
		return ExitUsage
	}
	if err := client.New(*serverURL).Release(*name, *holder, *token); err != nil {
		fmt.Fprintf(stderr, "leasepin release: %v\n", err)
		var gone *client.GoneError
		if errors.As(err, &gone) {
			return ExitLost
		}
		return ExitRuntime
	}
	fmt.Fprintf(stdout, "released %s\n", *name)
	return ExitOK
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	name := fs.String("name", "", "lock name (required)")
	format := fs.String("format", "text", "output format: text or json")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *name == "" {
		fmt.Fprintf(stderr, "leasepin status: --name is required\n")
		return ExitUsage
	}
	st, err := client.New(*serverURL).Get(*name)
	if err != nil {
		fmt.Fprintf(stderr, "leasepin status: %v\n", err)
		return ExitRuntime
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		out := map[string]any{"name": st.Name, "state": "free", "last_token": st.LastToken}
		if st.Held {
			out["state"] = "held"
			out["holder"] = st.Lease.Holder
			out["token"] = st.Lease.Token
			out["expires_at_unix_ms"] = st.Lease.ExpiresAt.UnixMilli()
		}
		_ = enc.Encode(out)
		return ExitOK
	}
	if st.Held {
		fmt.Fprintf(stdout, "%s: held by %s (token %d, expires in %s)\n",
			st.Name, st.Lease.Holder, st.Lease.Token, remaining(st.Lease.ExpiresAt, time.Now()))
	} else {
		fmt.Fprintf(stdout, "%s: free (last token %d)\n", st.Name, st.LastToken)
	}
	return ExitOK
}

func runList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("list", stderr)
	format := fs.String("format", "text", "output format: text or json")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	locks, err := client.New(*serverURL).List()
	if err != nil {
		fmt.Fprintf(stderr, "leasepin list: %v\n", err)
		return ExitRuntime
	}
	if *format == "json" {
		type row struct {
			Name        string `json:"name"`
			Holder      string `json:"holder"`
			Token       uint64 `json:"token"`
			ExpiresAtMS int64  `json:"expires_at_unix_ms"`
		}
		rows := make([]row, 0, len(locks))
		for _, st := range locks {
			rows = append(rows, row{st.Name, st.Lease.Holder, st.Lease.Token, st.Lease.ExpiresAt.UnixMilli()})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"locks": rows, "count": len(rows)})
		return ExitOK
	}
	if len(locks) == 0 {
		fmt.Fprintf(stdout, "no locks held\n")
		return ExitOK
	}
	now := time.Now()
	fmt.Fprintf(stdout, "%-24s %-8s %-32s %s\n", "NAME", "TOKEN", "HOLDER", "EXPIRES")
	for _, st := range locks {
		fmt.Fprintf(stdout, "%-24s %-8d %-32s %s\n",
			st.Name, st.Lease.Token, st.Lease.Holder, remaining(st.Lease.ExpiresAt, now))
	}
	return ExitOK
}
