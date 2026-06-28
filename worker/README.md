# bigfleet-demo coordinator (Cloudflare Worker)

The public front door at **bigfleet-demo.lucy.sh** and the only thing that creates demo
sessions. It knows the **runners** (the [`demohost`](../docs/operating-the-host.md) daemons)
by their internet addresses and live capacity, **gates every new visitor behind reCAPTCHA**,
assigns a passing visitor — keyed by **IP** — to a session on a runner with headroom, and
reverse-proxies their traffic to that session for its life.

## Two ways in, both gated

```
A. Homepage "Demo" button (bigfleet.lucy.sh)      B. Direct hit (typed bigfleet-demo.lucy.sh)
   reCAPTCHA v3 runs invisibly on click              the Worker serves a gate page that
   → /?rc=<v3-token>                                  runs reCAPTCHA v3 invisibly, and only
   the Worker scores it:                              if the score is low shows a v2 checkbox.
     score ≥ MIN and a slot free → session            Passing v3 or v2:
     else → 302 to bigfleet.lucy.sh/demo (tour)         slot free  → session
                                                         fleet FULL → FIFO queue: live position
   (casual intent: low score just gets the tour)                     + a tour link; dropped in
                                                                     when a slot frees
                                                       (no/low token → the tour)
```

Why v2 only on the direct hit: typing the demo URL is strong intent, so a real human with a
poor v3 score (VPN, privacy browser, fresh network) gets a second chance at the interactive
checkbox — while a bot meets a real challenge. The homepage button is casual, so a low score
just falls through to the tour.

Once a visitor holds a session, every request is reverse-proxied to it
(`bigfleet-demo.lucy.sh/… → https://runnerN/s/<id>/…`). When the session ends (the 1-hour cap,
or ~5 min after the tab closes) the runner serves `410`; the Worker drops the mapping and
sends the visitor back to the front door to **re-enter through the gate** — it never silently
mints another slot, so one IP can't camp a scarce session.

- **State (Cloudflare KV):** `ip:<ip> → {runner,id,expiresAt}` (the session mapping,
  auto-expiring at session end) and `q:<ip> → {joinedAt}` (a queue ticket, ordered by join
  time, with a ~2-min TTL the queue page refreshes by polling — stop polling ~2 min and you
  lose your place).
- **Capacity** is the runner's truth: the Worker reads `GET /v1/capacity` to rank runners and
  falls through to the next on a `503` from `POST /v1/sessions`.
- **Single origin:** visitors stay on `bigfleet-demo.lucy.sh`; the Worker proxies everything
  (SSE included), so URL/TLS are preserved and raw runner ports are never exposed. The
  `/s/<id>` hop is invisible — which is why the per-session Kubernetes dashboards, served by
  the backend at a relative `/dash/a|b|c/`, just work through the proxy.

## Endpoints

- `/` — proxy if the IP holds a session; else the gate page (direct hit), or a clean redirect
  after resolving a homepage `?rc=` token.
- `POST /_gate` — the gate page posts its v3/v2 token here; replies `{ok}` (assigned),
  `{needV2}` (low v3 → reveal the checkbox), `{queued, position}` (fleet full), or `{fail}`.
- `POST /_queue` — the queue page polls this (~20s): refreshes the ticket, returns the live
  `{queued, position}`, and `{ready}` once it reaches the front and a slot frees.
- `/_coordinator/health` — liveness.

## reCAPTCHA setup

- The path-A token is minted on **bigfleet.lucy.sh** by `site/src/components/DemoGate.astro`
  (in the `bigfleet` repo), which intercepts the "Demo" hero button. The path-B gate page mints
  its v3 token on **bigfleet-demo.lucy.sh** — so add **both** hosts to the reCAPTCHA **v3**
  site's allowed Domains.
- The **v2** checkbox site is registered for **bigfleet-demo.lucy.sh**. Both site keys are
  public (they ship in client HTML, embedded in the Worker / the Astro component); the secrets
  are Worker secrets (below).

## Deploy

**0. Stand up runners.** On each box run the daemon and expose its port over HTTPS with a
named hostname (one tunnel per runner; production runs both under `launchd` — see
[operating-the-host.md](../docs/operating-the-host.md)):

```sh
# on the runner
./bin/demohost serve --addr 127.0.0.1:8080 --demo-memory-mb 12000 \
  --session-memory-mb 2048 --max-sessions 5 --warm-pool 2 --dashboards
cloudflared tunnel --hostname r1-bigfleet-demo.lucy.sh --url http://localhost:8080
```

`demohost`'s `/v1/*` stays key-gated (the public gets 401); `/s/<id>/*` is the visitor surface.

**1. KV namespace:**

```sh
cd worker
npx wrangler kv namespace create SESSIONS    # paste the id into wrangler.toml ([[kv_namespaces]])
```

**2. Runners** — set `RUNNERS` in `wrangler.toml` to your tunnel URLs:

```toml
[vars]
RUNNERS = '["https://r1-bigfleet-demo.lucy.sh","https://r2-bigfleet-demo.lucy.sh"]'
RECAPTCHA_MIN_SCORE = "0.5"   # v3 score a casual homepage click must clear
```

**3. Secrets** — three of them (for a Workers-Builds git deploy, set these in the dashboard
under *Settings → Variables and Secrets* instead of the CLI):

```sh
npx wrangler secret put DEMOHOST_KEY        # the runners' shared key (../secrets/demohost.key)
npx wrangler secret put RECAPTCHA_SECRET     # reCAPTCHA v3 secret
npx wrangler secret put RECAPTCHA_V2_SECRET  # reCAPTCHA v2 (checkbox) secret
```

> **Until `RECAPTCHA_SECRET` is set the demo is closed** — every token fails verification, so
> all visitors fall through to the tour.

**4. Route + deploy** — bind the Worker to the hostname (uncomment `[[routes]]` in
`wrangler.toml`, set `zone_name`), then `npx wrangler deploy` (or push, if Workers Builds is
wired to the repo).

## Local dev

`wrangler dev` simulates KV and reads `worker/.dev.vars` (git-ignored). The reCAPTCHA gate
needs a real domain, so for local work either point a runner at the Worker and exercise the
proxy/queue paths directly, or drop in Google's reCAPTCHA **test keys** (which always pass).

```sh
# worker/.dev.vars
DEMOHOST_KEY=<contents of ../secrets/demohost.key>
RECAPTCHA_SECRET=<v3 secret or a test key>
RECAPTCHA_V2_SECRET=<v2 secret or a test key>
RUNNERS=["http://localhost:8080"]
```

## Notes & caveats

- **Identity is the IP** (`CF-Connecting-IP`), by design. Visitors behind one NAT/CGNAT (an
  office, a campus) share a session **and** a queue place — fine for a public demo, worth knowing.
- **The queue is approximately FIFO.** It orders by `joinedAt` and counts tickets with a KV
  `list`, which is eventually consistent — so a position can be off by one and fairness is
  best-effort. A Durable Object would make it exact; KV is plenty at demo scale, and the
  runner's own admission (`503` when full) caps any promotion race, so it never over-allocates.
- **One key** across runners (`DEMOHOST_KEY`). Per-runner keys would be a small change (a
  `{url,key}` shape in `RUNNERS`).
- The Worker holds no long-lived per-session state beyond the KV mapping + queue tickets; the
  runner's reaper remains the single owner of session lifetime.
