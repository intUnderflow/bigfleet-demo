# bigfleet-demo

A live, interactive demo of **[BigFleet](https://bigfleet.lucy.sh)** — a fleet-level infrastructure
autoscaler. You add workloads to a few Kubernetes clusters and watch BigFleet **move infrastructure
capacity between them** in real time: provisioning where high-priority demand lands, reclaiming idle
capacity elsewhere, preempting low-priority work under scarcity, and routing interruption-tolerant work to
cheap **spot** while keeping interruption-sensitive work on stable **on-demand**.

It's a real, miniature BigFleet against a *simulated* substrate — and it's honest about which is which.

## What's real, what's simulated

- **Real** — the BigFleet engine and all three phases (assign / preempt / reclaim) with the
  effective-cost and victim-score math; the operator↔shard stream and the `CapacityRequest` /
  `AvailableCapacity` / `UpcomingNode` CRDs; **three out-of-tree `CapacityProvider` backends**
  (on-prem / AWS / GCP) that implement the genuine six-RPC `providerkit` contract and pass the
  conformance suite (`core,cloud,spot`); and real Kubernetes scheduling — the stock upstream
  `kube-scheduler` per cluster (priority, preemption, bin-packing).
- **Simulated** — the cloud itself (`on-prem`/`AWS`/`GCP` are labels on [kwok](https://kwok.sigs.k8s.io/)
  fake nodes, not VMs); the dollar prices and interruption probabilities (illustrative author-chosen
  constants, **not** cloud quotes — the engine *reasons about* them, but no *measured* saving is claimed);
  and provisioning latency (a configured dwell — *"real decision; transfer speed simulated"*). Every node
  and pod is tagged `bigfleet.demo/simulated: "true"`.

## Run it

On a machine with **Docker** and **Go** (nothing else to clone — the demo depends on the published
`bigfleet` / `bigfleet-providers` modules, pinned in `go.mod`):

```sh
hack/demo-up.sh            # 3 kwokctl clusters + shard + 3 providers + controllers + backend + UI
open http://localhost:8090
hack/demo-down.sh          # tear it all down
```

Drive and watch it from the terminal (no browser):

```sh
hack/demo-workload.sh cluster-a 20      # add demand (level 0–40); add 'critical' for the preempting tier
hack/demo-observe.sh                    # snapshot the fleet by tier + the shard's real action counters
```

The full guided walkthrough is the **[demo tour on the docs site](https://bigfleet.lucy.sh/demo)**.

## How it's built

`kwokctl` clusters (real apiserver + stock `kube-scheduler` + kwok nodes) ← per-cluster operator / UPC /
node-creator ← one BigFleet shard ← three fake `providerkit` providers (`providers/fakecloud`, one
multiplexed gRPC server) that mint kwok nodes. The demo control plane (`backend/cmd/demo-backend`) is the
only surface a viewer touches — it streams derived fleet state and mints validated workloads server-side;
the apiserver is never exposed.

The demo depends one-way on the published [bigfleet](https://github.com/intUnderflow/bigfleet) and
[bigfleet-providers](https://github.com/intUnderflow/bigfleet-providers) modules — nothing here is imported
by them.
