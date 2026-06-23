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
RUN="$REPO_ROOT/run"                           # session state (gitignored)
BIN="$REPO_ROOT/bin"                           # built binaries (gitignored)
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
KWOK_RUNTIME="${KWOK_RUNTIME:-docker}"         # kube-apiserver has no darwin binary -> docker (prod = linux/binary)

CLUSTERS=(cluster-a cluster-b cluster-c)
SHARD_METRICS="127.0.0.1:18799"
SHARD_LISTEN="127.0.0.1:17780"
PROVIDER_LISTEN="127.0.0.1:19090"   # the fakecloud three-cloud CapacityProvider the shard dials
BACKEND_ADDR="127.0.0.1:8090"       # the demo backend + UI (and the /api/* the hack scripts drive)

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

build_bins(){
  mkdir -p "$BIN"
  resolve_bigfleet_dir
  log "building node-creator + fakecloud-provider + demo-backend (demo) + shard + operator + upc (published bigfleet) -> $BIN"
  ( cd "$REPO_ROOT" && go build -o "$BIN/node-creator" ./cmd/node-creator )
  ( cd "$REPO_ROOT" && go build -o "$BIN/fakecloud-provider" ./cmd/fakecloud-provider )
  ( cd "$REPO_ROOT" && go build -o "$BIN/demo-backend" ./backend/cmd/demo-backend )
  # shard/operator/UPC come from the PUBLISHED bigfleet module (built from its own
  # complete go.mod in the read-only module cache) — no local engine checkout required.
  ( cd "$BIGFLEET_DIR"
    go build -o "$BIN/shard"    ./cmd/bigfleet
    go build -o "$BIN/operator" ./cmd/operator
    go build -o "$BIN/upc"      ./cmd/bigfleet-unschedulable-pod-controller )
}

# create_cluster <name>: a real kwokctl cluster + BigFleet CRDs + PriorityClasses +
# the production/batch namespaces. Writes $RUN/<name>.kubeconfig.
create_cluster(){
  local c="$1"
  kwokctl delete cluster --name "$c" >/dev/null 2>&1 || true
  kwokctl create cluster --name "$c" --runtime "$KWOK_RUNTIME" --quiet-pull \
    --kube-scheduler-config "$HACK_DIR/scheduler-config.yaml" >/dev/null 2>&1
  kwokctl --name "$c" get kubeconfig > "$RUN/$c.kubeconfig"
  local kc="$RUN/$c.kubeconfig"
  resolve_bigfleet_dir
  KUBECONFIG="$kc" kubectl apply -f "$BIGFLEET_DIR/api/crd/" >/dev/null
  KUBECONFIG="$kc" kubectl apply -f "$HACK_DIR/priorityclasses.yaml" >/dev/null
  KUBECONFIG="$kc" kubectl create namespace production >/dev/null 2>&1 || true
  KUBECONFIG="$kc" kubectl create namespace batch >/dev/null 2>&1 || true
}

# kubectl against a cluster
ckubectl(){ local c="$1"; shift; KUBECONFIG="$RUN/$c.kubeconfig" kubectl "$@"; }
