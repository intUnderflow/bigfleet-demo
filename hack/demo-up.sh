#!/usr/bin/env bash
# Bring up the single-host demo stack on the REAL-cluster substrate:
#   3 kwokctl clusters (cluster-a/b/c) — real apiserver + stock kube-scheduler
#   (native priority + preemption) + kwok fake nodes — driven by 1 BigFleet shard
#   + per-cluster operator + UPC + node-creator (UpcomingNode -> kwok Node). The
#   demo backend creates native Deployments; the stock scheduler does the rest.
# Run demo-down.sh first if a stack is already up.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"

command -v go >/dev/null || die "go not found"
command -v curl >/dev/null || die "curl not found"
docker info >/dev/null 2>&1 || die "docker not available (kwokctl uses it for the apiserver)"
# The bigfleet engine binaries + CRDs come from the published module (go.mod-pinned);
# build_bins resolves & downloads it on demand — no local ../bigfleet checkout needed.

mkdir -p "$RUN"
: > "$RUN/pids"
start(){ local log="$1"; shift; nohup "$@" >"$RUN/$log" 2>&1 & echo $! >> "$RUN/pids"; }

ensure_kwok
build_bins

for c in "${CLUSTERS[@]}"; do
  log "creating real cluster $c (kwokctl)"
  create_cluster "$c"
done

log "starting fakecloud provider (3 real providerkit backends — on-prem/AWS/GCP, kwok-simulated; CERTIFIED core,cloud,spot)"
start fakecloud.log "$BIN/fakecloud-provider" --addr "$PROVIDER_LISTEN"
sleep 1

log "starting shard (dials the fakecloud provider — declares BareMetal+Reserved committed + OnDemand+Spot elastic = $FLEET_SIZE)"
# --execute-concurrency: parallel provisioning is safe since BigFleet ADR-0058 keyed the
# kit's fence high-water mark per (shard, machine). Before that, a single shard's concurrent
# execute workers fenced each other (out-of-order seqs against one per-shard mark) and machines
# landed in FAILED, which forced =1 (serial, gradual). Now concurrent ops on different machines
# don't fence, so we run a wide pool to keep a 120-node burst snappy.
start shard.log "$BIN/shard" shard \
  --provider-addr="$PROVIDER_LISTEN" --execute-concurrency=16 \
  --listen="$SHARD_LISTEN" --metrics-addr="$SHARD_METRICS" --data-dir="$RUN/shard-data"
sleep 2

for c in "${CLUSTERS[@]}"; do
  log "starting operator + upc + node-creator for $c"
  start "$c-operator.log" "$BIN/operator" --cluster-id="$c" --shard-addr="$SHARD_LISTEN" \
    --kubeconfig="$RUN/$c.kubeconfig" --metrics-addr=0
  start "$c-upc.log" "$BIN/upc" --kubeconfig="$RUN/$c.kubeconfig" --metrics-addr=0 \
    --priority-class-defaults="$HACK_DIR/priority-penalties.yaml"
  # --warmup: mint the initial baseline nodes with NO dwell so a fresh session settles to its
  # at-rest state in seconds (the dwell still applies after, for the interactive moments).
  start "$c-node-creator.log" "$BIN/node-creator" --kubeconfig="$RUN/$c.kubeconfig" \
    --warmup "${NODE_WARMUP:-60s}"
done

if [ "$DASHBOARDS" = "1" ]; then
  log "starting Kubernetes dashboards (one per cluster — real apiservers, fully functional)"
  bash "$HACK_DIR/demo-dashboards.sh" || true
else
  log "dashboards disabled (DASHBOARDS=0) — keeping the session lean"
  : > "$RUN/dashboards"
fi

# session descriptor for the backend
{
  echo "{"
  echo "  \"shardMetrics\": \"$SHARD_METRICS\","
  echo "  \"workspaces\": ["
  for idx in "${!CLUSTERS[@]}"; do
    c="${CLUSTERS[$idx]}"; comma=","; [ "$idx" -eq $(( ${#CLUSTERS[@]} - 1 )) ] && comma=""
    # || true: with dashboards off the file is empty, and grep-no-match (1) would trip set -e
    dash=$(grep "^$c " "$RUN/dashboards" 2>/dev/null | awk '{print $2}' || true)
    echo "    {\"name\": \"$c\", \"kubeconfig\": \"$RUN/$c.kubeconfig\", \"dashboard\": \"$dash\"}$comma"
  done
  echo "  ]"
  echo "}"
} > "$RUN/session.json"

log "starting demo backend + UI on $BACKEND_ADDR"
# Build args as an array so the optional session-clock flags splice in cleanly (the
# ${VAR:+ "..."} form mangles quoting). The clock flags are only added for a hosted
# session (SESSION_ID set by demohost); the standalone local demo passes none.
backend_args=( --session "$RUN/session.json" --ui "$REPO_ROOT/ui" --addr "$BACKEND_ADDR"
  --onprem-size "$ONPREM_SIZE" --cloud-size "$CLOUD_SIZE" --fleet-size "$FLEET_SIZE"
  --cloud-node-hourly "$CLOUD_NODE_HOURLY" )
[ -n "$SESSION_ID" ] && backend_args+=( --session-id "$SESSION_ID" )
[ -n "${SESSION_TTL:-}" ] && backend_args+=( --session-ttl "$SESSION_TTL" )
[ -n "${SESSION_IDLE_TIMEOUT:-}" ] && backend_args+=( --idle-timeout "$SESSION_IDLE_TIMEOUT" )
start backend.log "$BIN/demo-backend" "${backend_args[@]}"
sleep 2

log "demo stack up."
cat >&2 <<EOF

  ┌────────────────────────────────────────────────────────────┐
  │  Open the demo:   http://localhost:$BACKEND_PORT
  │  Real apiservers + stock kube-scheduler (native priority &  │
  │  preemption) + kwok nodes, with BigFleet moving capacity.   │
  └────────────────────────────────────────────────────────────┘

    hack/demo-down.sh    # tear it all down
EOF
