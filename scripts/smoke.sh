#!/usr/bin/env bash
# End-to-end smoke test for leasepin: builds the binary, starts a real
# server on a loopback port with a temp state file, and drives the whole
# lock lifecycle through the CLI — acquire, conflict, fencing tokens,
# withlock, release, expiry-free restart persistence. No network beyond
# 127.0.0.1, idempotent, finishes in seconds.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKDIR="$(mktemp -d)"
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

fail() {
  echo "SMOKE FAIL: $*" >&2
  exit 1
}

BIN="$WORKDIR/leasepin"
STATE="$WORKDIR/state.json"
ADDR="127.0.0.1:7461"
export LEASEPIN_SERVER="http://$ADDR"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/leasepin) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" --version | grep -qx "leasepin 0.1.0" || fail "--version mismatch"

echo "3. start the server on a loopback port"
"$BIN" serve --addr "$ADDR" --state "$STATE" --quiet >"$WORKDIR/serve.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  if "$BIN" status --name warmup >/dev/null 2>&1; then break; fi
  sleep 0.1
done
"$BIN" status --name warmup >/dev/null || fail "server did not come up"

echo "4. acquire grants fencing token 1"
OUT="$("$BIN" acquire --name deploy --holder smoke-a --ttl 30s)"
echo "$OUT" | grep -q "acquired deploy: token 1, holder smoke-a" || fail "unexpected acquire output: $OUT"

echo "5. second acquire conflicts with exit code 10"
set +e
"$BIN" acquire --name deploy --holder smoke-b --ttl 30s >/dev/null 2>"$WORKDIR/busy.err"
CODE=$?
set -e
[ "$CODE" -eq 10 ] || fail "busy acquire exited $CODE, want 10"
grep -q 'held by "smoke-a"' "$WORKDIR/busy.err" || fail "busy message should name the holder"

echo "6. status and list show the live lease"
"$BIN" status --name deploy | grep -q "held by smoke-a (token 1" || fail "status wrong"
"$BIN" list | grep -q "deploy" || fail "list missing the lock"

echo "7. renew extends without changing the token"
"$BIN" renew --name deploy --holder smoke-a --token 1 --ttl 1m | grep -q "renewed deploy: token 1" || fail "renew changed the token"

echo "8. release frees the lock; stale release is refused"
"$BIN" release --name deploy --holder smoke-a --token 1 | grep -qx "released deploy" || fail "release failed"
set +e
"$BIN" release --name deploy --holder smoke-a --token 1 >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 11 ] || fail "double release exited $CODE, want 11"

echo "9. tokens keep increasing across grants (fencing)"
"$BIN" acquire --name deploy --holder smoke-c --ttl 30s | grep -q "token 2" || fail "token did not increase"
"$BIN" release --name deploy --holder smoke-c --token 2 >/dev/null

echo "10. withlock runs a command under the lock with lease env"
OUT="$("$BIN" withlock --name deploy --holder smoke-w --ttl 30s -- sh -c 'echo "token=$LEASEPIN_TOKEN name=$LEASEPIN_NAME"')"
echo "$OUT" | grep -q "token=3 name=deploy" || fail "withlock env wrong: $OUT"
"$BIN" status --name deploy | grep -q "free (last token 3)" || fail "withlock did not release"

echo "11. withlock passes the child's exit code through"
set +e
"$BIN" withlock --name deploy --ttl 30s -- sh -c 'exit 7' >/dev/null 2>&1
CODE=$?
set -e
[ "$CODE" -eq 7 ] || fail "withlock exited $CODE, want the child's 7"

echo "12. withlock --wait outlasts a short-lived holder"
"$BIN" acquire --name deploy --holder squatter --ttl 30s >/dev/null
( sleep 1; "$BIN" release --name deploy --holder squatter --token 5 >/dev/null ) &
RELEASER=$!
"$BIN" withlock --name deploy --wait 30s --poll 200ms --ttl 30s -- true || fail "waited withlock failed"
wait "$RELEASER"

echo "13. state file survives a server restart with the token floor intact"
kill "$SERVER_PID" && wait "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""
grep -q '"last_token": 6' "$STATE" || fail "state file missing the floor"
"$BIN" serve --addr "$ADDR" --state "$STATE" --quiet >>"$WORKDIR/serve.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  if "$BIN" status --name deploy >/dev/null 2>&1; then break; fi
  sleep 0.1
done
"$BIN" acquire --name deploy --holder smoke-r --ttl 30s | grep -q "token 7" || fail "restart reset the fencing floor"

echo "14. usage errors exit 2"
set +e
"$BIN" frobnicate >/dev/null 2>&1
[ $? -eq 2 ] || fail "unknown command should exit 2"
"$BIN" withlock --name deploy >/dev/null 2>&1
[ $? -eq 2 ] || fail "withlock without a command should exit 2"
set -e

echo "SMOKE OK"
