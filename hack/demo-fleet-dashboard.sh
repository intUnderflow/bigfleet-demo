#!/usr/bin/env bash
# The REAL bigfleet-web-dashboard (read-only) for this session, started as two host
# processes — a per-session Prometheus scraping this session's shard+coordinator /metrics,
# and the dashboard binary pointed at that Prometheus + the coordinator + the 3 clusters'
# CRDs. The coordinator itself is started by demo-up.sh (the shard registers with it). This
# is the "prove it's real BigFleet" artifact: the engine's own product UI, reading the real
# control-plane telemetry this session emits. Read-only by construction — it cannot mutate.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"

start(){ local log="$1"; shift; nohup "$@" >"$RUN/$log" 2>&1 & echo $! >> "$RUN/pids"; }

[ -x "$BIN/bigfleet-web-dashboard" ] || { log "fleet-dash: dashboard binary missing — skipping"; exit 0; }
command -v prometheus >/dev/null 2>&1 || { log "fleet-dash: prometheus not installed — skipping (dashboard's fleet/shards/finops views need it)"; exit 0; }

# 1) Read-only multi-context kubeconfig: one context PER cluster, context name == cluster_id
#    (the dashboard takes the context name verbatim as the BigFleet cluster_id). Each context
#    authenticates as a SA bound to a get/list/watch-only ClusterRole on the 3 BigFleet CRDs —
#    NOT cluster-admin like the k8s-dashboard SA. Server is the kwokctl apiserver on 127.0.0.1
#    (the dashboard is a host process, so no host.docker.internal rewrite needed).
parts=()
for c in "${CLUSTERS[@]}"; do
  kc="$RUN/$c.kubeconfig"
  [ -f "$kc" ] || continue
  KUBECONFIG="$kc" kubectl apply -f - >/dev/null 2>&1 <<'YAML' || true
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: bigfleet-dashboard-reader
rules:
- apiGroups: ["bigfleet.lucy.sh"]
  resources: ["capacityrequests", "upcomingnodes", "availablecapacities"]
  verbs: ["get", "list", "watch"]
YAML
  KUBECONFIG="$kc" kubectl create serviceaccount fleet-dash-reader -n kube-system >/dev/null 2>&1 || true
  KUBECONFIG="$kc" kubectl create clusterrolebinding fleet-dash-reader \
    --clusterrole=bigfleet-dashboard-reader --serviceaccount=kube-system:fleet-dash-reader >/dev/null 2>&1 || true
  tok=$(KUBECONFIG="$kc" kubectl create token fleet-dash-reader -n kube-system --duration=24h 2>/dev/null)
  srv=$(KUBECONFIG="$kc" kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null)
  [ -n "$tok" ] && [ -n "$srv" ] || { log "fleet-dash: no token/server for $c (skipping that context)"; continue; }
  pkc="$RUN/fleet-dash-$c.kubeconfig"
  cat > "$pkc" <<YAML
apiVersion: v1
kind: Config
clusters:
- name: $c
  cluster:
    server: $srv
    insecure-skip-tls-verify: true
users:
- name: $c
  user:
    token: $tok
contexts:
- name: $c
  context:
    cluster: $c
    user: $c
current-context: $c
YAML
  parts+=("$pkc")
done
[ ${#parts[@]} -gt 0 ] || { log "fleet-dash: no cluster kubeconfigs; skipping"; exit 0; }
fdkc="$RUN/fleet-dash.kubeconfig"
KUBECONFIG=$(IFS=:; echo "${parts[*]}") kubectl config view --flatten > "$fdkc" 2>/dev/null

# 2) Minimal Prometheus scraping THIS session's shard + coordinator /metrics. The Shards view
#    groups by (pod) and filters component="shard", so stamp those labels; fleet/finops don't need them.
cat > "$RUN/prometheus.yml" <<YAML
global:
  scrape_interval: 15s
scrape_configs:
- job_name: bigfleet-shard
  static_configs:
  - targets: ["$SHARD_METRICS"]
    labels:
      pod: bigfleet-shard-0
      component: shard
- job_name: bigfleet-coordinator
  static_configs:
  - targets: ["$COORD_METRICS"]
    labels:
      component: coordinator
YAML
log "starting per-session Prometheus (scrapes shard+coordinator /metrics) on 127.0.0.1:$PROM_PORT"
start prometheus.log prometheus --config.file="$RUN/prometheus.yml" \
  --storage.tsdb.path="$RUN/promdata" --storage.tsdb.retention.time=45m \
  --web.listen-address="127.0.0.1:$PROM_PORT"

# 3) The dashboard (plaintext loopback — no --tls-*; matches the demo's plaintext shard/coordinator).
log "starting bigfleet-web-dashboard on 127.0.0.1:$FLEETDASH_PORT (served at /fleet-dash/)"
start fleet-dash.log "$BIN/bigfleet-web-dashboard" \
  --listen="127.0.0.1:$FLEETDASH_PORT" \
  --prometheus-url="http://127.0.0.1:$PROM_PORT" \
  --coordinator-addr="$COORD_LISTEN" \
  --kubeconfig="$fdkc"
echo "http://localhost:$FLEETDASH_PORT" > "$RUN/bigfleet-dashboard"
