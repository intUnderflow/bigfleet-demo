# ADR 0001 — Module layout & sibling pinning (Spike C)

**Status:** accepted (2026-06-22). **Context:** the v1 build plan's Day-0 spike.

## Context

`bigfleet-demo` depends **one-way** on two *unpublished* sibling repos:
`../bigfleet` (the engine: `pkg/shard`, `pkg/operator`, `pkg/provider/fake`, the
`v1alpha1` CRD types) and `../bigfleet-providers` (`providerkit`). They are
wired via `replace` directives — they can never be `go get`'d from a registry.

`bigfleet` pins **`k8s.io/* v0.36` + `sigs.k8s.io/controller-runtime v0.24`**.
The demo also needs a `client-go` to talk to **KCP** (v0.32, ≈ k8s 1.35). The
open question Spike C had to settle: can one demo binary import the engine
*and* a KCP-facing client-go without a version conflict — and is the engine even
importable as a library (the embedded-session model needs `shard.New` /
`operator.New`)?

## Decision

1. **One root Go module** (`github.com/intUnderflow/bigfleet-demo`) for every
   binary that imports the engine or providerkit: `providers/fakecloud`,
   `orchestrator`, `backend`, `hostagent`. **Proven:** `hack/modcheck` compiles
   the engine (`shard.New`, `pkg/operator`, `fake.New`, CRD types) + `providerkit`
   + `client-go` together against one resolved set — **client-go/api/apimachinery
   v0.36.2, controller-runtime v0.24.0, no conflicts**. `client-go v0.36` talking
   to KCP v0.32 is one minor apart and Phase-0 already proved client-go↔KCP works.

2. ~~**`scheduler/` (schedanim) gets its own nested module**, added in **Spike A**,
   to isolate the heavy / version-fragile `k8s.io/kubernetes` scheduler framework
   from the root.~~ **WITHDRAWN 2026-06-23** — the KCP/schedanim direction was set
   aside; the live demo runs on `kwokctl` with the **stock `kube-scheduler` binary**,
   so no in-repo scheduler-framework module is needed. The `scheduler/` module was
   deleted (see the Update at the bottom).

3. **Siblings pinned by SHA** in `ci/sibling-pins.env`; `hack/pin-siblings.sh`
   checks them out at the pin (CI) or verifies-and-warns (local). **`hack/modcheck`
   is the CI drift-guard** — it fails the build if the engine + providerkit +
   client-go ever stop reconciling. Bumping a sibling = update the SHA, run
   `pin-siblings.sh`, re-run the gates (modcheck, conformance `core,cloud,spot`,
   the scenario action-fires check).

## Consequences

- The demo is reproducible and a silent sibling drift becomes a loud CI failure.
- The embedded-session model (Spike B) is **import-feasible** — `shard.New`,
  `pkg/operator`, and `fake.New` are exported libraries (composing N of them in
  one process is still Spike B's job).
- `node-creator`'s `UpcomingNode→Node` reconciler is a `main` (not importable);
  the demo reimplements it (~50 lines, proven in Phase-0) inside the shared pool.
- M1 can proceed: the module foundation it imports is proven.

## Update — 2026-06-23: scheduler module removed

The `scheduler/` module (the `schedanim` KCP scheduler-framework spike) has been
**deleted**. The live demo pivoted to **`kwokctl` clusters running the stock upstream
`kube-scheduler` binary** (`hack/scheduler-config.yaml`: NodeResourcesFit +
`MostAllocated`), which schedules and preempts real pods against kwok fake nodes
without needing an in-repo scheduler. The custom-binder approach was only required on
KCP (no `/binding` subresource on CRD-style pods); on kwokctl it isn't.

Historical note (Spike A, 2026-06-22): the spike *did* prove `k8s.io/kubernetes v1.36.2`
is importable as a library (the full framework stack reconciled with `multicluster-runtime`
+ `multicluster-provider` + `client-go v0.36.2`, at the cost of 33 staging `replace`
directives). That finding stands if the KCP substrate is ever revisited — but the code is
gone from the tree (it was never committed). Decision **#1** (one root module) remains in force.

## Update — 2026-06-23: sibling pinning superseded (Decision #3 reversed)

`bigfleet` and `bigfleet-providers` are **published** repos, so the demo no longer treats them as
unpublished siblings resolved by local `replace` + SHA pins. It now requires them as **ordinary Go
modules**, pinned by version in `go.mod` (a concrete version + `go.sum` hash = reproducible builds;
bump with `go get <module>@latest`). A fresh clone is `go build`-able with no sibling checkout, and
`hack/demo-up.sh` builds the engine binaries + applies the CRDs straight from the published module cache.
**Removed:** `ci/sibling-pins.env`, `hack/pin-siblings.sh`, `hack/modcheck` — `go build ./...` is the
drift check now. (Local engine development: add a gitignored `go.work` over `../bigfleet`.)
