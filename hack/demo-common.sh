#!/usr/bin/env bash
# Shared config + helpers for the demo stack. Each "cluster" is a REAL kwokctl
# cluster (kube-apiserver + kube-scheduler + kube-controller-manager + kwok), so
# pods, scheduling, priority and preemption are all NATIVE Kubernetes. BigFleet
# moves capacity between them; a tiny node-creator turns UpcomingNodes into kwok
# Nodes. Sourced, not run.
set -euo pipefail

HACK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HACK_DIR/.." && pwd)"
# BIGFLEET_DIR is where the demo gets the shard/operator/UPC binaries + the bigfleet
# CRD yaml. By default it's the PUBLISHED bigfleet module in the local module cache
# (the version is pinned in go.mod) — the demo depends on the published repo, NOT a local
# checkout. Set BIGFLEET_DIR=/path/to/bigfleet to build against a local engine checkout.
BIGFLEET_DIR="${BIGFLEET_DIR:-}"
KWOK_DIR="${KWOK_DIR:-/tmp/kwokbin}"          # kwokctl + kwok live here
BIN="$REPO_ROOT/bin"                           # built binaries (gitignored, SHARED across sessions)
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
KWOK_RUNTIME="${KWOK_RUNTIME:-docker}"         # kube-apiserver has no darwin binary -> docker (prod = linux/binary)

# SESSION_ID scopes a whole demo stack so many can run side by side on one host (the
# demohost daemon sets it per session). Empty = the classic single-session local demo.
SESSION_ID="${SESSION_ID:-}"
if [ -n "$SESSION_ID" ]; then
  RUN="$REPO_ROOT/run/sessions/$SESSION_ID"    # per-session state (gitignored)
else
  RUN="$REPO_ROOT/run"                          # single-session state (gitignored)
fi

# Logical cluster ids — STABLE across sessions (operator --cluster-id, kubeconfig
# filenames, UI labels). The GLOBAL kwokctl/docker name is derived per-session via
# kwokname() so concurrent sessions never collide on a cluster or container name.
CLUSTERS=(cluster-a cluster-b cluster-c)
kwokname(){ if [ -n "$SESSION_ID" ]; then echo "${SESSION_ID}-$1"; else echo "$1"; fi; }

# Ports. A demohost session passes PORT_BASE and we carve a small block out of it so
# sessions don't fight over ports; the classic single-session demo keeps fixed ports.
# (kwokctl auto-allocates each apiserver's own high port, independent of PORT_BASE.)
PORT_BASE="${PORT_BASE:-}"
DASHBOARDS="${DASHBOARDS:-1}"        # per-cluster k8s dashboards (3 docker containers/session); host sets 0 to stay lean
BACKEND_PORT="${PORT_BASE:-8090}"
BACKEND_ADDR="127.0.0.1:$BACKEND_PORT"  # demo backend + UI bind (and the /api/* the hack scripts drive)
if [ -n "$PORT_BASE" ]; then
  SHARD_LISTEN="127.0.0.1:$((PORT_BASE+1))"
  SHARD_METRICS="127.0.0.1:$((PORT_BASE+2))"
  PROVIDER_LISTEN="127.0.0.1:$((PORT_BASE+3))"
  DASH_PORT_BASE=$((PORT_BASE+5))
  # fleet-dashboard stack (the real bigfleet-web-dashboard + its coordinator + Prometheus),
  # all in the free upper half of the 16-port block (+0..+3,+5..+7 are the core stack).
  COORD_LISTEN="127.0.0.1:$((PORT_BASE+8))"
  COORD_RAFT="127.0.0.1:$((PORT_BASE+9))"
  COORD_METRICS="127.0.0.1:$((PORT_BASE+10))"
  PROM_PORT=$((PORT_BASE+11))
  FLEETDASH_PORT=$((PORT_BASE+12))
else
  SHARD_LISTEN="127.0.0.1:17780"
  SHARD_METRICS="127.0.0.1:18799"
  PROVIDER_LISTEN="127.0.0.1:19090"  # the fakecloud three-cloud CapacityProvider the shard dials
  DASH_PORT_BASE=9101
  COORD_LISTEN="127.0.0.1:17790"
  COORD_RAFT="127.0.0.1:17791"
  COORD_METRICS="127.0.0.1:18790"
  PROM_PORT=19091
  FLEETDASH_PORT=18081
fi
COORD_DATA="$RUN/coord-data"

