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
# Live on R1 + R2 since 2026-06-27 (launchd, 5-min interval).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PARENT="$(cd "$REPO_ROOT/.." && pwd)"
LABEL="sh.lucy.bigfleet.demohost"
STATE="$REPO_ROOT/run/.autodeploy-state"   # run/ is gitignored
SIBLINGS="bigfleet bigfleet-web-dashboard bigfleet-providers"

log(){ printf '%s autodeploy: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*"; }

# Public runner label — R1 / R2 ONLY, derived from the rN- advertise-host. The underlying host
# identity is deliberately never named, here or in any commit status, since these are public.
RUNNER="$(grep -oE 'r[0-9]+-bigfleet-demo' "$HOME/Library/LaunchAgents/sh.lucy.bigfleet.demohost.plist" 2>/dev/null | grep -oE '^r[0-9]+' | head -1)"
RUNNER="$(printf '%s' "${RUNNER:-r?}" | tr '[:lower:]' '[:upper:]')"
# Optional GitHub token (secrets/ is gitignored) -> post each deploy as a commit status so an
# update's progress/success/failure is visible at a glance on the commit. Absent token = no-op.
GH_TOKEN="$(cat "$REPO_ROOT/secrets/gh-token.key" 2>/dev/null || printf '%s' "${GH_STATUS_TOKEN:-}")"
GH_REPO="$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null | sed -E 's#^.*github\.com[:/]##; s#\.git$##')"

# gh_status STATE DESCRIPTION SHA — post a commit status (state: pending|success|failure|error).
# Context is "deploy/R1" etc.; description names only R1/R2. No-op without token/repo/sha.
gh_status(){
  local state="$1" desc="$2" sha="$3"
  [ -n "$GH_TOKEN" ] && [ -n "$GH_REPO" ] && [ -n "$sha" ] || { log "CI status skipped ($state — $desc)"; return 0; }
  if curl -s -o /dev/null --max-time 20 -X POST \
       -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github+json" \
       "https://api.github.com/repos/$GH_REPO/statuses/$sha" \
       -d "{\"state\":\"$state\",\"context\":\"deploy/$RUNNER\",\"description\":\"$desc\"}" 2>/dev/null; then
    log "CI status: deploy/$RUNNER -> $state ($desc)"
  else
    log "CI status POST failed ($state)"
  fi
}

# wait_build_result FULL_SHA -> echoes ok | held-back | down. After a kickstart, the demohost
# rebuilds (demo-build.sh) before it listens, writing bin/.build-status (ok|failed <short-sha>);
# so healthz 200 + that file referencing this deploy's sha tells us how the rebuild went.
wait_build_result(){
  # Budget ~12 min (144x5s): a kickstart reaps the build cache, so a cold rebuild (cold Go cache +
  # npm cold install after a toolchain bump on the slower runner) can legitimately run long; a
  # tighter window would misreport a slow-but-healthy bring-up as 'down'.
  local full="$1" short st code i
  short="$(git -C "$REPO_ROOT" rev-parse --short "$full" 2>/dev/null)"
  for i in $(seq 1 144); do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:8080/healthz 2>/dev/null)"
    st="$(cat "$REPO_ROOT/bin/.build-status" 2>/dev/null || true)"
    if [ "$code" = "200" ] && [ -n "$short" ] && printf '%s' "$st" | grep -q " $short"; then
      case "$st" in ok\ *) echo ok; return 0;; failed\ *) echo held-back; return 0;; esac
    fi
    sleep 5
  done
  echo down
}

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

# A CI commit status only makes sense for an actual demo-repo commit (a sibling-only bump has no
# commit to annotate). Capture the demo repo's local vs upstream head to decide.
demo_local="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || true)"
demo_remote="$(git -C "$REPO_ROOT" rev-parse origin/main 2>/dev/null || true)"

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

