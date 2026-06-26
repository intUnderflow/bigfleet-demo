/**
 * BigFleet demo coordinator — the Cloudflare Worker at bigfleet-demo.lucy.sh.
 *
 * Public front door + the only thing that creates sessions. Two ways in, both gated:
 *
 *   - Homepage "Demo" button (casual intent): arrives as /?rc=<v3-token> minted on
 *     bigfleet.lucy.sh. We score it with invisible reCAPTCHA v3; a good score + free
 *     capacity earns a live session, anything less falls through to the /demo tour.
 *
 *   - Direct hit to bigfleet-demo.lucy.sh (strong intent — typed/bookmarked the demo):
 *     we serve a small gate page that runs v3 invisibly, and ONLY if the score is too
 *     low shows a reCAPTCHA v2 checkbox. Passing either earns a live session; and if the
 *     fleet is FULL, a passed visitor joins a FIFO queue (with a live position and a tour
 *     link) instead of being turned away — they're dropped straight in when a slot frees,
 *     and lose their place if they stop polling for ~3 minutes.
 *
 * Once assigned, the visitor (keyed by IP) is reverse-proxied to their session until it
 * ends; ending sends them back to the front door rather than silently minting another slot.
 *
 * Config:
 *   RUNNERS             (var)    JSON array of runner base URLs.
 *   RECAPTCHA_MIN_SCORE (var)    minimum reCAPTCHA v3 score (0..1) to earn a slot; default 0.5.
 *   DEMOHOST_KEY        (secret) coordinator key -> X-Demo-Key to each runner's /v1/*.
 *   RECAPTCHA_SECRET    (secret) reCAPTCHA v3 secret -> Google siteverify.
 *   RECAPTCHA_V2_SECRET (secret) reCAPTCHA v2 (checkbox) secret -> Google siteverify.
 *   SESSIONS            (KV)     ip:<ip> -> session assignment; q:<ip> -> queue ticket.
 */

interface Env {
  RUNNERS: string;
  RECAPTCHA_MIN_SCORE?: string;
  DEMOHOST_KEY: string;
  RECAPTCHA_SECRET: string;
  RECAPTCHA_V2_SECRET: string;
  SESSIONS: KVNamespace;
  STATS: AnalyticsEngineDataset; // /stats time-series (writes via binding; reads via SQL API)
  CF_ACCOUNT_ID: string;
  CF_API_TOKEN: string; // Account Analytics:Read — only used by /stats to query Analytics Engine
}

interface Assignment {
  runner: string;
  id: string;
  expiresAt: string;
}

const KV_PREFIX = "ip:";
const QUEUE_PREFIX = "q:"; // q:<ip> -> {joinedAt}; metadata.joinedAt orders the FIFO line
const QUEUE_TTL = 180; // seconds: a queued visitor who stops polling for ~3min loses their place (>3x the 45s poll, so a missed beat never drops them)
const HOME_URL = "https://bigfleet.lucy.sh/"; // front door — re-enter the demo through the gate
const TOUR_URL = "https://bigfleet.lucy.sh/demo"; // the static, always-available terminal tour
const DEMO_URL = "https://bigfleet-demo.lucy.sh/"; // clean demo URL (no one-time token)

// reCAPTCHA site keys are PUBLIC by design (they ship in client HTML). Secrets live in env.
const V3_SITE_KEY = "6Lfu5zAtAAAAAO0Q7uXkJq7PlSHBOheVQnifOmzA";
const V2_SITE_KEY = "6Lcd6TAtAAAAALGFd4ZKVX_WpFwUQoS6u7UtKDKc";

// The BigFleet favicon (same six-rect fleet mark as bigfleet.lucy.sh). Served by the worker so
// the front-door pages (gate/queue/stats) carry it without a session; the per-session UI ships
// its own copy at ui/favicon.svg for direct runner access.
const FAVICON_SVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="32" height="32" aria-label="BigFleet">
  <rect x="6" y="20" width="14" height="14" rx="2" fill="#2563eb"/>
  <rect x="25" y="14" width="14" height="14" rx="2" fill="#3b82f6"/>
  <rect x="44" y="20" width="14" height="14" rx="2" fill="#60a5fa"/>
  <rect x="6" y="40" width="14" height="14" rx="2" fill="#1d4ed8"/>
  <rect x="25" y="34" width="14" height="14" rx="2" fill="#2563eb"/>
  <rect x="44" y="40" width="14" height="14" rx="2" fill="#3b82f6"/>
