// CLI tests: exercise Run in-process against an httptest server —
// commands, flags, exit codes, and output shapes, without building a
// binary or sleeping.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/leasepin/internal/client"
	"github.com/JaydenCJ/leasepin/internal/lease"
	"github.com/JaydenCJ/leasepin/internal/server"
	"github.com/JaydenCJ/leasepin/internal/version"
)

// runCLI invokes Run and captures both streams.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// startServer brings up an in-process lock server and returns its URL.
func startServer(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestNoArgsPrintsUsageAndVersionPrintsManifestVersion(t *testing.T) {
	code, out, _ := runCLI(t)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "withlock") || !strings.Contains(out, "fencing tokens") {
		t.Fatalf("usage missing key content:\n%s", out)
	}
	code, out, _ = runCLI(t, "version")
	if code != ExitOK || out != "leasepin "+version.Version+"\n" {
		t.Fatalf("version output = %d %q", code, out)
	}
	code2, out2, _ := runCLI(t, "--version")
	if code2 != ExitOK || out2 != out {
		t.Fatalf("--version differs from version: %q", out2)
	}
}

// Every usage mistake exits 2 with a message that names the problem.
func TestUsageErrorsExitTwo(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // substring stderr must contain
	}{
		{"unknown command", []string{"frobnicate"}, "unknown command"},
		{"acquire without name", []string{"acquire"}, "--name"},
		{"release without credentials", []string{"release", "--name", "deploy"}, "--token"},
		{"serve with stray argument", []string{"serve", "extra-arg"}, "unexpected argument"},
		// A zero or negative poll interval would spin the wait loop hot
		// against the server; both waiting commands refuse it up front.
		{"acquire with zero poll", []string{"acquire", "--name", "deploy", "--poll", "0s"}, "--poll must be positive"},
		{"withlock with negative poll", []string{"withlock", "--name", "deploy", "--poll", "-1s", "--", "true"}, "--poll must be positive"},
	}
	for _, tc := range cases {
		code, _, errOut := runCLI(t, tc.args...)
		if code != ExitUsage {
			t.Errorf("%s: exit = %d, want %d", tc.name, code, ExitUsage)
		}
		if !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: stderr %q should mention %q", tc.name, errOut, tc.want)
		}
	}
}

func TestAcquirePrintsTokenLine(t *testing.T) {
	url := startServer(t)
	code, out, errOut := runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1", "--ttl", "1m")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "acquired deploy: token 1, holder ci-1") {
		t.Fatalf("stdout: %s", out)
	}
}

func TestAcquireBusyExitsTen(t *testing.T) {
	url := startServer(t)
	runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1", "--ttl", "1m")
	code, _, errOut := runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-2", "--ttl", "1m")
	if code != ExitBusy {
		t.Fatalf("exit = %d, want %d", code, ExitBusy)
	}
	if !strings.Contains(errOut, `held by "ci-1"`) {
		t.Fatalf("stderr should name the holder: %s", errOut)
	}
}

func TestAcquireAndStatusJSONFormats(t *testing.T) {
	url := startServer(t)
	code, out, _ := runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not json: %s", out)
	}
	if got["name"] != "deploy" || got["token"] != float64(1) || got["ttl_ms"] != float64(30000) {
		t.Fatalf("acquire json = %v", got)
	}
	code, out, _ = runCLI(t, "status", "--server", url, "--name", "deploy", "--format", "json")
	if code != ExitOK {
		t.Fatalf("status exit = %d", code)
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status not json: %s", out)
	}
	if got["state"] != "held" || got["holder"] != "ci-1" || got["token"] != float64(1) {
		t.Fatalf("status json = %v", got)
	}
}

func TestRenewThenReleaseViaCLI(t *testing.T) {
	url := startServer(t)
	runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1")

	code, out, errOut := runCLI(t, "renew", "--server", url, "--name", "deploy", "--holder", "ci-1", "--token", "1", "--ttl", "2m")
	if code != ExitOK || !strings.Contains(out, "renewed deploy: token 1") {
		t.Fatalf("renew: %d %q %q", code, out, errOut)
	}

	code, out, errOut = runCLI(t, "release", "--server", url, "--name", "deploy", "--holder", "ci-1", "--token", "1")
	if code != ExitOK || !strings.Contains(out, "released deploy") {
		t.Fatalf("release: %d %q %q", code, out, errOut)
	}
}

func TestRenewLostLeaseExitsEleven(t *testing.T) {
	url := startServer(t)
	code, _, _ := runCLI(t, "renew", "--server", url, "--name", "deploy", "--holder", "ci-1", "--token", "7")
	if code != ExitLost {
		t.Fatalf("renew of nonexistent lease = %d, want %d", code, ExitLost)
	}
}

