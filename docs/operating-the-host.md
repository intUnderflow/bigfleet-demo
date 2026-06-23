# Operating the demo host (`demohost`)

`demohost` is the program that lives on a machine and runs **many isolated BigFleet demo
sessions at once**. It is the single point that may create a session — every create is gated
by a secret key, so whoever holds the key is the **central coordinator**. Each session is a
full demo stack (3 `kwokctl` clusters + shard + 3 fakecloud providers + per-cluster
controllers + backend/UI) on its own port block; it runs for **up to an hour** and is reaped
a few minutes after its browser tab goes away. The host reserves a fixed slice of memory to
demos and **refuses any session that would push past that slice**, so the box never tips over.

> One session in isolation is `hack/demo-up.sh` (see the [demo tour](https://bigfleet.lucy.sh/demo)).
> `demohost` is how you run that, many times over, safely, on one server.

## The model

```
            secret key (X-Demo-Key)
 coordinator ───────────────────────▶  demohost serve  ─┬─▶  session abcde  → http://host:21000
 (holds key)                            (one per box)    ├─▶  session f7h2k  → http://host:21016
                                                         └─▶  session …      → http://host:2106N
                                                  admission: reserve ≤ --demo-memory-mb
                                                  reaper: 1h hard cap · ~5m idle · crash
```

- **Creation is key-gated.** `POST /v1/sessions` requires the key. The public never calls
  this directly — a trusted coordinator (today: you, or a thin front door that holds the key)
  does, so it can rate-limit and pick a host with capacity. The key-gated boundary is what
  lets one coordinator fan out across many hosts later.
- **Each session is fully isolated** — its own kwokctl clusters (`<id>-cluster-a/b/c`), its
  own shard/providers/controllers/backend, its own port block. Demand in one never touches
  another (verified: two sessions, independent shards and fleets).
- **The apiserver is never exposed.** Visitors only ever touch a session's demo backend on
  its advertised port, exactly as in the single-session demo.

## The secret key

The key is generated locally and mirrored to the repo's GitHub Actions secret:

- **Local:** `secrets/demohost.key` (git-ignored, `chmod 600`). `demohost` reads it by default.
- **CI / deploy:** the GitHub Actions secret **`DEMOHOST_KEY`**. A workflow injects it as the
  `DEMOHOST_KEY` environment variable, which `demohost` also reads.

Resolution order (daemon and client both): `--key` flag → `$DEMOHOST_KEY` → `--key-file`
(default `secrets/demohost.key`). Whitespace is trimmed, so a trailing newline never corrupts
the comparison. To rotate: regenerate the file and re-push the secret —

```sh
umask 077
openssl rand -hex 32 | tr -d '\n' > secrets/demohost.key
gh secret set DEMOHOST_KEY --repo intUnderflow/bigfleet-demo < secrets/demohost.key
```

## Running it

```sh
# on a Docker- + Go-capable box, from the repo root
./bin/demohost serve \
  --addr :8080 \
  --demo-memory-mb 16384 \      # total RAM reserved to demos — admission never exceeds this
  --session-memory-mb 2048 \    # reserved per session (measured ~1.9 GB/session, see below)
  --max-sessions 8 \            # hard backstop on concurrency
  --session-ttl 1h \            # hard lifetime
  --idle-timeout 5m \           # reaped this long after the tab stops heart-beating
  --advertise-host demo.example.com   # host part of returned session URLs
```

`serve` builds the shared demo binaries once (`hack/demo-build.sh`), then listens. On
Ctrl-C / SIGTERM it reaps every live session before exiting (no leaked clusters).

Drive it with the client subcommands (same key resolution):

```sh
export DEMOHOST_KEY=$(cat secrets/demohost.key)
./bin/demohost create        # → prints the session id, URL, and expiry
./bin/demohost ls            # live sessions + time left
./bin/demohost capacity      # machine RAM / budget / reserved / measured / headroom
./bin/demohost rm <id>       # tear one down now
```

## Session lifetime & the UI top bar

A hosted session ends at whichever comes first:

- **the 1-hour hard cap**, or
- **~5 minutes after the browser tab goes away.** The UI POSTs `/api/heartbeat` every ~20s
  while its tab is visible; when the heartbeats stop, the session's idle clock runs out and
  the reaper removes it.

The UI shows a **top bar** with a live countdown to the hard deadline, and a full-screen
"this session has ended" overlay when the clock runs out. The standalone local demo passes no
`--session-id`, reports `hosted:false`, and shows none of this.

Two clocks, two owners: the **backend** owns the truth (`/api/session` reports `expired`),
and the **host** reaper obeys it — but the host also enforces the 1-hour cap itself, and
reaps any session whose backend has been unreachable for >60s, so a crashed backend can't
leak a stack.

## Memory: reserve, and don't blow past it

Admission lets a new session start only if **both** hold:

1. **Reservation budget** — `(running + 1) × --session-memory-mb ≤ --demo-memory-mb`. This is
   the primary, deterministic guard: you decide how much RAM demos may use, and the host
   never reserves past it.
2. **Measured actual** — the host samples the *real* footprint every 15s (host-process RSS via
   `ps` + the kwok containers via `docker stats`) and also refuses if
   `measured + --session-memory-mb > --demo-memory-mb`. So even if sessions run heavier than
   their reservation, real usage still can't cross the budget.

`capacity` reports both the reservation and the measured actual, so the gap is always visible.

> **Honesty:** `--session-memory-mb` is a **reservation** used for admission, **not** a live
> cgroup cap on each session. The measured-actual cross-check is what actually keeps real
> usage under the budget; the reservation just decides how many we *admit*. Pick the
> reservation from a real measurement on the target box, not this dev figure.

### Measured footprint (dev Mac, Docker Desktop)

A single session measured **~1.85 GB**: ~1.4 GB across its 15 kwok containers (etcd /
apiserver / controller-manager / scheduler / kwok-controller × 3 clusters) + ~0.47 GB of
host processes (shard, 3 providers, 3×{operator, UPC, node-creator}, backend). Early in a
session's life it's lower (~1.1 GB) and it climbs as nodes/pods seed. The default
`--session-memory-mb 2048` leaves headroom over the measured peak. **Re-measure on the prod
host** (native Linux cgroups differ from Docker Desktop's VM) and set the reservation from
that — `demohost capacity` gives you the measured number to calibrate against.

To stay lean, hosted sessions run with the per-cluster Kubernetes dashboards **off** by
default (they'd add 3 containers/session); pass `--dashboards` to re-enable them.

## Reaching sessions: the `/s/{id}` proxy + the coordinator

`demohost` serves a public **`/s/{id}/…`** reverse-proxy to each session's backend (it strips
the prefix and streams SSE). A reaped/unknown id returns **`410 Gone`** so a coordinator knows
to hand the visitor a fresh session. So one HTTPS tunnel per runner, pointed at the demohost
port, exposes every session — no raw per-session ports on the internet. (`/v1/*` stays
key-gated; `/s/*` is the visitor surface.)

The public front door is the **Cloudflare Worker coordinator** (`worker/`, served at
`bigfleet-demo.lucy.sh`): it holds the key, maps each visitor IP → a session on a runner with
capacity, proxies them in, and re-assigns when a session ends. See
[`worker/README.md`](../worker/README.md) for the architecture and deploy steps.

`demohost create` still returns the direct `http://<advertise-host>:<portBase>` URL, handy
for local/manual use; the coordinator uses the `/s/{id}` path instead.

## Honesty, unchanged

Everything the [honesty mandate](https://bigfleet.lucy.sh/demo) requires still holds per
session: real BigFleet engine + real Kubernetes scheduling, simulated (kwok) cloud, illustrative
prices, "real decision; transfer speed simulated", and the always-on what's-real/simulated
panel. `demohost` only changes *how many* of these run at once and *who* may start one — it
adds no new claims.
