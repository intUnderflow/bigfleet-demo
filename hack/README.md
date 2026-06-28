# hack/ — dev scripts

## Run the demo (single host, one command)

```sh
hack/demo-up.sh            # builds binaries, starts 3 kwokctl clusters + shard + 3 providers + per-cluster controllers + backend + UI
open http://localhost:8090
#   press "Add demand to cluster-A" and watch BigFleet provision nodes and move
#   capacity across the fleet — narrated honestly in the decision feed.
hack/demo-down.sh          # tear it all down
```

Drive + watch it from the terminal (no browser): `hack/demo-workload.sh <cluster> <level> [demand|critical]`
sets a cluster's demand (via the same `/api/demand` the UI uses); `hack/demo-observe.sh` snapshots the
fleet — per-cluster nodes by tier + the shard's real inventory/action counters. The full guided terminal
tour is the demo guide on the docs site (`bigfleet.lucy.sh/demo`).

Prereqs: `go`, `curl`, and **Docker** (kwokctl runs each cluster's kube-apiserver in a container).
`kwokctl` + `kwok` are auto-downloaded on first run. Nothing else to clone — the BigFleet engine and
providerkit come from the published `bigfleet` / `bigfleet-providers` modules (pinned in `go.mod`), which
`go build` fetches on demand.

## What's running (the demo stack)

- **3 kwokctl clusters** (`cluster-a/b/c`) — each a real kube-apiserver + the **stock upstream
  `kube-scheduler`** (NodeResourcesFit + `MostAllocated` bin-packing, via `hack/scheduler-config.yaml`)
  + kwok fake nodes.
- **1 BigFleet shard** — the real engine. It dials **3 real `CapacityProvider` backends** (on-prem / AWS /
  GCP — `cmd/fakecloud-provider`, one multiplexed `providerkit` server, conformance-certified
  `core,cloud,spot`) over `--provider-addr`; they declare the four capacity types (committed BareMetal +
  Reserved, elastic OnDemand + Spot) and mint kwok nodes.
- per cluster: the BigFleet **operator** + **UPC** + **node-creator** (`cmd/node-creator` — turns each
  Ready `UpcomingNode` into a cloud-realistic kwok Node, tagged `bigfleet.demo/simulated`).
- **demo-backend** (`backend/cmd/demo-backend`) — the only surface a viewer touches: tails the shard's
  real `/metrics` + each workspace's nodes/UpcomingNodes/pods, streams derived `FleetState` to the
  browser over SSE, synthesizes the decision feed, and mints validated workloads server-side.
- **ui/index.html** — Shared Supply Bar (centerpiece) + Fleet Map + Decision Feed + the always-on
  "what's real / simulated" panel.

**Real:** BigFleet's engine, its Phase 1/2/3 decisions, the shard actions + inventory, the CRDs.
**Simulated:** the cloud, the nodes (kwok fakes), prices/interruption odds, and **transfer speed**.

State (`run/`) and built binaries (`bin/`) are gitignored. To run this stack many times over on one
machine behind a public front door, see [`docs/operating-the-host.md`](../docs/operating-the-host.md)
(the `demohost` daemon) and [`worker/README.md`](../worker/README.md) (the coordinator).
