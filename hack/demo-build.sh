#!/usr/bin/env bash
# Build the SHARED demo binaries once, and make sure kwokctl/kwok are present. The
# demohost daemon runs this at startup, then spawns each session with SKIP_BUILD=1 so
# every session reuses bin/ instead of rebuilding (which would be wasteful and racy).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/demo-common.sh"
ensure_kwok
SKIP_BUILD=0 build_bins
log "shared demo binaries ready in $BIN"