func TestStatusFreeAndHeld(t *testing.T) {
	url := startServer(t)
	code, out, _ := runCLI(t, "status", "--server", url, "--name", "deploy")
	if code != ExitOK || !strings.Contains(out, "deploy: free (last token 0)") {
		t.Fatalf("free status: %d %q", code, out)
	}
	runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1")
	code, out, _ = runCLI(t, "status", "--server", url, "--name", "deploy")
	if code != ExitOK || !strings.Contains(out, "held by ci-1 (token 1") {
		t.Fatalf("held status: %d %q", code, out)
	}
}

func TestListShowsHeldLocksTable(t *testing.T) {
	url := startServer(t)
	code, out, _ := runCLI(t, "list", "--server", url)
	if code != ExitOK || !strings.Contains(out, "no locks held") {
		t.Fatalf("empty list: %d %q", code, out)
	}
	runCLI(t, "acquire", "--server", url, "--name", "deploy", "--holder", "ci-1")
	runCLI(t, "acquire", "--server", url, "--name", "backup", "--holder", "cron-2")
	code, out, _ = runCLI(t, "list", "--server", url)
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "deploy") || !strings.Contains(out, "backup") {
		t.Fatalf("list table:\n%s", out)
	}
	if strings.Index(out, "backup") > strings.Index(out, "deploy") {
		t.Fatalf("list not sorted:\n%s", out)
	}
}

func TestCommandsFailFastWhenServerIsDown(t *testing.T) {
	// Grab a URL from a server we immediately close: connection refused,
	// no timeout, fully deterministic.
	ts := httptest.NewServer(server.New(lease.NewTable(nil)).Handler())
	url := ts.URL
	ts.Close()
	code, _, errOut := runCLI(t, "status", "--server", url, "--name", "deploy")
	if code != ExitRuntime {
		t.Fatalf("exit = %d, want %d", code, ExitRuntime)
	}
	if !strings.Contains(errOut, "unreachable") {
		t.Fatalf("stderr: %s", errOut)
	}
}

func TestDefaultHolderIsUniqueAndValid(t *testing.T) {
	a, b := defaultHolder(), defaultHolder()
	if a == b {
		t.Fatalf("two default holders collided: %s", a)
	}
	if err := lease.ValidateHolder(a); err != nil {
		t.Fatalf("default holder %q invalid: %v", a, err)
	}
}

// waitLoop is the shared retry engine of acquire and withlock; driven
// here with a fake clock and sleeper, so no wall time passes.
func TestWaitLoopRetriesUntilLockFrees(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var slept []time.Duration
	attempts := 0
	l, err := waitLoop(10*time.Second, time.Second,
		func() time.Time { return now },
		func(d time.Duration) { slept = append(slept, d); now = now.Add(d) },
		func() (client.Lease, error) {
			attempts++
			if attempts < 4 {
				return client.Lease{}, &client.BusyError{Name: "deploy", Holder: "other"}
			}
			return client.Lease{Name: "deploy", Token: 9}, nil
		})
	if err != nil {
		t.Fatalf("waitLoop: %v", err)
	}
	if l.Token != 9 || attempts != 4 || len(slept) != 3 {
		t.Fatalf("token %d, attempts %d, sleeps %v", l.Token, attempts, slept)
	}
}

func TestWaitLoopGivesUpAtDeadlineAndZeroWaitFailsFast(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	attempts := 0
	_, err := waitLoop(3*time.Second, time.Second,
		func() time.Time { return now },
		func(d time.Duration) { now = now.Add(d) },
		func() (client.Lease, error) {
			attempts++
			return client.Lease{}, &client.BusyError{Name: "deploy", Holder: "other"}
		})
	var busy *client.BusyError
	if err == nil || !errors.As(err, &busy) {
		t.Fatalf("want BusyError after deadline, got %v", err)
	}
	// t0 try, sleep to t1, try, ... deadline t3: tries at 0,1,2,3 = 4.
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	attempts = 0
	_, err = waitLoop(0, time.Second,
		time.Now,
		func(time.Duration) { t.Fatal("must not sleep with wait=0") },
		func() (client.Lease, error) {
			attempts++
			return client.Lease{}, &client.BusyError{Name: "deploy"}
		})
	if err == nil || attempts != 1 {
		t.Fatalf("wait=0: attempts = %d, err = %v", attempts, err)
	}
}

func TestWaitLoopStopsOnNonBusyError(t *testing.T) {
	attempts := 0
	_, err := waitLoop(time.Hour, time.Second,
		time.Now,
		func(time.Duration) { t.Fatal("must not retry a non-busy error") },
		func() (client.Lease, error) {
			attempts++
			return client.Lease{}, &client.GoneError{Name: "deploy", Reason: "boom"}
		})
	if err == nil || attempts != 1 {
		t.Fatalf("attempts = %d, err = %v", attempts, err)
	}
}
