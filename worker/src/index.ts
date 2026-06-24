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
 *     low shows a reCAPTCHA v2 "I'm not a robot" checkbox. Passing either (with capacity)
 *     earns a live session — so a real human on a VPN/privacy browser with a poor v3
 *     score can still prove themselves, while a bot hitting the URL meets a real challenge.
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
 *   SESSIONS            (KV)     ip -> {runner,id,expiresAt}; auto-expires at session end.
 */

interface Env {
  RUNNERS: string;
  RECAPTCHA_MIN_SCORE?: string;
  DEMOHOST_KEY: string;
  RECAPTCHA_SECRET: string;
  RECAPTCHA_V2_SECRET: string;
  SESSIONS: KVNamespace;
}

interface Assignment {
  runner: string;
  id: string;
  expiresAt: string;
}

const KV_PREFIX = "ip:";
const HOME_URL = "https://bigfleet.lucy.sh/"; // front door — re-enter the demo through the gate
const TOUR_URL = "https://bigfleet.lucy.sh/demo"; // the static, always-available terminal tour
const DEMO_URL = "https://bigfleet-demo.lucy.sh/"; // clean demo URL (no one-time token)

// reCAPTCHA site keys are PUBLIC by design (they ship in client HTML). Secrets live in env.
const V3_SITE_KEY = "6Lfu5zAtAAAAAO0Q7uXkJq7PlSHBOheVQnifOmzA";
const V2_SITE_KEY = "6Lcd6TAtAAAAALGFd4ZKVX_WpFwUQoS6u7UtKDKc";

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/_coordinator/health") {
      return new Response("ok\n", { headers: { "content-type": "text/plain" } });
    }

    const ip = visitorIP(request);
    const kvKey = KV_PREFIX + ip;

    // The direct-hit gate page calls this with a v3 (then, if needed, v2) token.
    if (url.pathname === "/_gate" && request.method === "POST") {
      return handleGate(request, env, ip, kvKey);
    }

    // Homepage "Demo" button (casual intent) arrives with a one-time ?rc=<v3-token>. Resolve
    // it FIRST, and always end in a clean redirect, so the token never reaches the session or
    // lingers in the address bar — whether we assign now or the visitor is already in. (Were
    // this below the proxy branch, a click while already in a session would forward ?rc=...
    // straight through to the demo.)
    if (url.searchParams.has("rc")) {
      if (await readAssignment(env, kvKey)) return Response.redirect(DEMO_URL, 302);
      const score = await scoreV3(env, url.searchParams.get("rc") || "", ip);
      if (!(score >= minScore(env))) return Response.redirect(TOUR_URL, 302);
      const fresh = await assignNew(env);
      if (!fresh) return Response.redirect(TOUR_URL, 302);
      await writeAssignment(env, kvKey, fresh);
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
};

/**
 * handleGate verifies a token from the direct-hit gate page and, if it clears, assigns a
 * session. v3 below the bar asks the page to fall back to the v2 checkbox; a passed v2 (or a
 * good v3) earns a slot if any runner has room. Idempotent: if this IP already holds a
 * session it just says "go", so a retry/reload never double-assigns.
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
  if (!fresh) return json({ full: true });
  await writeAssignment(env, kvKey, fresh);
  return json({ ok: true });
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
 * checkbox. No capacity -> a friendly pointer to the tour. The inline script avoids template
 * literals so it nests cleanly inside this one.
 */
function gatePage(): string {
  return `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>BigFleet — live demo</title>
<script src="https://www.google.com/recaptcha/api.js?render=${V3_SITE_KEY}" async defer></script>
<style>
:root{color-scheme:dark light}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#17181c;color:#edeef3;font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
@media(prefers-color-scheme:light){body{background:#fff;color:#17181c}}
.card{max-width:440px;text-align:center;padding:32px}
.logo{display:inline-flex;gap:9px;align-items:center;font-weight:600;font-size:18px;color:#2563eb;margin-bottom:18px}
h1{font-size:19px;margin:0 0 8px}
p{color:#888c96;margin:0 0 16px}
a{color:#2563eb}
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
<div id="busy" class="hide"><h1>All live sessions are busy</h1><p>Every session is in use right now — each frees up within the hour. In the meantime, <a href="${TOUR_URL}">take the guided terminal tour</a>.</p></div>
</div>
<script>
(function(){
  var V3=${JSON.stringify(V3_SITE_KEY)}, V2=${JSON.stringify(V2_SITE_KEY)};
  var v2id=null, tries=0;
  function show(id){ ["checking","challenge","starting","busy"].forEach(function(x){
    document.getElementById(x).classList[x===id?"remove":"add"]("hide"); }); }
  function handle(res){
    if(res&&res.ok){ show("starting"); location.href="/"; return; }
    if(res&&res.full){ show("busy"); return; }
    if(res&&res.needV2){ challenge(); return; }
    document.getElementById("cherr").classList.remove("hide");
    if(v2id!==null){ try{ grecaptcha.reset(v2id); }catch(e){} }
  }
  function post(mode,token){
    fetch("/_gate",{method:"POST",headers:{"content-type":"application/json"},
      body:JSON.stringify({mode:mode,token:token})})
      .then(function(r){return r.json();}).then(handle).catch(function(){ show("busy"); });
  }
  function challenge(){
    show("challenge");
    if(v2id===null){
      try{ v2id=grecaptcha.render("v2box",{sitekey:V2,callback:function(t){
        document.getElementById("cherr").classList.add("hide"); post("v2",t); }}); }
      catch(e){ /* api not ready yet — the waitReady loop will retry */ v2id=null; setTimeout(challenge,300); }
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
