// Package cli implements the leasepin command-line interface. Run takes
// argv and two writers and returns an exit code, so the whole surface is
// testable in-process without building a binary.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/JaydenCJ/leasepin/internal/client"
	"github.com/JaydenCJ/leasepin/internal/version"
)

// Exit codes. The lock-specific ones (10, 11) are deliberately far away
// from common shell and child-process codes so wrappers can branch on
// them: `leasepin withlock` passes the wrapped command's own exit code
// through unchanged.
const (
	ExitOK      = 0
	ExitUsage   = 2
	ExitRuntime = 3
	ExitBusy    = 10 // lock is held by someone else (and --wait ran out)
	ExitLost    = 11 // lease was lost mid-run; the wrapped command was stopped
)

// Run dispatches argv and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return ExitOK
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "acquire":
		return runAcquire(args[1:], stdout, stderr)
	case "renew":
		return runRenew(args[1:], stdout, stderr)
	case "release":
		return runRelease(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "withlock":
		return runWithlock(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "leasepin %s\n", version.Version)
		return ExitOK
	case "help", "--help", "-h":
		usage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "leasepin: unknown command %q\n\n", args[0])
		usage(stderr)
		return ExitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `leasepin — HTTP lock service with leases and fencing tokens.

Usage:
  leasepin <command> [flags] [args]

Commands:
  serve     run the lock server (file-persisted, binds 127.0.0.1)
  withlock  acquire a lock, run a command under it, renew, release
  acquire   take a lease and print its fencing token
  renew     extend a lease you hold
  release   free a lease you hold
  status    show one lock (held/free, holder, token, deadline)
  list      show all currently held locks
  version   print the version

Exit codes:
  0 ok · 2 usage error · 3 runtime error · 10 lock busy · 11 lease lost

Run "leasepin <command> -h" for the flags of each command.
`)
}

// newFlagSet builds a silent FlagSet whose usage goes to stderr and
// whose parse errors map to ExitUsage in the callers.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// serverFlag registers --server with its env-var fallback chain.
func serverFlag(fs *flag.FlagSet) *string {
	def := os.Getenv("LEASEPIN_SERVER")
	if def == "" {
		def = client.DefaultServer
	}
	return fs.String("server", def, "leasepin server base URL (env LEASEPIN_SERVER)")
}

// defaultHolder builds a holder id that is unique enough to never
// collide across hosts and runs, yet readable in `leasepin list`.
func defaultHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// Extremely unlikely; fall back to pid-only uniqueness.
		return fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(buf))
}

// waitLoop tries fn until it succeeds, the lock stays busy past the wait
// budget, or a non-busy error occurs. now and sleep are injected so
// tests drive the loop without wall-clock time.
func waitLoop(wait, poll time.Duration, now func() time.Time, sleep func(time.Duration),
	fn func() (client.Lease, error)) (client.Lease, error) {
	deadline := now().Add(wait)
	for {
		l, err := fn()
		if err == nil {
			return l, nil
		}
		var busy *client.BusyError
		if !errors.As(err, &busy) {
			return client.Lease{}, err
		}
		if wait <= 0 || !now().Before(deadline) {
			return client.Lease{}, err
		}
		sleep(poll)
	}
}

// remaining formats a deadline as a human-friendly countdown, rounded to
// whole seconds for stable-looking output.
func remaining(expires time.Time, now time.Time) string {
	d := expires.Sub(now).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}
