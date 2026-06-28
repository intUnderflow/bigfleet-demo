#!/usr/bin/env bash
# Snapshot the fleet from the terminal: per-cluster node count + tier breakdown
# (from the provider-declared bigfleet.demo/billing label) + pending pods, then the
# shard's REAL inventory and action counters. Run it repeatedly (or under `watch`)
# to see BigFleet move capacity. No browser needed.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"

for c in "${CLUSTERS[@]}"; do
  [ -f "$RUN/$c.kubeconfig" ] || continue
  nodes=$(ckubectl "$c" get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')
  tiers=$(ckubectl "$c" get nodes -o jsonpath='{range .items[*]}{.metadata.labels.bigfleet\.demo/billing}{"\n"}{end}' 2>/dev/null \
            | sort | uniq -c | awk '{printf "%s:%s ",$2,$1}')
  pending=$(ckubectl "$c" get pods -A --field-selector=status.phase=Pending --no-headers 2>/dev/null | wc -l | tr -d ' ')
  printf "  %-10s nodes=%-3s [ %s] pending=%s\n" "$c" "$nodes" "${tiers:-—}" "$pending"
done

echo "  --- shard inventory (REAL: BigFleet's declared capacity, by type+state) ---"
curl -s "$SHARD_METRICS/metrics" 2>/dev/null \
  | grep '^bigfleet_shard_inventory_machines{' \
  | sed -E 's/.*capacity_type="([^"]*)".*state="([^"]*)".* ([0-9]+)$/\2 \1 \3/' \
  | awk '{s[$1" "$2]+=$3} END{for(k in s) if(s[k]>0) print "    "k" = "s[k]}' | sort \
  || echo "    (shard metrics unreachable — is the stack up?)"

echo "  --- shard actions (REAL: Bootstrap/Provision/Reclaim/Preempt) ---"
curl -s "$SHARD_METRICS/metrics" 2>/dev/null \
  | grep '^bigfleet_shard_actions_total{' | grep -v ' 0$' | sed 's/^/    /' \
  || true
