#!/usr/bin/env bash
# fenced-writer.sh — demonstrates the fencing-token consumer contract
# against a shared directory, end to end.
#
# The wrapped job writes to a shared target that ENFORCES fencing: it
# keeps the highest token it has ever accepted in .fence, and refuses
# writers carrying a lower token. Run it twice and watch the token rise;
# a zombie process replaying an old token would be rejected exactly the
# same way.
#
#   bash examples/fenced-writer.sh /tmp/shared-target
set -euo pipefail

TARGET="${1:?usage: fenced-writer.sh <target-dir>}"
LEASEPIN_SERVER="${LEASEPIN_SERVER:-http://127.0.0.1:7420}"
LEASEPIN_BIN="${LEASEPIN_BIN:-leasepin}"   # set LEASEPIN_BIN to a repo build if not installed
export LEASEPIN_SERVER
mkdir -p "$TARGET"

# The inner job: this is what enforcement looks like at the resource.
write_fenced() {
  fence_file="$TARGET/.fence"
  highest="$(cat "$fence_file" 2>/dev/null || echo 0)"
  if [ "$LEASEPIN_TOKEN" -lt "$highest" ]; then
    echo "REJECTED: token $LEASEPIN_TOKEN < highest seen $highest (stale writer)" >&2
    exit 1
  fi
  echo "$LEASEPIN_TOKEN" >"$fence_file"
  echo "write accepted under fencing token $LEASEPIN_TOKEN"
  date +%s >"$TARGET/last-write"
}

# withlock exports LEASEPIN_TOKEN to the child; the child enforces it.
export -f write_fenced
export TARGET
exec "$LEASEPIN_BIN" withlock --name "fenced-writer" --ttl 30s --wait 1m \
  -- bash -c write_fenced
