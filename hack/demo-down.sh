#!/usr/bin/env bash
# Tear down the demo stack: kill host processes, delete the kwokctl clusters and
# dashboards, remove run/ state.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"
if [ -f "$RUN/pids" ]; then
  while read -r pid; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done < "$RUN/pids"
fi
# belt-and-suspenders for the SINGLE-session local demo only — match however each was
# launched, so a manually-restarted process (started outside run/pids) is killed too.
# NEVER do this when SESSION_ID is set: sessions share the bin/ dir, so a broad pkill
# would tear down sibling sessions. There, $RUN/pids is the authoritative process list.
if [ -z "$SESSION_ID" ]; then
  # bin/shard matches BOTH the shard and the coordinator (both are `bin/shard <subcmd>`).
  for b in node-creator fakecloud-provider shard operator upc demo-backend bigfleet-web-dashboard; do
    pkill -f "bin/$b" 2>/dev/null || true
  done
  pkill -f "config.file=$RUN/prometheus.yml" 2>/dev/null || true
fi
# stop the per-cluster dashboards + delete the real clusters (kwokctl removes their
# containers) — by GLOBAL name so only THIS session's clusters/containers are removed.
for c in "${CLUSTERS[@]}"; do docker rm -f "dash-$(kwokname "$c")" >/dev/null 2>&1 || true; done
for c in "${CLUSTERS[@]}"; do kwokctl delete cluster --name "$(kwokname "$c")" >/dev/null 2>&1 || true; done
sleep 1
rm -rf "$RUN"
log "demo stack down."
