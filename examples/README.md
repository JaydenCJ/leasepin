# leasepin examples

Two runnable scripts, both offline and self-contained (they only talk to
your own leasepin server on loopback). Start a server first:

```bash
go build -o leasepin ./cmd/leasepin
./leasepin serve --state /tmp/leasepin-demo.state.json
```

- **`cron-wrap.sh <command> [args...]`** — the duplicate-cron killer.
  Wraps any job in `leasepin withlock` with a 5-minute self-renewing
  lease named after the command. A second invocation while the first
  runs exits 10 and does nothing; cron just skips that cycle. Point
  every host's crontab at the same server and the job becomes
  fleet-wide singleton.

- **`fenced-writer.sh <target-dir>`** — the fencing contract, live. The
  wrapped job receives `LEASEPIN_TOKEN` and the *target* enforces
  monotonicity: it records the highest token accepted in `.fence` and
  rejects anything lower. Run it repeatedly and watch tokens climb;
  that same check is what stops a paused zombie from clobbering the
  current holder's writes.

```bash
bash examples/cron-wrap.sh sleep 2 &
bash examples/cron-wrap.sh sleep 2 ; echo "second run exited $?"   # -> 10

bash examples/fenced-writer.sh /tmp/shared-target
bash examples/fenced-writer.sh /tmp/shared-target
cat /tmp/shared-target/.fence
```
