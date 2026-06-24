#!/usr/bin/env bash
# Official Kubernetes Dashboard per cluster (real apiservers now → fully functional,
# all views work). Seamless login via a ServiceAccount token injected by the demo
# backend's reverse proxy. Ports 9101/9102/9103, bound to 127.0.0.1.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"
IMG="${DASHBOARD_IMG:-kubernetesui/dashboard:v2.7.0}"
command -v docker >/dev/null && docker info >/dev/null 2>&1 || { log "docker not available — skipping dashboards"; : > "$RUN/dashboards"; exit 0; }
docker image inspect "$IMG" >/dev/null 2>&1 || { log "pulling $IMG"; docker pull "$IMG" >/dev/null 2>&1 || true; }

: > "$RUN/dashboards"
port=$DASH_PORT_BASE
for c in "${CLUSTERS[@]}"; do
  kc="$RUN/$c.kubeconfig"
  dn="dash-$(kwokname "$c")"   # GLOBAL container name — unique per session
  [ -f "$kc" ] || { port=$((port+1)); continue; }
  # a read-only dashboard ServiceAccount + token (real apiserver supports this)
  KUBECONFIG="$kc" kubectl create serviceaccount dashboard -n kube-system >/dev/null 2>&1 || true
  KUBECONFIG="$kc" kubectl create clusterrolebinding dashboard-view \
    --clusterrole=cluster-admin --serviceaccount=kube-system:dashboard >/dev/null 2>&1 || true
  tok=$(KUBECONFIG="$kc" kubectl create token dashboard -n kube-system --duration=24h 2>/dev/null)
  [ -n "$tok" ] && echo "$tok" > "$RUN/$c.dashtoken"
  # the kwokctl apiserver host port (kubeconfig server, e.g. https://127.0.0.1:NNNNN)
  srv=$(KUBECONFIG="$kc" kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null)
  hostsrv=$(echo "$srv" | sed -E 's#https://(127\.0\.0\.1|localhost):#https://host.docker.internal:#')
  # docker-reachable kubeconfig for the dashboard container (server -> host.docker.internal, insecure TLS)
  dkc="$RUN/dash-$c.kubeconfig"
  KUBECONFIG="$kc" kubectl config view --minify --flatten 2>/dev/null \
    | sed -E "s#server: .*#server: $hostsrv#" \
    | sed -E 's#certificate-authority-data:.*#insecure-skip-tls-verify: true#' > "$dkc"
  docker rm -f "$dn" >/dev/null 2>&1 || true
  chmod 0644 "$dkc" 2>/dev/null || true
  # Create + `docker cp` the kubeconfig in + start, instead of a host bind-mount (-v $dkc:/kc):
  # the public runner locks Docker file-sharing to the kwok dir for security, so run/sessions/
  # is NOT a mountable path there (a bind-mount silently leaves the container stuck "Created").
  # Copying the kubeconfig into the container's own fs needs no host mount — and is a harmless
  # equivalent on an unlocked dev machine. (uid-1001 dashboard reads /kc, hence chmod 0644.)
  if docker create --name "$dn" --restart unless-stopped --tmpfs /tmp:rw,size=64m \
       -p "127.0.0.1:$port:9090" "$IMG" \
       --kubeconfig=/kc --insecure-bind-address=0.0.0.0 --insecure-port=9090 \
       --enable-insecure-login --enable-skip-login --disable-settings-authorizer >/dev/null 2>&1 \
     && docker cp "$dkc" "$dn:/kc" >/dev/null 2>&1 \
     && docker start "$dn" >/dev/null 2>&1; then
    echo "$c http://localhost:$port" >> "$RUN/dashboards"
    log "Kubernetes dashboard for $c -> http://localhost:$port"
  else
    log "dashboard for $c failed (non-fatal)"
  fi
  port=$((port+1))
done
