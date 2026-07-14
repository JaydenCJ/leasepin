// withlock tests: the full run-under-lock lifecycle. The simple paths go
// through Run against a real in-process server; the timing-sensitive
// paths (wait-then-acquire, renew heartbeat, lease-lost kill) drive the
// runner directly with a fake lease API and hand-fired tick channels, so
// nothing here depends on wall-clock time.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JaydenCJ/leasepin/internal/client"
	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/server"
)

func TestWithlockRequiresNameAndCommand(t *testing.T) {
	code, _, errOut := runCLI(t, "withlock", "--", "true")
	if code != ExitUsage || !strings.Contains(errOut, "--name") {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	code, _, errOut = runCLI(t, "withlock", "--name", "deploy")
	if code != ExitUsage || !strings.Contains(errOut, "no command") {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
}

func TestWithlockRunsCommandPassingStdoutAndExitCodeThrough(t *testing.T) {
	url := startServer(t)
	code, out, errOut := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--ttl", "1m", "--", "echo", "under lock")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "under lock") {
		t.Fatalf("child stdout not passed through: %q", out)
	}
	code, _, _ = runCLI(t, "withlock", "--server", url, "--name", "deploy", "--ttl", "1m", "--", "sh", "-c", "exit 7")
	if code != 7 {
		t.Fatalf("exit = %d, want the child's 7", code)
	}
}

func TestWithlockMapsChildSignalDeathTo128PlusSignal(t *testing.T) {
	url := startServer(t)
	// The child kills itself with SIGTERM (15): the wrapper must report
	// 128+15, exactly like a shell would.
	code, _, _ := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--ttl", "1m", "--", "sh", "-c", "kill -TERM $$")
	if code != 143 {
		t.Fatalf("exit = %d, want 143", code)
	}
}

func TestWithlockExportsLeaseEnvToChild(t *testing.T) {
	url := startServer(t)
	script := `[ "$LEASEPIN_NAME" = deploy ] && [ "$LEASEPIN_TOKEN" = 1 ] && ` +
		`[ "$LEASEPIN_HOLDER" = ci-1 ] && [ -n "$LEASEPIN_EXPIRES_AT_MS" ] && [ -n "$LEASEPIN_SERVER" ]`
	code, _, errOut := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--holder", "ci-1", "--ttl", "1m", "--", "sh", "-c", script)
	if code != ExitOK {
		t.Fatalf("lease env not exported (exit %d): %s", code, errOut)
	}
}

func TestWithlockReleasesLockAfterCommandEnds(t *testing.T) {
	url := startServer(t)
	code, _, _ := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--ttl", "1m", "--", "true")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	_, out, _ := runCLI(t, "status", "--server", url, "--name", "deploy")
	if !strings.Contains(out, "free") {
		t.Fatalf("lock not released after run: %q", out)
	}
	if !strings.Contains(out, "last token 1") {
		t.Fatalf("token floor should survive the release: %q", out)
	}
}

func TestWithlockBusyLockFailsFastWithExitTen(t *testing.T) {
	url := startServer(t)
	runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "squatter", "--ttl", "1m")
	code, _, errOut := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--", "true")
	if code != ExitBusy {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitBusy, errOut)
	}
	if !strings.Contains(errOut, "squatter") {
		t.Fatalf("stderr should name the holder: %s", errOut)
	}
}

func TestWithlockFailedCommandStartReleasesLock(t *testing.T) {
	url := startServer(t)
	code, _, _ := runCLI(t, "withlock", "--server", url, "--name", "deploy", "--", "/nonexistent/binary-xyz")
	if code != ExitRuntime {
		t.Fatalf("exit = %d, want %d", code, ExitRuntime)
	}
	_, out, _ := runCLI(t, "status", "--server", url, "--name", "deploy")
	if !strings.Contains(out, "free") {
		t.Fatalf("lock leaked after failed start: %q", out)
	}
}

// --- runner-level tests with a scripted lease API ---------------------

// fakeAPI scripts acquire/renew/release outcomes and records calls.
type fakeAPI struct {
	mu           sync.Mutex
	acquireErrs  []error // popped per call; nil = success
	renewErr     error   // returned by every renew
	renewCount   int
	releaseCount int
	released     chan struct{} // closed on first release
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{released: make(chan struct{})}
}

