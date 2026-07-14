# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Lease-based lock table with per-lock monotonic **fencing tokens**:
  strictly increasing across releases, expiries, holders, and server
  restarts; persisted (fsync + atomic rename) *before* a grant is
  returned, so a crash can never reissue a token; renewals extend the
  deadline but never change the token.
- **File-persisted state** (`schema_version: 1` JSON): free locks keep
  their token floor forever, expired leases load as already dead, and a
  corrupt/truncated state file makes the server refuse to start rather
  than silently reset fencing floors.
- **HTTP API** on loopback by default: acquire / renew / release /
  status / list / healthz, with the load-bearing 409-busy ("retry
  later") vs 410-gone ("stop now") distinction, capability-safe conflict
  responses that never leak the live token, strict JSON decoding, and
  bounded request bodies.
- **`leasepin withlock`** — run any command under a lock: poll-based
  `--wait`, lease env for the child (`LEASEPIN_TOKEN` and friends),
  automatic renewal at ttl/3, transient-failure tolerance, and a
  SIGTERM→SIGKILL stop of the command with exit 11 when the lease is
  lost; the child's own exit code (or 128+signal) passes through
  otherwise.
- CLI commands `serve`, `acquire`, `renew`, `release`, `status`, `list`,
  `version` with `--format json`, documented exit codes
  (0/2/3/10 busy/11 lost), and `LEASEPIN_SERVER` support.
- TTL bounds (`--min-ttl`/`--max-ttl`), lock-name and holder validation,
  graceful shutdown, and an access log (`--quiet` to silence).
- Protocol and state-format specification (`docs/protocol.md`) including
  the fencing consumer contract, plus runnable examples
  (`examples/cron-wrap.sh`, `examples/fenced-writer.sh`).
- 90 deterministic offline tests (injected clocks, hand-fired renew
  ticks, in-process httptest servers — no sleeps) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/leasepin/releases/tag/v0.1.0
