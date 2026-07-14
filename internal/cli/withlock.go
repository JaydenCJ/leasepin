package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/JaydenCJ/leasepin/internal/client"
)

// leaseAPI is the slice of the client the withlock runner needs;
// narrowed to an interface so tests can drive lease loss and busy locks
// without a server or wall-clock time.
type leaseAPI interface {
	Acquire(name, holder string, ttl time.Duration) (client.Lease, error)
	Renew(name, holder string, token uint64, ttl time.Duration) (client.Lease, error)
	Release(name, holder string, token uint64) error
}

// withlockOptions is everything runWithlock parses from flags.
type withlockOptions struct {
	name       string
	holder     string
	ttl        time.Duration
	wait       time.Duration
	poll       time.Duration
	renewEvery time.Duration
	killGrace  time.Duration
	server     string
	argv       []string
}

// withlockRunner executes one command under one lease. Every source of
// time and every side channel is a field, so the full lifecycle —
// acquire-with-wait, renew heartbeat, lease-lost kill, signal
// forwarding — is testable in-process and deterministically.
type withlockRunner struct {
	api    leaseAPI
	opts   withlockOptions
	stderr io.Writer

	childStdin  io.Reader
	childStdout io.Writer
	childStderr io.Writer

	now     func() time.Time
	sleep   func(time.Duration)
	tick    func(time.Duration) (<-chan time.Time, func()) // renew heartbeat
	after   func(time.Duration) <-chan time.Time           // kill grace timer
	signals <-chan os.Signal                               // forwarded to the child
}

func runWithlock(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("withlock", stderr)
	name := fs.String("name", "", "lock name (required)")
	holder := fs.String("holder", "", "holder id (default: host-pid-random)")
	ttl := fs.Duration("ttl", 30*time.Second, "lease TTL; renewed at ttl/3 while the command runs")
	wait := fs.Duration("wait", 0, "keep retrying a busy lock for up to this long (0 = fail fast)")
	poll := fs.Duration("poll", time.Second, "retry interval while waiting")
	renewEvery := fs.Duration("renew-every", 0, "renew interval (default: ttl/3)")
	killGrace := fs.Duration("kill-grace", 5*time.Second, "time between SIGTERM and SIGKILL when the lease is lost")
	serverURL := serverFlag(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	argv := fs.Args()
	if *name == "" {
		fmt.Fprintf(stderr, "leasepin withlock: --name is required\n")
		return ExitUsage
	}
	if len(argv) == 0 {
		fmt.Fprintf(stderr, "leasepin withlock: no command given (usage: leasepin withlock --name NAME [flags] -- command args...)\n")
		return ExitUsage
	}
	if *poll <= 0 {
		fmt.Fprintf(stderr, "leasepin withlock: --poll must be positive (got %s)\n", *poll)
		return ExitUsage
	}
	if *holder == "" {
		*holder = defaultHolder()
	}

	sigC := make(chan os.Signal, 4)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigC)

	r := &withlockRunner{
		api: client.New(*serverURL),
		opts: withlockOptions{
			name: *name, holder: *holder, ttl: *ttl, wait: *wait, poll: *poll,
			renewEvery: *renewEvery, killGrace: *killGrace, server: *serverURL, argv: argv,
		},
		stderr:      stderr,
		childStdin:  os.Stdin,
		childStdout: stdout,
		childStderr: stderr,
		now:         time.Now,
		sleep:       time.Sleep,
		tick: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		after:   time.After,
		signals: sigC,
	}
	return r.run()
}

// renewInterval derives the heartbeat period: a third of the TTL, so two
// renewals can fail transiently before the lease actually expires.
func (r *withlockRunner) renewInterval() time.Duration {
	if r.opts.renewEvery > 0 {
		return r.opts.renewEvery
	}
	d := r.opts.ttl / 3
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	return d
}

// childEnv is the contract with the wrapped command: enough to verify,
// renew, or hand off the lease, and above all the fencing token to
// attach to every downstream write.
func (r *withlockRunner) childEnv(l client.Lease) []string {
	return append(os.Environ(),
		"LEASEPIN_NAME="+l.Name,
		"LEASEPIN_HOLDER="+l.Holder,
		fmt.Sprintf("LEASEPIN_TOKEN=%d", l.Token),
		fmt.Sprintf("LEASEPIN_EXPIRES_AT_MS=%d", l.ExpiresAt.UnixMilli()),
		"LEASEPIN_SERVER="+r.opts.server,
	)
}

func (r *withlockRunner) run() int {
	l, err := waitLoop(r.opts.wait, r.opts.poll, r.now, r.sleep, func() (client.Lease, error) {
		return r.api.Acquire(r.opts.name, r.opts.holder, r.opts.ttl)
	})
	if err != nil {
		fmt.Fprintf(r.stderr, "leasepin withlock: %v\n", err)
		var busy *client.BusyError
		if errors.As(err, &busy) {
			return ExitBusy
		}
		return ExitRuntime
	}

	cmd := exec.Command(r.opts.argv[0], r.opts.argv[1:]...)
	cmd.Stdin = r.childStdin
	cmd.Stdout = r.childStdout
	cmd.Stderr = r.childStderr
	cmd.Env = r.childEnv(l)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(r.stderr, "leasepin withlock: start command: %v\n", err)
		if rerr := r.api.Release(l.Name, l.Holder, l.Token); rerr != nil {
			fmt.Fprintf(r.stderr, "leasepin withlock: release: %v\n", rerr)
		}
		return ExitRuntime
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	tickC, stopTick := r.tick(r.renewInterval())
	defer stopTick()
	expiresAt := l.ExpiresAt

	for {
		select {
		case werr := <-done:
			// Normal end: the command finished while the lease was live.
			if rerr := r.api.Release(l.Name, l.Holder, l.Token); rerr != nil {
				var gone *client.GoneError
				if !errors.As(rerr, &gone) { // already-gone on exit is harmless
					fmt.Fprintf(r.stderr, "leasepin withlock: release: %v\n", rerr)
				}
			}
			return exitCodeFromWait(werr)

		case <-tickC:
			nl, rerr := r.api.Renew(l.Name, l.Holder, l.Token, r.opts.ttl)
			if rerr == nil {
				expiresAt = nl.ExpiresAt
				continue
			}
			var gone *client.GoneError
			var busy *client.BusyError
			lost := errors.As(rerr, &gone) || errors.As(rerr, &busy) || !r.now().Before(expiresAt)
			if !lost {
				// Transient failure (server restarting, blip): the lease
				// may well still be valid, so keep the command running
				// and retry on the next tick.
				fmt.Fprintf(r.stderr, "leasepin withlock: renew failed (will retry): %v\n", rerr)
				continue
			}
			fmt.Fprintf(r.stderr, "leasepin withlock: lease on %q lost (%v); stopping command\n", r.opts.name, rerr)
			r.stopChild(cmd, done)
			return ExitLost

		case sig := <-r.signals:
			// Forward and let the child decide; its exit surfaces via done.
			if cmd.Process != nil {
				_ = cmd.Process.Signal(sig)
			}
		}
	}
}

// stopChild is the fencing enforcement path: SIGTERM, a grace period,
// then SIGKILL. It only returns once the child is reaped.
func (r *withlockRunner) stopChild(cmd *exec.Cmd, done <-chan error) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
		return
	case <-r.after(r.opts.killGrace):
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	<-done
}

// exitCodeFromWait maps cmd.Wait's error to the code the shell expects:
// the child's own exit code, or 128+signal when it died from one.
func exitCodeFromWait(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
	}
	return ExitRuntime
}
