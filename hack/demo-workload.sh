#!/usr/bin/env bash
# Drive demand on a cluster from the terminal — the same /api/demand path the UI
# buttons use, so BigFleet provisions (or reclaims) capacity for it. Demand is a
# LEVEL (0-40 node-equivalents), per tier:
#   demand   = everyday production (interruption-tolerant → routes to cheap spot)
#   critical = high-priority (interruption-sensitive → on-demand; preempts batch)
#
#   hack/demo-workload.sh cluster-a 20            # production demand, level 20
#   hack/demo-workload.sh cluster-a 8 critical    # critical demand, level 8
#   hack/demo-workload.sh cluster-a 0             # clear this cluster's demand tier
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"
c="${1:?usage: demo-workload.sh <cluster> <level 0-40> [demand|critical]}"
level="${2:-12}"
tier="${3:-demand}"
[ -f "$RUN/$c.kubeconfig" ] || die "no session for $c (run demo-up.sh first)"

curl -s -m5 -XPOST "http://$BACKEND_ADDR/api/demand" -H 'content-type: application/json' \
  -d "{\"workspace\":\"$c\",\"level\":$level,\"tier\":\"$tier\"}" >/dev/null \
  && log "set $tier demand on $c to level $level — watch hack/demo-observe.sh" \
  || die "backend not reachable on $BACKEND_ADDR (is the stack up?)"