# Two-tier finite fleet (see scenarios/demo-fleet.yaml):
#   ONPREM_SIZE = owned bare-metal Idle pool (--seed-machines): always-on, $0 marginal.
#   CLOUD_SIZE  = elastic OnDemand Speculative quota (--seed-speculative): $0 until provisioned.
# Sized so baseline (3 x BASELINE pods) runs entirely on owned hardware with headroom,
# and a single-cluster surge tops out owned then visibly BURSTS onto cloud.
ONPREM_SIZE="${ONPREM_SIZE:-48}"
CLOUD_SIZE="${CLOUD_SIZE:-72}"
FLEET_SIZE="${FLEET_SIZE:-$((ONPREM_SIZE + CLOUD_SIZE))}"   # hard cap = owned + cloud quota
CLOUD_NODE_HOURLY="${CLOUD_NODE_HOURLY:-0.38}"             # illustrative $/hr per provisioned 8vCPU/32GiB on-demand node (~m5.2xlarge/n2-standard-8 list; NOT a quote)

export PATH="$KWOK_DIR:$PATH"

log(){ printf '\033[1;36m[demo]\033[0m %s\n' "$*" >&2; }
die(){ printf '\033[1;31m[demo]\033[0m %s\n' "$*" >&2; exit 1; }

# resolve_bigfleet_dir: lazily fill BIGFLEET_DIR with the published bigfleet module's
# directory in the local module cache (downloading it at the go.mod-pinned version if
# needed). Read-only — used to build the shard/operator/UPC and to apply the CRDs. A
# pre-set BIGFLEET_DIR (local engine checkout) wins.
resolve_bigfleet_dir(){
  [ -n "$BIGFLEET_DIR" ] && return
  ( cd "$REPO_ROOT" && go mod download github.com/intUnderflow/bigfleet ) 2>/dev/null || true
  BIGFLEET_DIR="$(cd "$REPO_ROOT" && go list -m -f '{{.Dir}}' github.com/intUnderflow/bigfleet 2>/dev/null)"
  [ -n "$BIGFLEET_DIR" ] && [ -d "$BIGFLEET_DIR" ] || die "could not resolve the bigfleet module (run 'go mod download' in $REPO_ROOT)"
}

ensure_kwok(){
  if [ -x "$KWOK_DIR/kwokctl" ] && [ -x "$KWOK_DIR/kwok" ]; then return; fi
  log "downloading kwokctl + kwok $KWOK_VERSION -> $KWOK_DIR"
  mkdir -p "$KWOK_DIR"
  local base="https://github.com/kubernetes-sigs/kwok/releases/download/$KWOK_VERSION"
  curl -fsSL -o "$KWOK_DIR/kwokctl" "$base/kwokctl-darwin-arm64"
  curl -fsSL -o "$KWOK_DIR/kwok"    "$base/kwok-darwin-arm64"
  chmod +x "$KWOK_DIR/kwokctl" "$KWOK_DIR/kwok"
}

# self_update_sibling DIR — fast-forward a sibling SOURCE checkout (../bigfleet etc.) to latest
# origin/main so the runners track upstream without hand-bumped pins. SAFE by construction: it
# NEVER clobbers local work — a dirty tree is left untouched, and it only fast-forwards (a
# diverged/non-ff branch is left as-is). Tolerates a detached HEAD (the old SHA-pinning left
# clones detached). No-op when the repo is absent (fresh clone / CI -> falls back to go.mod pins).
# NOTE: this file runs under `set -euo pipefail`, so every line here is written to ALWAYS
# succeed (return 0) — an absent sibling, a failed fetch, or a non-ff branch must be a no-op,
# never abort the build. (A bare `return 1` or a failing `a || b` chain would kill demo-build.sh
# and crash-loop the demohost.)
self_update_sibling(){
  local d="$1" name f; name="$(basename "$d")"
  [ -d "$d/.git" ] || return 0
  # The build churns tracked files that aren't source: `npm install` rewrites
  # ui/package-lock.json, `go build` can touch go.sum. Restore that churn so it isn't mistaken
  # for human WIP and left to block the fast-forward (this is what stuck a runner's dashboard
  # clone a release behind). Genuine source edits, checked next, still pause the update.
  for f in package-lock.json ui/package-lock.json go.sum; do
    git -C "$d" checkout -q -- "$f" 2>/dev/null || true
  done
  # Only TRACKED modifications count as human work-in-progress (untracked build artifacts don't).
  if ! git -C "$d" diff --quiet 2>/dev/null || ! git -C "$d" diff --cached --quiet 2>/dev/null; then
    log "self-update: $name has local source changes — left at $(git -C "$d" rev-parse --short HEAD 2>/dev/null)"; return 0
  fi
  if ! git -C "$d" fetch -q origin main 2>/dev/null; then
    log "self-update: $name fetch failed — using current checkout"; return 0
  fi
  git -C "$d" checkout -q main 2>/dev/null || git -C "$d" checkout -qB main FETCH_HEAD 2>/dev/null || true
  if git -C "$d" merge -q --ff-only FETCH_HEAD 2>/dev/null; then
    log "self-update: $name -> $(git -C "$d" rev-parse --short HEAD) (latest main)"
  else
    log "self-update: $name not fast-forwardable — left at $(git -C "$d" rev-parse --short HEAD 2>/dev/null)"
  fi
  return 0
}