</svg>`;

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/_coordinator/health") {
      return new Response("ok\n", { headers: { "content-type": "text/plain" } });
    }
    if (url.pathname === "/stats") {
      return statsPage(env);
    }
    if (url.pathname === "/favicon.svg" || url.pathname === "/favicon.ico") {
      return new Response(FAVICON_SVG, {
        headers: {
          "content-type": "image/svg+xml",
          "cache-control": "public, max-age=86400",
        },
      });
    }

    const ip = visitorIP(request);
    const kvKey = KV_PREFIX + ip;

    // The direct-hit gate page calls these: /_gate with a v3 (then, if needed, v2) token,
    // and /_queue to hold/advance a place in line while the fleet is full.
    if (url.pathname === "/_gate" && request.method === "POST") {
      return handleGate(request, env, ip, kvKey);
    }
    if (url.pathname === "/_queue" && request.method === "POST") {
      return handleQueue(env, ip, kvKey);
    }
    if (url.pathname === "/_event" && request.method === "POST") {
      return handleEvent(request, env, kvKey);
    }

    // Homepage "Demo" button (casual intent) arrives with a one-time ?rc=<v3-token>. Resolve
    // it FIRST, and always end in a clean redirect, so the token never reaches the session or
    // lingers in the address bar — whether we assign now or the visitor is already in. (Were
    // this below the proxy branch, a click while already in a session would forward ?rc=...
    // straight through to the demo.)
    if (url.searchParams.has("rc")) {
      if (await readAssignment(env, kvKey)) return Response.redirect(DEMO_URL, 302);
      const score = await scoreV3(env, url.searchParams.get("rc") || "", ip);
      if (!(score >= minScore(env))) {
        recordOutcome(env, "tour", "home", "lowscore");
        return Response.redirect(TOUR_URL, 302);
      }
      const fresh = await assignNew(env);
      if (!fresh) {
        recordOutcome(env, "tour", "home", "full");
        return Response.redirect(TOUR_URL, 302);
      }
      await writeAssignment(env, kvKey, fresh);
      await queueLeave(env, ip); // clear any stale q: ticket (mirrors handleGate) so a direct-nav
      // visitor who clicked the homepage Demo button while queued leaves no phantom queue entry
      recordOutcome(env, "demo", "home", "");
      return Response.redirect(DEMO_URL, 302);
    }

    // Already in a session -> proxy. When the session has ended the runner serves 410 (or
    // it's unreachable, 502); we drop the assignment and send the visitor back to the front
    // door. We deliberately do NOT silently mint another session — re-entry is rescored and
    // rechecked through the gate, so one IP can't camp a scarce slot indefinitely.
    const assignment = await readAssignment(env, kvKey);
    if (assignment) {
      const resp = await proxyToSession(request, url, assignment);
      if (resp.status === 410 || resp.status === 502) {
        await env.SESSIONS.delete(kvKey);
        return Response.redirect(HOME_URL, 302);
      }
      return resp;
    }

    // Direct hit, no session: serve the v3-then-v2 gate page (strong intent gets a real
    // chance to prove human, not an immediate bounce to the tour).
    return html(gatePage());
  },

  // Cron (every 15 min): snapshot capacity + queue depth, and scrape each runner's finished
  // sessions, into the Analytics Engine time-series the /stats page reads. The interval is a
  // direct KV cost — each run spends a queueDepth LIST + per-runner statscursor PUT/GET against
  // the 1,000/day free-tier list+write budgets — so it's kept coarse and the LIST/PUT are
  // skipped when there's nothing to record (see sampleStats).
  async scheduled(_event: ScheduledEvent, env: Env, _ctx: ExecutionContext): Promise<void> {
    await sampleStats(env);
  },
};

/**
 * handleGate verifies a token from the direct-hit gate page and assigns a session. v3 below
 * the bar asks the page to fall back to the v2 checkbox; a passed v2 (or a good v3) earns a
 * slot if any runner has room, and otherwise joins the FIFO queue. Idempotent: if this IP
 * already holds a session it just says "go", so a retry/reload never double-assigns.
 */
async function handleGate(
  request: Request,
  env: Env,
  ip: string,
  kvKey: string,
): Promise<Response> {
  if (await readAssignment(env, kvKey)) return json({ ok: true });

  let mode = "";
  let token = "";
  try {
    const b = (await request.json()) as { mode?: string; token?: string };
    mode = b.mode || "";
    token = b.token || "";
  } catch {
    return json({ fail: true });
  }

  if (mode === "v3") {
    const score = await scoreV3(env, token, ip);
    if (!(score >= minScore(env))) return json({ needV2: true });
  } else if (mode === "v2") {
    if (!(await passV2(env, token, ip))) return json({ fail: true });
  } else {
    return json({ fail: true });
  }

  const fresh = await assignNew(env);
  if (fresh) {
    await writeAssignment(env, kvKey, fresh);
    await queueLeave(env, ip);
    recordOutcome(env, "demo", "direct", "");
    return json({ ok: true });
  }
  // Passed reCAPTCHA but the fleet is full -> join the FIFO queue (direct-nav visitors only;
  // the homepage ?rc path instead falls through to the tour).
  const joinedAt = await queueTouch(env, ip);
  recordOutcome(env, "queued", "direct", "full");
  return json({ queued: true, position: await queuePosition(env, joinedAt) });
}

/**
 * handleQueue is polled by a waiting gate page (~every 45s). It refreshes the visitor's
 * ticket TTL (so stopping for ~3min drops them), reports their live FIFO position, and — when
 * they reach the front and a slot frees — assigns it and tells the page to go.
 */
async function handleQueue(env: Env, ip: string, kvKey: string): Promise<Response> {
  if (await readAssignment(env, kvKey)) {
    await queueLeave(env, ip);
    return json({ ready: true });
  }
  const joinedAt = await queueTouch(env, ip);
  const position = await queuePosition(env, joinedAt);
  if (position === 1) {
    const fresh = await assignNew(env);
    if (fresh) {
      await writeAssignment(env, kvKey, fresh);
      await queueLeave(env, ip);
      recordOutcome(env, "promoted", "queue", "");
      return json({ ready: true });
    }
  }
  return json({ queued: true, position });
}

/**
 * handleEvent records an in-demo funnel step (e.g. "landed", "section:6", "act:dashboard") for
 * the /stats funnel. Gated behind holding a session, so only real visitors can log — no public
 * spam of the dataset. The UI de-dupes per page load; counts are step-reaches, not unique users.
 */
async function handleEvent(request: Request, env: Env, kvKey: string): Promise<Response> {
  if (!(await readAssignment(env, kvKey))) return new Response(null, { status: 204 });
  let step = "";
  try {
    const b = (await request.json()) as { step?: string };
    step = (b.step || "").slice(0, 40);
  } catch {}
  if (step) {
    try {
      env.STATS?.writeDataPoint({ indexes: ["funnel"], blobs: [step], doubles: [1] });
    } catch {}
  }
  return new Response(null, { status: 204 });
}

/** Upsert my queue ticket, preserving my original joinedAt so polling never resets my place. */
async function queueTouch(env: Env, ip: string): Promise<number> {
  const k = QUEUE_PREFIX + ip;
  const existing = (await env.SESSIONS.get(k, { type: "json" })) as { joinedAt: number } | null;
  const joinedAt = existing && typeof existing.joinedAt === "number" ? existing.joinedAt : Date.now();
  await env.SESSIONS.put(k, JSON.stringify({ joinedAt }), {
    expirationTtl: QUEUE_TTL,
    metadata: { joinedAt },
  });
  return joinedAt;
}

/** 1-based FIFO position: (non-expired tickets that joined before me) + 1. */
async function queuePosition(env: Env, joinedAt: number): Promise<number> {
  let ahead = 0;
  let cursor: string | undefined;
  do {
    const res: any = await env.SESSIONS.list({ prefix: QUEUE_PREFIX, cursor });
    for (const k of res.keys) {
      const ja = k.metadata && (k.metadata as { joinedAt?: number }).joinedAt;
      if (typeof ja === "number" && ja < joinedAt) ahead++;
    }
    cursor = res.list_complete ? undefined : res.cursor;
  } while (cursor);
  return ahead + 1;
}

async function queueLeave(env: Env, ip: string): Promise<void> {
  await env.SESSIONS.delete(QUEUE_PREFIX + ip);
}

function minScore(env: Env): number {
  return parseFloat(env.RECAPTCHA_MIN_SCORE || "") || 0.5;
}

function visitorIP(request: Request): string {
  return (
    request.headers.get("CF-Connecting-IP") ||
    request.headers.get("X-Forwarded-For")?.split(",")[0].trim() ||
    "anon"
  );
}

/** Low-level Google siteverify; returns the parsed response (or {success:false} on error). */
async function siteverify(secret: string, token: string, ip: string): Promise<any> {
  if (!secret || !token) return { success: false };
  try {
    const body = new URLSearchParams({ secret, response: token });
    if (ip && ip !== "anon") body.set("remoteip", ip);
    const r = await fetch("https://www.google.com/recaptcha/api/siteverify", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body,
    });
    if (!r.ok) return { success: false };
    return await r.json();
  } catch {
    return { success: false };
  }
}

/** reCAPTCHA v3 -> 0..1 score, or -1 if missing/invalid/forged or minted for another action. */
async function scoreV3(env: Env, token: string, ip: string): Promise<number> {
  const v = await siteverify(env.RECAPTCHA_SECRET, token, ip);
  if (!v.success) return -1;
  if (v.action && v.action !== "demo") return -1;
  return typeof v.score === "number" ? v.score : -1;
}

/** reCAPTCHA v2 checkbox -> did the visitor solve the challenge. */
async function passV2(env: Env, token: string, ip: string): Promise<boolean> {
  const v = await siteverify(env.RECAPTCHA_V2_SECRET, token, ip);
  return !!v.success;
}

function parseRunners(env: Env): string[] {
  let raw: unknown;
  try {
    raw = JSON.parse(env.RUNNERS || "[]");
  } catch {
    return [];
  }
  if (!Array.isArray(raw)) return [];
  return raw
    .map((e) => (typeof e === "string" ? e : e && (e as any).url))
    .filter((u): u is string => typeof u === "string" && u.length > 0)
    .map((u) => u.replace(/\/+$/, ""));
}

async function readAssignment(env: Env, key: string): Promise<Assignment | null> {
  const v = await env.SESSIONS.get(key);
  if (!v) return null;
  try {
    return JSON.parse(v) as Assignment;
  } catch {
    return null;
  }
}

async function writeAssignment(env: Env, key: string, a: Assignment): Promise<void> {
  const secs = Math.max(60, Math.floor((Date.parse(a.expiresAt) - Date.now()) / 1000));
  await env.SESSIONS.put(key, JSON.stringify(a), { expirationTtl: secs });
}

/**
 * assignNew picks a runner with the most headroom and asks it for a session. On a 503
 * (the runner filled between the capacity check and the create) it falls through to the
 * next candidate. Returns null when every runner is full.
 */
async function assignNew(env: Env): Promise<Assignment | null> {
  const runners = parseRunners(env);
  if (runners.length === 0) return null;

  const caps = await Promise.all(runners.map((r) => capacityOf(env, r)));
  const ranked = runners
    .map((runner, i) => ({ runner, headroom: caps[i] }))
    .filter((c) => c.headroom > 0)
    .sort((a, b) => b.headroom - a.headroom);

  for (const c of ranked) {
    const created = await createSession(env, c.runner);
    if (created) return { runner: c.runner, id: created.id, expiresAt: created.expiresAt };
  }
  return null;
}

async function capacityOf(env: Env, runner: string): Promise<number> {
  try {
    const r = await fetch(runner + "/v1/capacity", {
      headers: { "X-Demo-Key": env.DEMOHOST_KEY },
      cf: { cacheTtl: 0 },
    } as RequestInit);
    if (!r.ok) return 0;
    const v = (await r.json()) as { headroomSessions?: number };
    return Math.max(0, v.headroomSessions || 0);
  } catch {
    return 0;
  }
}

async function createSession(
  env: Env,
  runner: string,
): Promise<{ id: string; expiresAt: string } | null> {
  try {
    const r = await fetch(runner + "/v1/sessions", {
      method: "POST",
      headers: { "X-Demo-Key": env.DEMOHOST_KEY },
    });
    if (!r.ok) return null; // 503 (full) or error -> caller tries the next runner
    const v = (await r.json()) as { id: string; expiresAt: string };
    return v && v.id ? { id: v.id, expiresAt: v.expiresAt } : null;
  } catch {
    return null;
  }
}

async function proxyToSession(request: Request, url: URL, a: Assignment): Promise<Response> {
  const target = `${a.runner}/s/${a.id}${url.pathname}${url.search}`;
  try {
    return await fetch(new Request(target, request));
  } catch {
    return new Response("session backend unreachable", { status: 502 });
  }
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function html(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "content-type": "text/html; charset=utf-8" } });
}

/**
 * gatePage — served on a direct hit to bigfleet-demo.lucy.sh. Runs reCAPTCHA v3 invisibly;
 * a good score POSTs straight through /_gate into a session, a low score reveals the v2
 * checkbox. If the fleet is full it shows a live FIFO queue position (polling /_queue) plus a
 * tour link. The inline script avoids template literals so it nests inside this one.
 */
function gatePage(): string {
  return `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<title>BigFleet — live demo</title>
