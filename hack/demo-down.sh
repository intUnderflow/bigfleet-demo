#!/usr/bin/env bash
# Tear down the demo stack: kill host processes, delete the kwokctl clusters and
# dashboards, remove run/ state.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"
if [ -f "$RUN/pids" ]; then
  while read -r pid; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done < "$RUN/pids"
fi
# belt-and-suspenders — match however each was launched (abs path, ./bin, or $BIN),
# so a manually-restarted process (e.g. a backend started outside run/pids) is killed too.
for b in node-creator fakecloud-provider shard operator upc demo-backend; do
  pkill -f "bin/$b" 2>/dev/null || true
done
# stop the per-cluster dashboards
for c in "${CLUSTERS[@]}"; do docker rm -f "dash-$c" >/dev/null 2>&1 || true; done
# delete the real clusters (kwokctl removes their containers)
for c in "${CLUSTERS[@]}"; do kwokctl delete cluster --name "$c" >/dev/null 2>&1 || true; done
sleep 1
rm -rf "$RUN"
log "demo stack down."
