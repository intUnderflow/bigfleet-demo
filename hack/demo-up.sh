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

# Fleet-dashboard stack (only when DASHBOARDS=1 AND the dashboard binary was built): a single-node
# coordinator the shard registers with, so the real bigfleet-web-dashboard can show this session's
# fleet. NON-load-bearing — the shard "must stay alive and ready with zero coordinators"
# (--coordinator-addr Empty disables), so this can't destabilise the core stack.
COORD_ARGS=""
if [ "$DASHBOARDS" = "1" ] && [ -x "$BIN/bigfleet-web-dashboard" ]; then
  log "starting single-node coordinator (Raft+BoltDB; the dashboard reads it for topology / per-shard providers / needs)"
  start coordinator.log "$BIN/shard" coordinator --bootstrap \
    --listen="$COORD_LISTEN" --raft-bind="$COORD_RAFT" \
    --metrics-addr="$COORD_METRICS" --data-dir="$COORD_DATA"
  sleep 1
  # the shard self-reports its --provider-addr to the coordinator on every ReportShard (ShardSummary.provider_address),
  # which is how the dashboard's per-shard Providers view populates — no separate provider registration.
  COORD_ARGS="--coordinator-addr=$COORD_LISTEN --advertise-addr=$SHARD_LISTEN"
fi

log "starting shard (dials the fakecloud provider — declares BareMetal+Reserved committed + OnDemand+Spot elastic = $FLEET_SIZE)"
# --execute-concurrency: parallel provisioning is safe since BigFleet ADR-0058 keyed the
# kit's fence high-water mark per (shard, machine). Before that, a single shard's concurrent
# execute workers fenced each other (out-of-order seqs against one per-shard mark) and machines
# landed in FAILED, which forced =1 (serial, gradual). Now concurrent ops on different machines
# don't fence, so we run a wide pool to keep a 120-node burst snappy.
# COORD_ARGS is unquoted on purpose (the flags carry no spaces) so it splices in or vanishes cleanly.
start shard.log "$BIN/shard" shard \
  --provider-addr="$PROVIDER_LISTEN" --execute-concurrency=16 \
  --listen="$SHARD_LISTEN" --metrics-addr="$SHARD_METRICS" --data-dir="$RUN/shard-data" \
  $COORD_ARGS
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

# the REAL bigfleet-web-dashboard (read-only) for this session, pointed at the coordinator +
# a per-session Prometheus + the 3 clusters' CRDs — proves it's the real BigFleet engine.
: > "$RUN/bigfleet-dashboard"
if [ "$DASHBOARDS" = "1" ] && [ -x "$BIN/bigfleet-web-dashboard" ]; then
  bash "$HACK_DIR/demo-fleet-dashboard.sh" || true
fi

# session descriptor for the backend
{
  echo "{"
  echo "  \"shardMetrics\": \"$SHARD_METRICS\","
  bfdash=$(cat "$RUN/bigfleet-dashboard" 2>/dev/null || true)
  echo "  \"bigfleetDashboard\": \"$bfdash\","
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