# If the demo repo itself advanced, this deploy maps to that commit — light it up as a CI check.
deploy_sha=""; host_ok=1
[ -n "$demo_remote" ] && [ "$demo_local" != "$demo_remote" ] && deploy_sha="$demo_remote"
[ -n "$deploy_sha" ] && gh_status pending "Deploying to $RUNNER…" "$deploy_sha"

# Fast-forward the demo repo + rebuild the host binary (kickstart alone won't pick up cmd/demohost).
if git -C "$REPO_ROOT" merge -q --ff-only origin/main 2>/dev/null; then
  if ( cd "$REPO_ROOT" && go build -o bin/demohost ./cmd/demohost ) 2>/dev/null; then
    log "demo repo updated + rebuilt bin/demohost"
  else
    host_ok=0; log "demohost rebuild FAILED — keeping the old binary (restart still picks up hack/ + siblings)"
  fi
else
  log "demo repo not fast-forwardable — leaving code as-is (siblings still self-update on restart)"
fi

# If the demo repo didn't actually advance to deploy_sha (dirty/diverged worktree), drop the CI
# wait: the rebuild will stamp the OLD sha, so polling for deploy_sha would burn the full ~12 min
# and post a FALSE 'did not come back up' on a demo that's actually healthy on the old code.
if [ -n "$deploy_sha" ] && [ "$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null)" != "$deploy_sha" ]; then
  gh_status error "$RUNNER could not fast-forward to this commit" "$deploy_sha"
  deploy_sha=""
fi

# Restart: demo-build.sh self-updates ../bigfleet + ../bigfleet-web-dashboard and rebuilds (with a
# staged last-good fallback — a broken upstream main holds back to the previous binaries, demo stays up).
if launchctl kickstart -k "gui/$(id -u)/$LABEL" 2>/dev/null; then
  log "restarted demohost — verifying build"
  # Record the signature (= "don't re-attempt this") only once the demohost actually came back, ok
  # or held-back. On 'down' we LEAVE the old signature so the next tick retries — a wiped state
  # would instead re-baseline (first-run path) and never retry.
  if [ -n "$deploy_sha" ]; then
    result="$(wait_build_result "$deploy_sha")"
    case "$result" in
      ok)
        if [ "$host_ok" = "1" ]; then gh_status success "Deployed to $RUNNER (healthy)" "$deploy_sha"; log "deploy OK"
        else gh_status failure "$RUNNER host binary failed to build — serving previous host" "$deploy_sha"; log "deploy OK but host stale"; fi
        printf '%s' "$sig" > "$STATE" ;;
      held-back)
        gh_status failure "Build failed on $RUNNER — serving last-good" "$deploy_sha"; log "deploy HELD BACK (last-good)"
        printf '%s' "$sig" > "$STATE" ;;
      *)
        gh_status failure "$RUNNER did not come back up after deploy" "$deploy_sha"; log "deploy DOWN — state left unrecorded so the next tick retries" ;;
    esac
  else
    printf '%s' "$sig" > "$STATE"   # sibling-only change: no demo commit to verify, trust the restart
  fi
else
  log "kickstart FAILED — will retry next tick"
  [ -n "$deploy_sha" ] && gh_status error "$RUNNER deploy could not start" "$deploy_sha"
fi

# ── install (run once per runner) ───────────────────────────────────────────────────────────────
# Adjust HOME + PATH for the runner (PATH must include go/git/npm); everything else is derived.
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
#       <key>PATH</key><string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>  <!-- + the dir holding go/git/npm -->
#       <key>GOTOOLCHAIN</key><string>auto</string></dict>
#     <key>StandardOutPath</key><string>$HOME/bigfleet/autodeploy.log</string>
#     <key>StandardErrorPath</key><string>$HOME/bigfleet/autodeploy.log</string>
#   </dict></plist>
#   PLIST
#   launchctl bootout "gui/$(id -u)/sh.lucy.bigfleet.autodeploy" 2>/dev/null
#   launchctl bootstrap "gui/$(id -u)" "$P"