# setup_engine_workspace — on a host that carries the sibling SOURCE checkouts (the live
# runners), pull them to latest main and write a gitignored go.work so the demo binaries, the
# engine binaries (shard/operator/upc, via resolve_bigfleet_dir -> go list) AND the dashboard
# all build from those local trees in lockstep — auto-updating whenever upstream main moves,
# instead of tracking a hand-bumped pin. No checkouts -> no go.work -> the build falls back to
# the reproducible go.mod pins (the fresh-clone / CI path). go.work{,​.sum} are gitignored.
setup_engine_workspace(){
  local parent bf dash providers
  parent="$(cd "$REPO_ROOT/.." 2>/dev/null && pwd)"
  bf="$parent/bigfleet"
  dash="${BIGFLEET_DASHBOARD_DIR:-$parent/bigfleet-web-dashboard}"
  providers="$parent/bigfleet-providers"
  self_update_sibling "$bf"
  self_update_sibling "$dash"
  self_update_sibling "$providers"
  if [ -d "$bf/.git" ]; then
    # if/fi (not `[ ] && echo`) so the block always exits 0 under set -e even without providers
    { echo "go $(grep -oE '^go [0-9.]+' "$REPO_ROOT/go.mod" | awk '{print $2}')"
      echo
      echo "use ."
      echo "use ../bigfleet"
      if [ -d "$providers/.git" ]; then echo "use ../bigfleet-providers"; fi
    } > "$REPO_ROOT/go.work"
    if [ -d "$providers/.git" ]; then
      log "engine workspace: go.work -> ../bigfleet + ../bigfleet-providers (tracking latest main)"
    else
      log "engine workspace: go.work -> ../bigfleet (tracking latest main)"
    fi
  fi
}

build_bins(){
  mkdir -p "$BIN"
  # SKIP_BUILD lets the demohost build the shared binaries ONCE, then spawn many
  # sessions that reuse them (building per session would be wasteful and racy).
  if [ "${SKIP_BUILD:-0}" = "1" ]; then
    local ok=1
    for b in node-creator fakecloud-provider demo-backend shard operator upc; do
      [ -x "$BIN/$b" ] || ok=0
    done
    if [ "$ok" = "1" ]; then log "SKIP_BUILD=1 — reusing prebuilt binaries in $BIN"; return; fi
    log "SKIP_BUILD=1 but binaries missing — building anyway"
  fi
  setup_engine_workspace   # pull sibling source checkouts to latest main + wire go.work (runners)
  # Force ONE toolchain across the whole build (demo binaries, shard/operator/upc, dashboard).
  # The demo (go 1.26.4), the engine clone (1.26.0) and the dashboard (1.26.1) declare different
  # minimums; left to GOTOOLCHAIN=auto each host picks its own, and the shared GOCACHE then
  # clashes ("compile: version goX does not match go tool version goY"). Pinning every build to
  # the demo's toolchain makes it deterministic regardless of the host's base go (Mini/M1 differ).
  export GOTOOLCHAIN="go$(grep -oE '^go [0-9.]+' "$REPO_ROOT/go.mod" | awk '{print $2}')"
  resolve_bigfleet_dir
  log "building node-creator + fakecloud-provider + demo-backend (demo) + shard + operator + upc (bigfleet @ $BIGFLEET_DIR) -> $BIN"
  ( cd "$REPO_ROOT" && go build -o "$BIN/node-creator" ./cmd/node-creator )
  ( cd "$REPO_ROOT" && go build -o "$BIN/fakecloud-provider" ./cmd/fakecloud-provider )
  ( cd "$REPO_ROOT" && go build -o "$BIN/demo-backend" ./backend/cmd/demo-backend )
  # shard/operator/UPC come from the PUBLISHED bigfleet module (built from its own
  # complete go.mod in the read-only module cache) — no local engine checkout required.
  ( cd "$BIGFLEET_DIR"
    go build -o "$BIN/shard"    ./cmd/bigfleet
    go build -o "$BIN/operator" ./cmd/operator
    go build -o "$BIN/upc"      ./cmd/bigfleet-unschedulable-pod-controller )
  build_fleet_dashboard
}

