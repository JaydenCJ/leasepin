# leasepin protocol and state format

This document specifies the HTTP API, the fencing-token contract, and
the on-disk state file. Everything here is covered by tests; the wire
shapes are stable within a major version.

## The model

A **lock** is a name: 1–128 characters from `A-Z a-z 0-9 . _ -`. Locks
are created implicitly on first acquire.

A **lease** is a hold on a lock: `(holder, token, deadline)`. Leases are
granted for a TTL and die at their deadline unless renewed. Expiry is
evaluated lazily and inclusively — a lease is gone at exactly its
deadline. There is no background reaper to race against.

A **fencing token** is a per-lock `uint64` counter with three
guarantees:

1. **Strictly increasing per grant.** Every successful acquire returns a
   token greater than every token ever returned for that lock — across
   releases, expiries, holders, and server restarts.
2. **Durable before visible.** The incremented counter is fsynced to the
   state file *before* the grant is returned. If persistence fails, the
   grant is refused and that token number is burned, never reissued.
3. **Stable per lease.** Renewals move the deadline, never the token.
   The token identifies the acquisition, not the heartbeat.

### The consumer contract

Fencing only works if the protected resource enforces it. The rule for
any downstream storage/API you guard with leasepin:

> Track the highest token seen per lock. Reject any write carrying a
> token lower than that. Accept and record equal-or-higher tokens.

With that rule, a paused-then-resumed process holding a stale lease
cannot clobber the work of the current holder — its token is smaller and
its writes bounce, no matter how confused it is about time.

## HTTP API

All bodies are JSON. Timestamps are Unix milliseconds. Errors are
`{"error": "..."}` plus fields listed below. Request bodies over 64 KiB
are rejected; unknown JSON fields are rejected (catches `ttl` vs
`ttl_ms` typos).

### `POST /v1/locks/{name}/acquire`

Request: `{"holder": "web01-4242", "ttl_ms": 30000}`

- `200` — the lease:
  `{"name", "holder", "token", "ttl_ms", "acquired_at_unix_ms", "expires_at_unix_ms"}`
- `409` — validly held: `{"error": "held", "name", "holder", "expires_at_unix_ms"}`.
  The response deliberately **omits the token**: tokens are capabilities,
  returned only to the holder they were granted to.
- `400` — invalid name/holder/TTL or malformed body.
- `500` — the state file could not be written; nothing was granted.

Acquire is **not reentrant**: the same holder acquiring a lock it
already holds gets a `409`. Renew is the way to extend. There is no
server-side blocking acquire; clients poll (`--wait`/`--poll` in the
CLI) so the server never parks connections.

### `POST /v1/locks/{name}/renew`

Request: `{"holder": "...", "token": 42, "ttl_ms": 30000}`

- `200` — the lease with the new deadline (same token).
- `410` — `{"error": "gone", "name", "reason"}`. The lease expired, was
  released, or the lock now belongs to someone else. **The caller must
  stop treating the resource as owned.** 409 vs 410 is the load-bearing
  distinction: busy means retry later, gone means stop now.

### `POST /v1/locks/{name}/release`

Request: `{"holder": "...", "token": 42}`

- `200` — `{"released": true, "name"}`.
- `410` — the lease was not yours to release (stale token, wrong holder,
  or already expired). A stale release never unlocks the current
  holder's lease.

### `GET /v1/locks/{name}` and `GET /v1/locks`

Status of one lock (`{"name", "state": "held"|"free", "last_token",
"lease"?}`) and the sorted list of all currently held locks
(`{"locks": [...], "count"}`). Unknown names read as free with
`last_token: 0`.

### `GET /v1/healthz`

`{"ok": true, "version": "0.1.0"}` — cheap liveness for wrappers.

## State file

A single JSON document, schema version 1, written atomically on every
mutation (temp file in the same directory → fsync → rename → directory
fsync) and created `0600`:

```json
{
  "schema_version": 1,
  "locks": {
    "deploy": { "last_token": 7 },
    "nightly-backup": {
      "last_token": 42,
      "holder": "cron-web01",
      "token": 42,
      "ttl_ms": 30000,
      "acquired_at_unix_ms": 1783144616000,
      "expires_at_unix_ms": 1783144646000
    }
  }
}
```

Free locks keep their `last_token` entry forever — that floor is the
fencing guarantee across restarts. The file grows by one small record
per distinct lock name ever used, which in practice is dozens of bytes
per cron job.

On startup, a **missing** state file is a normal first run. A present
but unreadable file (truncated, corrupt, future schema) makes the server
**refuse to start**: silently starting empty would reset every token
floor, which is precisely the failure fencing exists to prevent. Leases
whose deadline passed while the server was down load as already expired;
their floors survive.

## Security model

leasepin binds `127.0.0.1` by default and speaks plain HTTP with no
authentication: the trust boundary is "processes allowed to talk to the
socket". That is the right shape for its job — serializing cron jobs and
deploys on a host or within a private network. Tokens order writers;
they are not secrets that gate access. If you move the listener onto a
LAN address, anyone who can reach it can take locks — front it with a
reverse proxy that authenticates if that matters in your environment.
Never expose it to the public internet.
