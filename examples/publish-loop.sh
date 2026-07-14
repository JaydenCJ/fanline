#!/usr/bin/env bash
# Publishes a demo metric to the metrics.demo channel twice a second.
# Works against `fanline serve --dev` out of the box; set FANLINE_TOKEN
# (and FANLINE_URL if not 127.0.0.1:8787) for an authenticated hub.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/fanline"
[ -x "$BIN" ] || { echo "build first: go build -o fanline ./cmd/fanline" >&2; exit 1; }

i=0
while true; do
  i=$((i + 1))
  cpu=$(( (i * 37) % 100 ))       # deterministic fake load curve
  "$BIN" publish --channel metrics.demo --event sample \
    --data "{\"n\":$i,\"cpu\":$cpu,\"host\":\"demo-01\"}" >/dev/null
  sleep 0.5
done