# build_fleet_dashboard builds the out-of-tree bigfleet-web-dashboard ONCE (its UI baked with
# the /fleet-dash/ reverse-proxy prefix) into $BIN. Best-effort: a host without the dashboard
# checkout or npm just skips it and the session runs without the fleet dashboard (demo-up.sh and
# demo-fleet-dashboard.sh both gate on $BIN/bigfleet-web-dashboard existing). The dashboard links
# the coordinator/shard-read protos from a sibling ../bigfleet (its go.mod `replace`); setup_engine_workspace
# already pulled BOTH ../bigfleet and ../bigfleet-web-dashboard to latest main, so they stay in gRPC
# lockstep without a hand-pinned revision. We still force the demo's Go toolchain so the shared
# build cache doesn't clash with the dashboard's own go.mod toolchain line.
build_fleet_dashboard(){
  local dash="${BIGFLEET_DASHBOARD_DIR:-$(cd "$REPO_ROOT/.." 2>/dev/null && pwd)/bigfleet-web-dashboard}"
  [ -d "$dash" ] || { log "fleet-dash build: $dash absent — skipping (no BigFleet dashboard this run)"; return 0; }
  command -v npm >/dev/null 2>&1 || { log "fleet-dash build: npm not found — skipping"; return 0; }
  local gotc bf dashsha bfsha
  gotc="go$(grep -oE '^go [0-9.]+' "$REPO_ROOT/go.mod" | awk '{print $2}')"
  bf="$(cd "$dash/.." 2>/dev/null && pwd)/bigfleet"
  dashsha=$(git -C "$dash" rev-parse --short HEAD 2>/dev/null)
  bfsha=$(git -C "$bf" rev-parse --short HEAD 2>/dev/null)
  log "building bigfleet-web-dashboard (BASE_PATH=/fleet-dash/, dashboard ${dashsha:-?}, engine ${bfsha:-?}, $gotc) -> $BIN/bigfleet-web-dashboard"
  if ( cd "$dash" && GOTOOLCHAIN="$gotc" BASE_PATH=/fleet-dash/ make build >/dev/null 2>&1 ) \
     && [ -x "$dash/bin/bigfleet-web-dashboard" ]; then
    cp "$dash/bin/bigfleet-web-dashboard" "$BIN/bigfleet-web-dashboard"
  else
    log "fleet-dash build FAILED — session runs without the BigFleet dashboard (non-fatal)"
  fi
}

# create_cluster <name>: a real kwokctl cluster + BigFleet CRDs + PriorityClasses +
# the production/batch namespaces. Writes $RUN/<name>.kubeconfig.
create_cluster(){
  local c="$1"               # logical id (cluster-a/b/c) — stable, used everywhere downstream
  local kn; kn="$(kwokname "$c")"  # global kwokctl/docker name — unique per session
  kwokctl delete cluster --name "$kn" >/dev/null 2>&1 || true
  kwokctl create cluster --name "$kn" --runtime "$KWOK_RUNTIME" --quiet-pull \
    --kube-scheduler-config "$HACK_DIR/scheduler-config.yaml" >/dev/null 2>&1
  kwokctl --name "$kn" get kubeconfig > "$RUN/$c.kubeconfig"
  local kc="$RUN/$c.kubeconfig"
  resolve_bigfleet_dir
  KUBECONFIG="$kc" kubectl apply -f "$BIGFLEET_DIR/api/crd/" >/dev/null
  KUBECONFIG="$kc" kubectl apply -f "$HACK_DIR/priorityclasses.yaml" >/dev/null
  KUBECONFIG="$kc" kubectl create namespace production >/dev/null 2>&1 || true
  KUBECONFIG="$kc" kubectl create namespace batch >/dev/null 2>&1 || true
}

# kubectl against a cluster
ckubectl(){ local c="$1"; shift; KUBECONFIG="$RUN/$c.kubeconfig" kubectl "$@"; }
