# Contributing to leasepin

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — the tool and its tests are pure
standard library and never talk to anything beyond 127.0.0.1.

```bash
git clone https://github.com/JaydenCJ/leasepin && cd leasepin
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, starts a real server on a loopback
port with a temp state file, and drives the full lock lifecycle through
the CLI — acquire, conflict, renew, withlock, lost leases, and a
restart that must keep the fencing floor; it must finish by printing
`SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (90 deterministic tests, no sleeps, no
   network beyond loopback httptest).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules — the lock table takes an injected clock and a persistence
   hook precisely so every rule is testable without timers or disk.

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in
  the PR.
- No network calls other than the user's own server address, and no
  telemetry. The server binds loopback by default and must keep doing so.
- The fencing invariants are contracts, not implementation details:
  tokens strictly increase per lock across releases, expiries, and
  restarts, and are persisted before they are granted. Any change
  touching them needs a test that would fail under the old bug.
- Code comments and doc comments are written in English.
- Timing-free tests only: inject clocks, tick channels, and sleepers.
  A test that sleeps to "wait for" something will be rejected.

## Reporting bugs

Include the output of `leasepin version`, the exact commands you ran
(server flags and client flags), the relevant slice of the state file
if persistence is involved, and what you expected. Lock bugs are almost
always reproducible with two or three CLI calls against a fresh server.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
