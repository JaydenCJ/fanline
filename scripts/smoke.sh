#!/usr/bin/env bash
# End-to-end smoke test for fanline: builds the binary, mints tokens, runs
# a real hub on a loopback port, and drives publish/tail/replay/auth paths
# through the actual CLI. No external tools, no network beyond 127.0.0.1,
# idempotent, finishes in seconds.
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
  [ -f "$WORKDIR/server.log" ] && sed 's/^/  server: /' "$WORKDIR/server.log" >&2
  exit 1
}

# wait_for <file> <pattern>: poll (bounded) until pattern appears in file.
wait_for() {
  for _ in $(seq 1 200); do
    grep -q "$2" "$1" 2>/dev/null && return 0
    sleep 0.05
  done
  fail "timed out waiting for '$2' in $1"
}

BIN="$WORKDIR/fanline"
KEYS="main=smoke-secret-1,backup=smoke-secret-2"

echo "1. build"
(cd "$ROOT" && go build -o "$BIN" ./cmd/fanline) || fail "go build failed"

echo "2. version matches manifest"
"$BIN" version | grep -qx "fanline 0.1.0" || fail "--version mismatch"

echo "3. mint and verify tokens"
PUB_TOKEN="$("$BIN" token new --keys "$KEYS" --kid main --channel 'orders.**' --cap pub --ttl 1h)"
SUB_TOKEN="$("$BIN" token new --keys "$KEYS" --kid backup --channel 'orders.**' --cap sub --ttl 1h)"
"$BIN" token inspect --keys "$KEYS" "$PUB_TOKEN" | grep -q '"signature": "valid"' \
  || fail "minted publish token did not verify"
"$BIN" token inspect --keys "$KEYS" "$SUB_TOKEN" | grep -q '"backup"' \
  || fail "token did not carry its key id"

echo "4. tampered key must not verify"
if "$BIN" token inspect --keys "main=wrong,backup=alsowrong" "$PUB_TOKEN" >"$WORKDIR/bad.json" 2>&1; then
  fail "inspect with wrong keys exited 0"
fi
grep -q '"signature": "invalid"' "$WORKDIR/bad.json" || fail "wrong-key inspect not marked invalid"

echo "5. start the hub on a loopback port"
"$BIN" serve --addr 127.0.0.1:0 --keys "$KEYS" --replay 16 2>"$WORKDIR/server.log" &
SERVER_PID=$!
wait_for "$WORKDIR/server.log" "listening on"
URL="http://$(sed -n 's#.*listening on http://\([0-9.:]*\).*#\1#p' "$WORKDIR/server.log")"
[ "$URL" != "http://" ] || fail "could not parse listen address"

echo "6. live fanout: tail sees events published after it connects"
"$BIN" tail --url "$URL" --channel orders.eu --token "$SUB_TOKEN" --max 2 \
  >"$WORKDIR/tail.out" 2>"$WORKDIR/tail.err" &
TAIL_PID=$!
wait_for "$WORKDIR/tail.err" "# connected"
"$BIN" publish --url "$URL" --channel orders.eu --token "$PUB_TOKEN" \
  --event created --data '{"order":41}' >"$WORKDIR/pub1.json"
"$BIN" publish --url "$URL" --channel orders.eu --token "$PUB_TOKEN" \
  --event created --data '{"order":42}' >"$WORKDIR/pub2.json"
wait "$TAIL_PID" || fail "tail exited non-zero"
grep -q '"seq":1' "$WORKDIR/pub1.json" || fail "publish response missing seq"
grep -q '{"order":41}' "$WORKDIR/tail.out" || fail "tail missed event 1"
grep -q '{"order":42}' "$WORKDIR/tail.out" || fail "tail missed event 2"
grep -qE $'^[0-9a-f]+-1\tcreated\t' "$WORKDIR/tail.out" || fail "tail output missing id/event columns"

echo "7. replay: a late subscriber recovers the history"
"$BIN" tail --url "$URL" --channel orders.eu --token "$SUB_TOKEN" \
  --replay 10 --max 2 >"$WORKDIR/replay.out" 2>"$WORKDIR/replay.err"
grep -q "replayed=2" "$WORKDIR/replay.err" || fail "replay count not reported"
grep -q '{"order":41}' "$WORKDIR/replay.out" || fail "replay missed the first event"

echo "8. resume: Last-Event-ID delivers only what was missed"
LAST_ID="$(head -n1 "$WORKDIR/replay.out" | cut -f1)"
"$BIN" publish --url "$URL" --channel orders.eu --token "$PUB_TOKEN" --data '{"order":43}' >/dev/null
"$BIN" tail --url "$URL" --channel orders.eu --token "$SUB_TOKEN" \
  --last-id "$LAST_ID" --max 2 >"$WORKDIR/resume.out" 2>/dev/null
grep -q '{"order":41}' "$WORKDIR/resume.out" && fail "resume replayed an already-seen event"
grep -q '{"order":42}' "$WORKDIR/resume.out" || fail "resume missed order 42"
grep -q '{"order":43}' "$WORKDIR/resume.out" || fail "resume missed order 43"

echo "9. auth: a subscribe token cannot publish, wrong channel is denied"
if "$BIN" publish --url "$URL" --channel orders.eu --token "$SUB_TOKEN" --data x 2>"$WORKDIR/deny1.err"; then
  fail "sub-only token was allowed to publish"
fi
grep -q "403" "$WORKDIR/deny1.err" || fail "capability denial was not a 403"
if "$BIN" publish --url "$URL" --channel invoices.eu --token "$PUB_TOKEN" --data x 2>"$WORKDIR/deny2.err"; then
  fail "token published outside its channel pattern"
fi

echo "10. dev-mode hub refuses non-loopback binds"
if "$BIN" serve --dev --addr 0.0.0.0:0 2>"$WORKDIR/dev.err"; then
  fail "--dev accepted a non-loopback address"
fi
grep -q "loopback" "$WORKDIR/dev.err" || fail "refusal did not explain the loopback rule"

echo "SMOKE OK"
