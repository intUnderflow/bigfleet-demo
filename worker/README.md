# bigfleet-demo coordinator (Cloudflare Worker)

The public front door at **bigfleet-demo-api.lucy.sh**. It knows the **runners** (the
[`demohost`](../docs/operating-the-host.md) daemons) by their internet addresses and live
capacity, assigns each visitor — keyed by **IP** — to a session on a runner with headroom,
and reverse-proxies their traffic to that session. When a session ends (the 1-hour cap, or
~5 min after the tab closes), the runner serves `410` for it and the visitor's next request
gets a **fresh** session on a runner that still has room.

```
 visitor ──▶ bigfleet-demo-api.lucy.sh (this Worker)
              │  1. IP in KV?  ── yes ─▶ proxy ──▶ https://runnerN/s/<id>/…
              │                 ── no  ─▶ interstitial → POST /_coordinator/assign
              │                                            │ pick runner with most headroom
              │                                            │ POST runnerN/v1/sessions  (X-Demo-Key)
              │                                            └ store ip→{runner,id} in KV (TTL=session)
              └  proxy gets 410/502 ─▶ drop mapping, assign fresh, retry
```

- **State:** Cloudflare **KV** — `ip → {runner,id,expiresAt}`, auto-expiring at session end.
- **Capacity:** the runner is the source of truth. The Worker reads `GET /v1/capacity` to
  rank runners, and if a `POST /v1/sessions` races to `503` it falls through to the next.
- **Single origin:** visitors stay on `bigfleet-demo-api.lucy.sh`; the Worker proxies (SSE and
  all), so the URL and TLS are preserved and raw runner ports are never exposed.

## Deploy

**0. Stand up a runner.** On each box (e.g. a Mac Mini) run the daemon and expose its port
over HTTPS with a named hostname (one tunnel per runner):

```sh
# on the runner
./bin/demohost serve --addr :8080 --demo-memory-mb 16384 --session-memory-mb 2048
cloudflared tunnel --hostname r1.bigfleet-demo.lucy.sh --url http://localhost:8080
```

`demohost`'s `/v1/*` stays key-gated (the public gets 401); `/s/<id>/*` is the visitor
surface. For defence in depth you can also put Cloudflare Access in front of `/v1/*` so only
this Worker can reach it.

**1. KV namespace:**

```sh
cd worker
npx wrangler kv namespace create SESSIONS
npx wrangler kv namespace create SESSIONS --preview
# paste both ids into wrangler.toml ([[kv_namespaces]] id / preview_id)
```

**2. Runners** — set `RUNNERS` in `wrangler.toml` to your tunnel URLs:

```toml
[vars]
RUNNERS = '["https://r1.bigfleet-demo.lucy.sh","https://r2.bigfleet-demo.lucy.sh"]'
```

**3. The coordinator key** (same key the runners use, from `../secrets/demohost.key`):

```sh
npx wrangler secret put DEMOHOST_KEY     # paste the key when prompted
```

**4. Route** — bind the Worker to the hostname (uncomment `[[routes]]` in `wrangler.toml`,
set `zone_name`), then:

```sh
npx wrangler deploy
```

## Local dev

`wrangler dev` simulates KV locally and reads `worker/.dev.vars` (git-ignored) for the key +
a localhost runner override:

```sh
# worker/.dev.vars
DEMOHOST_KEY=<contents of ../secrets/demohost.key>
RUNNERS=["http://localhost:8080"]
```

```sh
# terminal 1: a runner
./bin/demohost serve --addr :8080 --demo-memory-mb 8192 --session-memory-mb 2048
# terminal 2: the coordinator
cd worker && npx wrangler dev --local --port 8787
# terminal 3: act as a visitor (set your IP)
curl -XPOST localhost:8787/_coordinator/assign -H 'CF-Connecting-IP:1.2.3.4'
curl localhost:8787/api/session -H 'CF-Connecting-IP:1.2.3.4'
```

## Notes & caveats

- **Identity is the IP** (`CF-Connecting-IP`), by design. Visitors behind the same NAT/CGNAT
  (an office, a campus) **share one session** — fine for a public demo, but worth knowing.
- **Interstitial:** the first hit returns a lightweight "spinning up…" page that kicks off
  session creation and reloads when ready, so the first paint isn't a ~10-30s hang.
- **One key** is shared across runners (`DEMOHOST_KEY`). Per-runner keys would mean a small
  config change (a `{url,key}` shape in `RUNNERS`).
- The Worker holds no long-lived per-session state beyond the KV mapping; the runner's reaper
  remains the single owner of session lifetime.