func (f *fakeAPI) Acquire(name, holder string, ttl time.Duration) (client.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acquireErrs) > 0 {
		err := f.acquireErrs[0]
		f.acquireErrs = f.acquireErrs[1:]
		if err != nil {
			return client.Lease{}, err
		}
	}
	return client.Lease{
		Name: name, Holder: holder, Token: 42, TTL: ttl,
		AcquiredAt: time.Now(), ExpiresAt: time.Now().Add(ttl),
	}, nil
}

func (f *fakeAPI) Renew(name, holder string, token uint64, ttl time.Duration) (client.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCount++
	if f.renewErr != nil {
		return client.Lease{}, f.renewErr
	}
	return client.Lease{Name: name, Holder: holder, Token: token, TTL: ttl, ExpiresAt: time.Now().Add(ttl)}, nil
}

func (f *fakeAPI) Release(name, holder string, token uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCount++
	if f.releaseCount == 1 {
		close(f.released)
	}
	return nil
}

// newTestRunner wires a runner whose renew ticks fire only when the test
// sends on tickC, and whose kill-grace timer fires immediately (the
// child is a `sleep` that dies on SIGTERM anyway).
func newTestRunner(api leaseAPI, argv []string) (*withlockRunner, chan time.Time, *bytes.Buffer) {
	tickC := make(chan time.Time)
	stderr := &bytes.Buffer{}
	r := &withlockRunner{
		api: api,
		opts: withlockOptions{
			name: "deploy", holder: "ci-1", ttl: time.Hour, poll: time.Second,
			killGrace: time.Hour, server: "http://127.0.0.1:7420", argv: argv,
		},
		stderr:      stderr,
		childStdin:  strings.NewReader(""),
		childStdout: &bytes.Buffer{},
		childStderr: &bytes.Buffer{},
		now:         time.Now,
		sleep:       func(time.Duration) {},
		tick:        func(time.Duration) (<-chan time.Time, func()) { return tickC, func() {} },
		after: func(time.Duration) <-chan time.Time {
			c := make(chan time.Time, 1)
			c <- time.Time{} // grace elapses instantly; SIGKILL is exercised
			return c
		},
		signals: make(chan os.Signal),
	}
	return r, tickC, stderr
}

// The fencing enforcement path: a renew that comes back "gone" must stop
// the wrapped command and exit 11 — this is what prevents a paused or
// stalled job from writing after its lease moved on.
func TestRunnerKillsChildAndExitsElevenWhenLeaseIsLost(t *testing.T) {
	api := newFakeAPI()
	api.renewErr = &client.GoneError{Name: "deploy", Reason: "lease expired or was released"}
	// A long sleep: without the kill this test would hang, so the fast
	// finish is itself the assertion.
	r, tickC, stderr := newTestRunner(api, []string{"sleep", "60"})

	codeC := make(chan int, 1)
	go func() { codeC <- r.run() }()
	tickC <- time.Now() // heartbeat fires, renew says gone

	code := <-codeC
	if code != ExitLost {
		t.Fatalf("exit = %d, want %d", code, ExitLost)
	}
	if !strings.Contains(stderr.String(), "lease") || !strings.Contains(stderr.String(), "lost") {
		t.Fatalf("stderr should explain the lost lease: %s", stderr.String())
	}
	if api.releaseCount != 0 {
		t.Fatalf("must not release a lease it no longer owns (releases: %d)", api.releaseCount)
	}
}

// Transient renew failures (server blip, restart) must NOT kill the
// command: the lease may still be valid, and the next tick retries.
func TestRunnerKeepsChildAliveThroughTransientRenewFailure(t *testing.T) {
	api := newFakeAPI()
	api.renewErr = fmt.Errorf("server returned HTTP 500: disk full")
	r, tickC, stderr := newTestRunner(api, []string{"sh", "-c", "read line"})
	// The child waits on stdin; give the runner a pipe we control.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	r.childStdin = pr

	codeC := make(chan int, 1)
	go func() { codeC <- r.run() }()

	tickC <- time.Now() // transient failure #1
	tickC <- time.Now() // transient failure #2 — still running

	pw.Write([]byte("done\n")) // let the child finish normally
	pw.Close()

	if code := <-codeC; code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if api.renewCount != 2 {
		t.Fatalf("renew attempts = %d, want 2", api.renewCount)
	}
	if !strings.Contains(stderr.String(), "will retry") {
		t.Fatalf("stderr should log the retry: %s", stderr.String())
	}
}

