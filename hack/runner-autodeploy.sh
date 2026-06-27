#!/usr/bin/env bash
# Periodic self-deploy for a live runner. Run on an interval by launchd (see the install block
# at the bottom). Each tick it fetches the demo repo + the sibling source checkouts and, ONLY if
# an upstream `origin/main` has actually moved since the last successful deploy, it:
#   1. fast-forwards the demo repo and rebuilds bin/demohost (a plain restart won't pick up
#      cmd/demohost changes — demo-build.sh rebuilds the session binaries but not the host),
#   2. restarts the demohost, whose startup demo-build.sh then self-updates ../bigfleet +
#      ../bigfleet-web-dashboard to latest main and rebuilds everything (see setup_engine_workspace).
#
# Disruption-on-update is acceptable (author OK'd 2026-06-27), so there is no graceful drain — a
# restart reaps the warm pool + any live session. The whole point of the change-gate is that an
# IDLE poll (nothing moved) never restarts, so visitors are only ever disrupted by a REAL update.
#
# DRY_RUN=1 reports what it would do without pulling/rebuilding/restarting.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PARENT="$(cd "$REPO_ROOT/.." && pwd)"
LABEL="sh.lucy.bigfleet.demohost"
STATE="$REPO_ROOT/run/.autodeploy-state"   # run/ is gitignored
SIBLINGS="bigfleet bigfleet-web-dashboard bigfleet-providers"

log(){ printf '%s autodeploy: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*"; }

# Build a signature of every upstream origin/main we track. A fetch failure aborts the whole tick
# (returning a partial signature would look like a change and trigger a spurious restart).
sig=""
add_sig(){ # name dir
  local n="$1" d="$2" sha
  git -C "$d" fetch -q origin main 2>/dev/null || { log "$n: fetch failed — skipping this tick"; exit 0; }
  sha="$(git -C "$d" rev-parse origin/main 2>/dev/null)" || { log "$n: rev-parse failed — skipping this tick"; exit 0; }
  sig="$sig $n:$sha"
}
add_sig demo "$REPO_ROOT"
for name in $SIBLINGS; do
  [ -d "$PARENT/$name/.git" ] && add_sig "$name" "$PARENT/$name"
done

mkdir -p "$REPO_ROOT/run"
prev="$(cat "$STATE" 2>/dev/null || true)"

# First run: record the baseline and do NOT restart (the runner was just deployed by hand; there's
# nothing pending). We only ever restart on a CHANGE from a recorded baseline.
if [ -z "$prev" ]; then
  [ "${DRY_RUN:-0}" = "1" ] || printf '%s' "$sig" > "$STATE"
  log "first run — recorded baseline, no restart"
  exit 0
fi

if [ "$sig" = "$prev" ]; then
  log "up to date — no restart"
  exit 0
fi

log "upstream moved -> deploying"
log "  was:${prev}"
log "  now:${sig}"
if [ "${DRY_RUN:-0}" = "1" ]; then
  log "DRY_RUN — would: ff demo repo, rebuild bin/demohost, kickstart $LABEL"
  exit 0
fi

# Fast-forward the demo repo + rebuild the host binary (kickstart alone won't pick up cmd/demohost).
if git -C "$REPO_ROOT" merge -q --ff-only origin/main 2>/dev/null; then
  if ( cd "$REPO_ROOT" && go build -o bin/demohost ./cmd/demohost ) 2>/dev/null; then
    log "demo repo updated + rebuilt bin/demohost"
  else
    log "demohost rebuild FAILED — keeping the old binary (restart still picks up hack/ + siblings)"
  fi
else
  log "demo repo not fast-forwardable — leaving code as-is (siblings still self-update on restart)"
fi

# Restart: demo-build.sh self-updates ../bigfleet + ../bigfleet-web-dashboard and rebuilds.
if launchctl kickstart -k "gui/$(id -u)/$LABEL" 2>/dev/null; then
  printf '%s' "$sig" > "$STATE"   # record only on success, so a failed deploy retries next tick
  log "restarted demohost — deploy complete"
else
  log "kickstart FAILED — will retry next tick"
fi

# ── install (run once per runner) ───────────────────────────────────────────────────────────────
# Adjust HOME if the runner user differs; everything else is derived.
#   U=$(id -un); P=$HOME/Library/LaunchAgents/sh.lucy.bigfleet.autodeploy.plist
#   cat > "$P" <<PLIST
#   <?xml version="1.0" encoding="UTF-8"?>
#   <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
#   <plist version="1.0"><dict>
#     <key>Label</key><string>sh.lucy.bigfleet.autodeploy</string>
#     <key>ProgramArguments</key><array>
#       <string>/bin/bash</string><string>$HOME/bigfleet/src/hack/runner-autodeploy.sh</string></array>
#     <key>StartInterval</key><integer>300</integer>
#     <key>RunAtLoad</key><true/>
#     <key>EnvironmentVariables</key><dict>
#       <key>HOME</key><string>$HOME</string>
#       <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
#       <key>GOTOOLCHAIN</key><string>auto</string></dict>
#     <key>StandardOutPath</key><string>$HOME/bigfleet/autodeploy.log</string>
#     <key>StandardErrorPath</key><string>$HOME/bigfleet/autodeploy.log</string>
#   </dict></plist>
#   PLIST
#   launchctl bootout "gui/$(id -u)/sh.lucy.bigfleet.autodeploy" 2>/dev/null
#   launchctl bootstrap "gui/$(id -u)" "$P"