<script src="https://www.google.com/recaptcha/api.js?render=${V3_SITE_KEY}" async defer></script>
<style>
:root{color-scheme:dark light}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#17181c;color:#edeef3;font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
@media(prefers-color-scheme:light){body{background:#fff;color:#17181c}}
.card{max-width:460px;text-align:center;padding:32px}
.logo{display:inline-flex;gap:9px;align-items:center;font-weight:600;font-size:18px;color:#2563eb;margin-bottom:18px}
h1{font-size:19px;margin:0 0 8px}
p{color:#888c96;margin:0 0 16px}
a{color:#2563eb}
.small{font-size:13px}
.pos{font-size:40px;font-weight:700;color:#edeef3;margin:6px 0 2px;font-variant-numeric:tabular-nums}
@media(prefers-color-scheme:light){.pos{color:#17181c}}
.spin{width:30px;height:30px;border:3px solid #353841;border-top-color:#2563eb;border-radius:50%;
  margin:6px auto 18px;animation:r 1s linear infinite}@keyframes r{to{transform:rotate(360deg)}}
#v2box{display:inline-block;margin:6px 0 4px}
.err{color:#f87171}
.hide{display:none}
</style></head><body><div class="card">
<div class="logo"><svg viewBox="0 0 64 64" width="22" height="22"><rect x="6" y="20" width="14" height="14" rx="2" fill="#2563eb"/><rect x="25" y="14" width="14" height="14" rx="2" fill="#3b82f6"/><rect x="44" y="20" width="14" height="14" rx="2" fill="#60a5fa"/><rect x="6" y="40" width="14" height="14" rx="2" fill="#1d4ed8"/><rect x="25" y="34" width="14" height="14" rx="2" fill="#2563eb"/><rect x="44" y="40" width="14" height="14" rx="2" fill="#3b82f6"/></svg><span>BigFleet</span></div>
<div id="checking"><div class="spin"></div><h1>Getting your live demo ready…</h1><p>One quick automated check.</p></div>
<div id="challenge" class="hide"><h1>Quick check</h1><p>Confirm you're human to get your own live session.</p><div id="v2box"></div><p id="cherr" class="err hide">That didn't go through — try again.</p></div>
<div id="starting" class="hide"><div class="spin"></div><h1>Starting your live demo…</h1><p>Standing up real Kubernetes clusters and a BigFleet shard just for you.</p></div>
<div id="queued" class="hide"><h1>You're in line</h1><p>The live demo is at capacity right now. Keep this tab open — you'll be dropped straight in when a spot frees.</p><div class="pos">#<span id="qpos">—</span></div><p class="small">your place in line</p><p class="small">Don't want to wait? <a href="${TOUR_URL}">Take the guided terminal tour →</a></p></div>
</div>
<script>
(function(){
  var V3=${JSON.stringify(V3_SITE_KEY)}, V2=${JSON.stringify(V2_SITE_KEY)};
  var v2id=null, tries=0, qpoll=null;
  function show(id){ ["checking","challenge","starting","queued"].forEach(function(x){
    document.getElementById(x).classList[x===id?"remove":"add"]("hide"); }); }
  function handle(res){
    if(res&&res.ok){ show("starting"); location.href="/"; return; }
    if(res&&res.queued){ queue(res.position); return; }
    if(res&&res.needV2){ challenge(); return; }
    document.getElementById("cherr").classList.remove("hide");
    if(v2id!==null){ try{ grecaptcha.reset(v2id); }catch(e){} }
  }
  function post(mode,token){
    fetch("/_gate",{method:"POST",headers:{"content-type":"application/json"},
      body:JSON.stringify({mode:mode,token:token})})
      .then(function(r){return r.json();}).then(handle).catch(function(){ queue(null); });
  }
  function queue(pos){
    show("queued");
    if(pos){ document.getElementById("qpos").textContent=pos; }
    if(!qpoll){ qpoll=setInterval(pollQueue,45000); }
  }
  function pollQueue(){
    fetch("/_queue",{method:"POST"}).then(function(r){return r.json();}).then(function(res){
      if(res&&res.ready){ if(qpoll)clearInterval(qpoll); show("starting"); location.href="/"; return; }
      if(res&&res.queued){ document.getElementById("qpos").textContent=res.position; }
    }).catch(function(){});
  }
  function challenge(){
    show("challenge");
    if(v2id===null){
      try{ v2id=grecaptcha.render("v2box",{sitekey:V2,callback:function(t){
        document.getElementById("cherr").classList.add("hide"); post("v2",t); }}); }
      catch(e){ v2id=null; setTimeout(challenge,300); }
    }
  }
  function startV3(){
    grecaptcha.ready(function(){
      grecaptcha.execute(V3,{action:"demo"}).then(function(t){ post("v3",t); }, challenge);
    });
  }
  (function waitReady(){
    if(window.grecaptcha&&grecaptcha.execute){ startV3(); return; }
    if(tries++>50){ challenge(); return; }   // ~10s grace, then fall back to the visible check
    setTimeout(waitReady,200);
  })();
})();
</script>
</body></html>`;
}

// ── /stats: usage analytics ──────────────────────────────────────────────────
// Writes go through the Analytics Engine binding (no token); the page reads back via the SQL
// API (needs CF_API_TOKEN). Schema: index1 = kind ("outcome" | "capacity" | "session").
//   outcome:  blob1=outcome blob2=via blob3=reason   double1=1
//   capacity: double1=free double2=busy double3=queueDepth
//   session:  blob1=reason  double1=durationSec

const STATS_DATASET = "bigfleet_demo_stats";

/** Record a terminal visitor outcome (fire-and-forget) for the /stats time-series. */
function recordOutcome(env: Env, outcome: string, via: string, reason: string): void {
  try {
    env.STATS?.writeDataPoint({ indexes: ["outcome"], blobs: [outcome, via, reason], doubles: [1] });
  } catch {}
}

/** Cron body: snapshot fleet capacity + queue depth, and pull each runner's finished sessions. */
async function sampleStats(env: Env): Promise<void> {
  const runners = parseRunners(env);
  const caps = await Promise.all(runners.map((r) => capacityStatsOf(env, r)));
  const free = caps.reduce((a, c) => a + c.free, 0);
  const busy = caps.reduce((a, c) => a + c.busy, 0);
  const warm = caps.reduce((a, c) => a + c.warm, 0);
  // A queue only forms when every runner is full (assignNew returned null), so when there's
  // free capacity skip the queueDepth KV LIST entirely — it's the cron's single biggest list
  // cost and would otherwise run unconditionally against the 1,000/day free-tier list budget.
  const qd = free > 0 ? 0 : await queueDepth(env);
  try {
    env.STATS?.writeDataPoint({ indexes: ["capacity"], blobs: [], doubles: [free, busy, qd, warm] });
  } catch {}

  for (const r of runners) {
    const ck = "statscursor:" + r;
    const since = parseInt((await env.SESSIONS.get(ck)) || "0", 10) || 0;
    const s = await fetchRunnerStats(env, r, since);
    if (!s) continue;
    for (const c of s.completions || []) {
      try {
        env.STATS?.writeDataPoint({ indexes: ["session"], blobs: [c.reason || ""], doubles: [c.durationSec || 0] });
      } catch {}
    }
    // Advance the cursor (a KV PUT) only when we actually recorded completions — idle cron
    // runs would otherwise burn one write per runner per interval against the 1,000/day budget.
    // Not advancing on an idle run is safe: next run re-queries from the same `since` and finds
    // nothing new, so no completion is ever double-counted.
    if (typeof s.now === "number" && (s.completions || []).length > 0)
      await env.SESSIONS.put(ck, String(s.now));
  }
}

// busy = sessions actually serving a visitor; free = slots not serving a visitor (some of which
// are the warm pool, surfaced separately). NOT headroom/runningSessions, which count the pool.
async function capacityStatsOf(
  env: Env,
  runner: string,
): Promise<{ busy: number; free: number; warm: number; max: number; ok: boolean }> {
  try {
    const r = await fetch(runner + "/v1/capacity", {
      headers: { "X-Demo-Key": env.DEMOHOST_KEY },
      cf: { cacheTtl: 0 },
    } as RequestInit);
    if (!r.ok) return { busy: 0, free: 0, warm: 0, max: 0, ok: false };
    const v = (await r.json()) as {
      maxSessions?: number;
      visitorSessions?: number;
      warmReady?: number;
    };
    const max = Math.max(0, v.maxSessions || 0);
    const busy = Math.max(0, v.visitorSessions || 0);
    return { busy, free: Math.max(0, max - busy), warm: Math.max(0, v.warmReady || 0), max, ok: true };
  } catch {
    return { busy: 0, free: 0, warm: 0, max: 0, ok: false };
  }
}

// Stable public label for a runner — "R1"/"R2" from the rN-… hostname (falls back to position).
// Deliberately hides the underlying hardware; the /stats page only ever shows R1/R2.
function runnerLabel(url: string, i: number): string {
  try {
    const m = new URL(url).hostname.match(/^r(\d+)\b/i);
    if (m) return "R" + m[1];
  } catch {}
  return "R" + (i + 1);
}

async function queueDepth(env: Env): Promise<number> {
  let n = 0;
  let cursor: string | undefined;
  do {
    const res: any = await env.SESSIONS.list({ prefix: QUEUE_PREFIX, cursor });
    n += res.keys.length;
    cursor = res.list_complete ? undefined : res.cursor;
  } while (cursor);
  return n;
}

async function fetchRunnerStats(
  env: Env,
  runner: string,
  since: number,
): Promise<{ now: number; completions: { durationSec: number; reason: string }[] } | null> {
  try {
    const r = await fetch(runner + "/v1/stats?since=" + since, {
      headers: { "X-Demo-Key": env.DEMOHOST_KEY },
    });
    if (!r.ok) return null;
    return (await r.json()) as any;
  } catch {
    return null;
  }
}

/** Run an Analytics Engine SQL query (returns the data rows, or [] on any failure). */
async function aeQuery(env: Env, sql: string): Promise<any[]> {
  if (!env.CF_API_TOKEN || !env.CF_ACCOUNT_ID) return [];
  try {
    const r = await fetch(
      `https://api.cloudflare.com/client/v4/accounts/${env.CF_ACCOUNT_ID}/analytics_engine/sql`,
      { method: "POST", headers: { Authorization: "Bearer " + env.CF_API_TOKEN }, body: sql },
    );
    if (!r.ok) return [];
    const j = (await r.json()) as { data?: any[] };
    return j.data || [];
  } catch {
    return [];
  }
}

async function statsPage(env: Env): Promise<Response> {
  const D = STATS_DATASET;
  const W = "INTERVAL '7' DAY";
  const [outcomes, sess, capTs, sessTs, funnel] = await Promise.all([
    aeQuery(env, `SELECT blob1 AS outcome, blob2 AS via, sum(_sample_interval) AS n FROM ${D} WHERE index1='outcome' AND timestamp > NOW() - ${W} GROUP BY outcome, via`),
    aeQuery(env, `SELECT sum(double1*_sample_interval)/sum(_sample_interval) AS avg_dur, sum(_sample_interval) AS n FROM ${D} WHERE index1='session' AND timestamp > NOW() - ${W}`),
    aeQuery(env, `SELECT intDiv(toUInt32(timestamp),3600)*3600 AS t, sum(double1*_sample_interval)/sum(_sample_interval) AS free, sum(double2*_sample_interval)/sum(_sample_interval) AS busy, sum(double3*_sample_interval)/sum(_sample_interval) AS qd FROM ${D} WHERE index1='capacity' AND timestamp > NOW() - ${W} GROUP BY t ORDER BY t`),
    aeQuery(env, `SELECT intDiv(toUInt32(timestamp),3600)*3600 AS t, sum(_sample_interval) AS n FROM ${D} WHERE index1='session' AND timestamp > NOW() - ${W} GROUP BY t ORDER BY t`),
    aeQuery(env, `SELECT blob1 AS step, sum(_sample_interval) AS n FROM ${D} WHERE index1='funnel' AND timestamp > NOW() - ${W} GROUP BY step`),
  ]);
  const runners = parseRunners(env);
  const caps = await Promise.all(runners.map((r) => capacityStatsOf(env, r)));
  const freeNow = caps.reduce((a, c) => a + c.free, 0);
  const busyNow = caps.reduce((a, c) => a + c.busy, 0);
  const warmNow = caps.reduce((a, c) => a + c.warm, 0);
  const perRunner = runners.map((r, i) => ({ label: runnerLabel(r, i), ...caps[i] }));
  const qNow = freeNow > 0 ? 0 : await queueDepth(env); // skip the LIST unless the fleet is full
  return html(renderStats({ outcomes, sess, capTs, sessTs, funnel, freeNow, busyNow, warmNow, qNow, perRunner, configured: !!env.CF_API_TOKEN }));
}

function spark(series: { values: number[]; color: string }[], w = 300, h = 48): string {
  const all = series.flatMap((s) => s.values);
  if (!all.length) return `<svg width="100%" height="${h}" viewBox="0 0 ${w} ${h}"></svg>`;
  const max = Math.max(1, ...all);
  let lines = "";
  for (const s of series) {
    if (!s.values.length) continue;
    const step = s.values.length > 1 ? w / (s.values.length - 1) : w;
    const pts = s.values
      .map((v, i) => `${(i * step).toFixed(1)},${(h - (v / max) * (h - 4) - 2).toFixed(1)}`)
      .join(" ");
    lines += `<polyline fill="none" stroke="${s.color}" stroke-width="2" points="${pts}"/>`;
  }
  return `<svg width="100%" height="${h}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">${lines}</svg>`;
}

function renderStats(d: {
  outcomes: any[];
  sess: any[];
  capTs: any[];
  sessTs: any[];
  funnel: any[];
  freeNow: number;
  busyNow: number;
  warmNow: number;
  qNow: number;
  perRunner: { label: string; busy: number; free: number; warm: number; max: number; ok: boolean }[];
  configured: boolean;
}): string {
  const num = (x: any) => (typeof x === "number" ? x : parseFloat(x) || 0);
  const oc = (outcome: string, via: string) =>
    d.outcomes
      .filter((r) => r.outcome === outcome && r.via === via)
      .reduce((a, r) => a + num(r.n), 0);

  const homeDemo = oc("demo", "home");
  const homeTour = oc("tour", "home");
  const directDemo = oc("demo", "direct");
  const directQueued = oc("queued", "direct");
  const promoted = oc("promoted", "queue");
  const homeTotal = homeDemo + homeTour;
  const liveTotal = homeDemo + directDemo + promoted;
  const avgDur = d.sess[0] ? num(d.sess[0].avg_dur) : 0;
  const sessN = d.sess[0] ? num(d.sess[0].n) : 0;
  const slots = d.freeNow + d.busyNow;

  const pct = (a: number, t: number) => (t > 0 ? Math.round((a / t) * 100) : 0);
  const dur = (s: number) =>
    s >= 60 ? Math.floor(s / 60) + "m " + Math.round(s % 60) + "s" : Math.round(s) + "s";
  const n = (x: number) => Math.round(x).toLocaleString();

  const free = d.capTs.map((r) => num(r.free));
  const busy = d.capTs.map((r) => num(r.busy));
  const qd = d.capTs.map((r) => num(r.qd));
  const sphv = d.sessTs.map((r) => num(r.n));

  const banner = d.configured
    ? ""
    : `<div class="warn">Stats collection is running, but this page can't query yet — set the <code>CF_API_TOKEN</code> Worker secret (a Cloudflare API token with <b>Account Analytics: Read</b>). It fills in once that's set and data accrues.</div>`;

  const card = (label: string, value: string, sub = "") =>
    `<div class="card"><div class="v">${value}</div><div class="l">${label}</div>${sub ? `<div class="s">${sub}</div>` : ""}</div>`;
  const chart = (title: string, legend: string, svg: string) =>
    `<div class="chart"><div class="ct">${title}</div><div class="cl">${legend}</div>${svg}</div>`;

  // funnel: scroll-depth through the 10-section arc + a few engagement actions
  const fmap: Record<string, number> = {};
  d.funnel.forEach((r) => (fmap[String(r.step)] = num(r.n)));
  const SECTIONS: [number, string][] = [
    [1, "Not another cluster autoscaler"],
    [2, "Your fleet"],
    [3, "Capacity moves between clusters"],
    [4, "Preemption across the fleet"],
    [5, "How it chooses (cheapest first)"],
    [6, "Drive it yourself (provision/reclaim)"],
    [7, "The fleet is finite"],
    [8, "Prove the clusters are real"],
    [9, "What's real / simulated"],
  ];
  const landed = fmap["landed"] || fmap["section:1"] || 0;
  const funnelRows = SECTIONS.map(([nn, label]) => {
    const c = fmap["section:" + nn] || 0;
    const w = landed > 0 ? Math.round((c / landed) * 100) : 0;
    return `<div class="frow"><div class="fl">&sect;${nn} ${label}</div><div class="fbar"><i style="width:${w}%"></i></div><div class="fn">${n(c)}${landed > 0 ? " &middot; " + w + "%" : ""}</div></div>`;
  }).join("");
  const engage = [
    ["act:scenario:move", "ran the cross-cluster MOVE (lead payoff)"],
    ["act:scenario:critical", "ran fleet preemption"],
    ["act:scenario:saturate", "saturated the fleet"],
    ["act:demand", "drove demand (§6)"],
    ["act:fleet-dashboard", "opened BigFleet's own dashboard"],
    ["act:dashboard", "opened a cluster's k8s dashboard"],
  ]
    .map(([k, label]) =>
      card(label, n(fmap[k] || 0), landed > 0 ? pct(fmap[k] || 0, landed) + "% of those who landed" : ""),
    )
    .join("");

  // Per-runner live load (R1/R2 — hardware is deliberately not named).
  const runnerCard = (rr: typeof d.perRunner[number]) => {
    if (!rr.ok)
      return `<div class="card"><div class="v" style="color:#888c96">offline</div><div class="l">${rr.label}</div></div>`;
    const load = rr.max > 0 ? pct(rr.busy, rr.max) : 0;
    return `<div class="card"><div class="v">${n(rr.busy)}<span style="color:#6b7280;font-size:16px;font-weight:600"> / ${n(rr.max)}</span></div>` +
      `<div class="l">${rr.label} &mdash; in use</div>` +
      `<div class="s">${n(rr.warm)} warm ready &middot; ${load}% load</div>` +
      `<div class="bar" title="${load}% load"><i style="width:${load}%;background:#2563eb"></i><i style="width:${100 - load}%;background:#3a8a52"></i></div></div>`;
  };
  const runnersBlock = d.perRunner.length
    ? `<h2>By runner</h2><div class="grid">${d.perRunner.map(runnerCard).join("")}</div>`
    : "";

  return `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<title>BigFleet demo — usage stats</title>
<style>
:root{color-scheme:dark light}
body{margin:0;background:#17181c;color:#edeef3;font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
@media(prefers-color-scheme:light){body{background:#f6f7f9;color:#17181c}}
.wrap{max-width:880px;margin:0 auto;padding:40px 22px 64px}
.logo{display:inline-flex;gap:9px;align-items:center;font-weight:600;font-size:18px;color:#2563eb}
h1{font-size:22px;margin:14px 0 2px}
.sub{color:#888c96;margin:0 0 26px}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.06em;color:#888c96;margin:30px 0 12px;font-weight:600}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px}
.card{background:#1f2127;border:1px solid #2c2f37;border-radius:12px;padding:16px}
@media(prefers-color-scheme:light){.card{background:#fff;border-color:#e3e5ea}}
.card .v{font-size:26px;font-weight:700;font-variant-numeric:tabular-nums}
.card .l{color:#888c96;font-size:13px;margin-top:2px}
.card .s{color:#6b7280;font-size:12px;margin-top:6px}
.bar{height:8px;border-radius:5px;background:#2c2f37;overflow:hidden;margin-top:10px;display:flex}
.bar>i{display:block;height:100%}
.chart{background:#1f2127;border:1px solid #2c2f37;border-radius:12px;padding:16px;margin-top:12px}
@media(prefers-color-scheme:light){.chart{background:#fff;border-color:#e3e5ea}}
.chart .ct{font-weight:600;font-size:14px}
.chart .cl{color:#888c96;font-size:12px;margin:1px 0 8px}
.funnel{background:#1f2127;border:1px solid #2c2f37;border-radius:12px;padding:16px}
@media(prefers-color-scheme:light){.funnel{background:#fff;border-color:#e3e5ea}}
.frow{display:flex;align-items:center;gap:10px;margin:5px 0}
.frow .fl{flex:0 0 210px;font-size:13px;color:#c9ccd4}
@media(prefers-color-scheme:light){.frow .fl{color:#3a3f4a}}
.frow .fbar{flex:1;height:18px;background:#2c2f37;border-radius:5px;overflow:hidden}
.frow .fbar>i{display:block;height:100%;background:linear-gradient(90deg,#1d4ed8,#60a5fa);border-radius:5px;min-width:2px}
.frow .fn{flex:0 0 92px;text-align:right;font-size:12.5px;color:#888c96;font-variant-numeric:tabular-nums}
.warn{background:#3b2f12;border:1px solid #6b5418;color:#f2d999;border-radius:10px;padding:12px 14px;margin-bottom:22px;font-size:13.5px}
.dot{display:inline-block;width:9px;height:9px;border-radius:50%;vertical-align:middle;margin-right:4px}
code{background:#2c2f37;padding:1px 5px;border-radius:5px;font-size:12.5px}
a{color:#2563eb}
.note{color:#6b7280;font-size:12.5px;margin-top:30px;border-top:1px solid #2c2f37;padding-top:16px}
</style></head><body><div class="wrap">
<div class="logo"><svg viewBox="0 0 64 64" width="22" height="22"><rect x="6" y="20" width="14" height="14" rx="2" fill="#2563eb"/><rect x="25" y="14" width="14" height="14" rx="2" fill="#3b82f6"/><rect x="44" y="20" width="14" height="14" rx="2" fill="#60a5fa"/><rect x="6" y="40" width="14" height="14" rx="2" fill="#1d4ed8"/><rect x="25" y="34" width="14" height="14" rx="2" fill="#2563eb"/><rect x="44" y="40" width="14" height="14" rx="2" fill="#3b82f6"/></svg><span>BigFleet demo</span></div>
<h1>Usage stats</h1>
<p class="sub">How the live demo is being used. Last 7 days unless noted.</p>
${banner}

<h2>Right now</h2>
<div class="grid">
  ${card("in use by visitors", n(d.busyNow), slots > 0 ? pct(d.busyNow, slots) + "% of " + n(slots) + " slots" : "no runners")}
  ${card("free", n(d.freeNow))}
  ${card("warm pool ready", n(d.warmNow), "pre-warmed, instant dive-in")}
  ${card("waiting in queue", n(d.qNow))}
</div>
<div class="bar" title="in use vs free"><i style="width:${slots > 0 ? pct(d.busyNow, slots) : 0}%;background:#2563eb"></i><i style="width:${slots > 0 ? 100 - pct(d.busyNow, slots) : 100}%;background:#3a8a52"></i></div>

${runnersBlock}

<h2>Homepage &quot;Demo&quot; button</h2>
<div class="grid">
  ${card("&rarr; into the live demo", n(homeDemo), pct(homeDemo, homeTotal) + "% of clicks")}
  ${card("&rarr; sent to the tour", n(homeTour), pct(homeTour, homeTotal) + "% of clicks")}
  ${card("total button clicks", n(homeTotal))}
</div>

<h2>Direct visits to bigfleet-demo.lucy.sh</h2>
<div class="grid">
  ${card("&rarr; straight into the demo", n(directDemo))}
  ${card("&rarr; joined the queue", n(directQueued))}
  ${card("promoted from queue", n(promoted))}
</div>

<h2>Sessions</h2>
<div class="grid">
  ${card("completed sessions", n(sessN))}
  ${card("avg session length", sessN > 0 ? dur(avgDur) : "&mdash;")}
  ${card("served into the demo", n(liveTotal), "button + direct + queue")}
</div>

<h2>How far visitors get</h2>
<div class="funnel">${funnelRows}</div>
<div class="grid" style="margin-top:12px">${engage}</div>

<h2>Over time (hourly)</h2>
${chart("Slots in use vs free", `<span class="dot" style="background:#3a8a52"></span>free &nbsp; <span class="dot" style="background:#2563eb"></span>in use`, spark([{ values: free, color: "#3a8a52" }, { values: busy, color: "#2563eb" }]))}
${chart("Queue depth", `<span class="dot" style="background:#f5a623"></span>visitors waiting`, spark([{ values: qd, color: "#f5a623" }]))}
${chart("Completed sessions per hour", `<span class="dot" style="background:#60a5fa"></span>sessions`, spark([{ values: sphv, color: "#60a5fa" }]))}

<p class="note">Numbers illustrate demo traffic, not a benchmark. Capacity and queue depth are sampled every 15 minutes; session lengths are measured by each runner from hand-out to reap. Stored in Cloudflare Analytics Engine (~90-day retention). Back to <a href="https://bigfleet.lucy.sh/">BigFleet</a>.</p>
</div></body></html>`;
}