func TestRunnerRenewalHeartbeatExtendsLease(t *testing.T) {
	api := newFakeAPI()
	r, tickC, _ := newTestRunner(api, []string{"sh", "-c", "read line"})
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	r.childStdin = pr

	codeC := make(chan int, 1)
	go func() { codeC <- r.run() }()
	tickC <- time.Now()
	tickC <- time.Now()
	tickC <- time.Now()
	pw.Write([]byte("done\n"))
	pw.Close()

	if code := <-codeC; code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if api.renewCount != 3 {
		t.Fatalf("renew count = %d, want 3", api.renewCount)
	}
	if api.releaseCount != 1 {
		t.Fatalf("release count = %d, want exactly 1", api.releaseCount)
	}
}

// Waiting for a busy lock: two busy answers, then success — driven by
// the injected sleeper, no wall time.
func TestRunnerWaitsThroughBusyLockThenRuns(t *testing.T) {
	api := newFakeAPI()
	busy := &client.BusyError{Name: "deploy", Holder: "other", ExpiresAt: time.Now().Add(time.Minute)}
	api.acquireErrs = []error{busy, busy, nil}
	r, _, _ := newTestRunner(api, []string{"true"})
	r.opts.wait = time.Hour
	slept := 0
	r.sleep = func(time.Duration) { slept++ }

	if code := r.run(); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if slept != 2 {
		t.Fatalf("sleeps = %d, want 2", slept)
	}
}

func TestRunnerForwardsSignalsToChild(t *testing.T) {
	api := newFakeAPI()
	r, _, _ := newTestRunner(api, []string{"sleep", "60"})
	sigC := make(chan os.Signal, 1)
	r.signals = sigC

	codeC := make(chan int, 1)
	go func() { codeC <- r.run() }()
	// The buffered channel parks the signal until the runner's select
	// loop starts (which is after the child has started), so one send is
	// race-free. `sleep` dies on the forwarded SIGINT.
	sigC <- os.Interrupt

	code := <-codeC
	if code != 130 { // 128 + SIGINT(2)
		t.Fatalf("exit = %d, want 130", code)
	}
	if api.releaseCount != 1 {
		t.Fatalf("release count = %d, want 1 (normal exit path)", api.releaseCount)
	}
}

func TestRenewIntervalDefaultsToThirdOfTTL(t *testing.T) {
	r := &withlockRunner{opts: withlockOptions{ttl: 30 * time.Second}}
	if got := r.renewInterval(); got != 10*time.Second {
		t.Fatalf("interval = %v, want 10s", got)
	}
	r.opts.renewEvery = 2 * time.Second
	if got := r.renewInterval(); got != 2*time.Second {
		t.Fatalf("explicit interval = %v, want 2s", got)
	}
	r.opts.renewEvery = 0
	r.opts.ttl = 150 * time.Millisecond
	if got := r.renewInterval(); got != 100*time.Millisecond {
		t.Fatalf("floor = %v, want 100ms", got)
	}
}

func TestExitCodeFromWaitMapsOutcomes(t *testing.T) {
	if got := exitCodeFromWait(nil); got != 0 {
		t.Fatalf("nil -> %d, want 0", got)
	}
	if got := exitCodeFromWait(errors.New("not an exit error")); got != ExitRuntime {
		t.Fatalf("plain error -> %d, want %d", got, ExitRuntime)
	}
}

// End-to-end lost-lease drill against a real server: expire the lease by
// releasing it out from under the runner, then fire the heartbeat.
func TestRunnerAgainstRealServerDetectsStolenLease(t *testing.T) {
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	t.Cleanup(ts.Close)
	cl := client.New(ts.URL)

	r, tickC, _ := newTestRunner(cl, []string{"sleep", "60"})
	codeC := make(chan int, 1)
	go func() { codeC <- r.run() }()

	// Steal the lease with the runner's own credentials (an admin
	// force-release does the same thing operationally).
	for {
		st, err := cl.Get("deploy")
		if err != nil {
			t.Fatal(err)
		}
		if st.Held {
			if err := cl.Release("deploy", st.Lease.Holder, st.Lease.Token); err != nil {
				t.Fatal(err)
			}
			break
		}
		// Acquire hasn't landed yet; yield and re-check. This loop is
		// bounded by the acquire that is already in flight.
	}
	tickC <- time.Now() // heartbeat now discovers the loss

	if code := <-codeC; code != ExitLost {
		t.Fatalf("exit = %d, want %d", code, ExitLost)
	}
}
