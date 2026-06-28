#!/usr/bin/env bash
# conformance.sh — certify the demo's fakecloud provider against the real
# BigFleet provider contract (core,cloud,spot profiles).
#
# The demo's three fake providers (on-prem/AWS/GCP) are composed behind ONE
# multiplexed providerkit.Server, so we certify that one binary as a black box
# over the wire (-target), exactly like any real provider. We boot it with a
# LARGE Speculative seed (the suite consumes a fresh Speculative machine per
# behavior across ~60 checks; with the demo's 72 it would exhaust mid-run and
# silently SKIP — a hollow CERTIFIED). fault/durable/scale need -provider mode
# (the harness must own kill/restart) and a providers/<name> dir in the sibling
# repo — out of scope for a demo binary; those lanes show as not-implemented.
#
# DEV/CI GATE — needs the bigfleet-providers repo checked out at ../bigfleet-providers
# (override with PROVIDERS_DIR). The conformance harness is a submodule with local
# `replace` directives, so it can't run from the published module cache. (Running the
# demo itself — hack/demo-up.sh — needs NO sibling checkout; only this gate does.)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROVIDERS_DIR="${PROVIDERS_DIR:-$REPO_ROOT/../bigfleet-providers}"
PORT="${PORT:-19095}"
OUT="${OUT:-$REPO_ROOT/run/conformance}"
BIN="$REPO_ROOT/bin/fakecloud-provider"

mkdir -p "$OUT"
echo "building fakecloud-provider…"
go -C "$REPO_ROOT" build -o "$BIN" ./cmd/fakecloud-provider

echo "booting provider on 127.0.0.1:$PORT with a large Speculative seed…"
"$BIN" --addr "127.0.0.1:$PORT" \
  --onprem-baremetal 20 --aws-reserved 10 \
  --aws-ondemand 200 --aws-spot 100 --gcp-ondemand 200 --gcp-spot 100 \
  > "$OUT/provider.log" 2>&1 &
PROV_PID=$!
trap 'kill "$PROV_PID" 2>/dev/null || true' EXIT

# wait for listen
for _ in $(seq 1 30); do
  if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN 2>/dev/null | grep -q fakecloud; then break; fi
  sleep 0.3
done

echo "running conformance (core,cloud,spot)…"
go -C "$PROVIDERS_DIR/conformance" run ./cmd/bfconformance \
  -target "127.0.0.1:$PORT" -profile core,cloud,spot -out "$OUT"

# Guard against a hollow CERTIFIED: require the verdict AND zero extension skips.
python3 - "$OUT/report.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
verdict = str(d.get("verdict", "")).upper()
ext = d.get("extension", {})
skipped = ext.get("skipped", 0)
failed = ext.get("failed", 0)
print(f"verdict={verdict} extension: passed={ext.get('passed')} failed={failed} skipped={skipped}")
if verdict != "CERTIFIED" or failed or skipped:
    print("FAIL: not cleanly certified (hollow skips or failures)")
    sys.exit(1)
print("OK: fakecloud provider CERTIFIED core,cloud,spot with no skips")
PY
