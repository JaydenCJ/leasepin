#!/usr/bin/env bash
# cron-wrap.sh — the one-line pattern that stops duplicate cron runs.
#
# Put your real job command after `--`. If another host (or a stuck
# previous run) still holds the lock, this exits 10 immediately and cron
# simply skips this cycle — no overlap, no pileup.
#
#   crontab:  */15 * * * *  /opt/leasepin/examples/cron-wrap.sh /usr/local/bin/nightly-backup.sh
#
# Requires a running server, e.g.:  leasepin serve --state /var/lib/leasepin/state.json
set -euo pipefail

LEASEPIN_SERVER="${LEASEPIN_SERVER:-http://127.0.0.1:7420}"
LEASEPIN_BIN="${LEASEPIN_BIN:-leasepin}"   # set LEASEPIN_BIN to a repo build if not installed
export LEASEPIN_SERVER

if [ $# -eq 0 ]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

# --ttl 5m with automatic renewal every ttl/3: a job that runs for an
# hour keeps the lease alive the whole time, and a job that dies keeps
# the lock for at most 5 minutes.
exec "$LEASEPIN_BIN" withlock \
  --name "cron.$(basename "$1")" \
  --ttl 5m \
  --kill-grace 10s \
  -- "$@"
