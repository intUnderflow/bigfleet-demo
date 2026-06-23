/**
 * BigFleet demo coordinator — the Cloudflare Worker at bigfleet-demo-api.lucy.sh.
 *
 * It is the public front door and the only thing that creates sessions. It knows the
 * runners (demohost daemons) by their internet addresses and current capacity, assigns
 * each visitor — keyed by IP — to a session on a runner with headroom, and reverse-proxies
 * the visitor's traffic to that session. When a session ends (the 1-hour cap, or ~5 minutes
 * after the tab closes), the runner serves 410 for it and the visitor's next request gets a
 * fresh session on a runner that still has room.
 *
 * Config:
 *   RUNNERS       (var)    JSON array of runner base URLs, e.g.
 *                          ["https://r1.bigfleet-demo.lucy.sh","https://r2.bigfleet-demo.lucy.sh"]
 *                          (entries may also be objects {"url":"...","name":"..."}).
 *   DEMOHOST_KEY  (secret) the coordinator key — sent as X-Demo-Key to each runner's /v1/*.
 *   SESSIONS      (KV)     ip -> {runner,id,expiresAt}; auto-expires at session end.
 */

interface Env {
  RUNNERS: string;
  DEMOHOST_KEY: string;
  SESSIONS: KVNamespace;
}

interface Assignment {
  runner: string;
  id: string;
  expiresAt: string;
}

const KV_PREFIX = "ip:";

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/_coordinator/health") {
      return new Response("ok\n", { headers: { "content-type": "text/plain" } });
    }

    const ip = visitorIP(request);
    const kvKey = KV_PREFIX + ip;

    // The interstitial calls this to (idempotently) get a session for this IP.
    if (url.pathname === "/_coordinator/assign" && request.method === "POST") {
      let a = await readAssignment(env, kvKey);
      if (!a) {
        a = await assignNew(env);
        if (!a) return json({ full: true }, 503);
        await writeAssignment(env, kvKey, a);
      }
      return json({ ready: true });
    }

    const assignment = await readAssignment(env, kvKey);
    if (!assignment) {
      // No session yet for this IP — show the "spinning up" interstitial, which POSTs
      // /_coordinator/assign and reloads once a session exists. (Keeps the first paint
      // instant instead of hanging ~10-30s on session bring-up.)
      return html(interstitialPage());
    }

    // Proxy to the assigned session. Clone first so we can retry on a dead session.
    const retry = request.clone();
    let resp = await proxyToSession(request, url, assignment);
    if (resp.status === 410 || resp.status === 502) {
      await env.SESSIONS.delete(kvKey);
      const fresh = await assignNew(env);
      if (!fresh) return html(busyPage(), 503);
      await writeAssignment(env, kvKey, fresh);
      resp = await proxyToSession(retry, url, fresh);
    }
    return resp;
  },
};

function visitorIP(request: Request): string {
  return (
    request.headers.get("CF-Connecting-IP") ||
    request.headers.get("X-Forwarded-For")?.split(",")[0].trim() ||
    "anon"
  );
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

const SHELL = (title: string, inner: string) => `<!doctype html><html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>${title}</title><style>
:root{color-scheme:dark light}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
  background:#17181c;color:#edeef3;font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
@media(prefers-color-scheme:light){body{background:#fff;color:#17181c}}
.card{max-width:440px;text-align:center;padding:32px}
.logo{display:inline-flex;gap:9px;align-items:center;font-weight:600;font-size:18px;color:#2563eb;margin-bottom:18px}
h1{font-size:19px;margin:0 0 8px}
p{color:#888c96;margin:0 0 18px}
.spin{width:30px;height:30px;border:3px solid #353841;border-top-color:#2563eb;border-radius:50%;
  margin:6px auto 18px;animation:r 1s linear infinite}@keyframes r{to{transform:rotate(360deg)}}
button{background:#2563eb;color:#fff;border:0;border-radius:9px;padding:10px 18px;font-size:14px;font-weight:600;cursor:pointer}
</style></head><body><div class="card">
<div class="logo"><svg viewBox="0 0 64 64" width="22" height="22"><rect x="6" y="20" width="14" height="14" rx="2" fill="#2563eb"/><rect x="25" y="14" width="14" height="14" rx="2" fill="#3b82f6"/><rect x="44" y="20" width="14" height="14" rx="2" fill="#60a5fa"/><rect x="6" y="40" width="14" height="14" rx="2" fill="#1d4ed8"/><rect x="25" y="34" width="14" height="14" rx="2" fill="#2563eb"/><rect x="44" y="40" width="14" height="14" rx="2" fill="#3b82f6"/></svg><span>BigFleet</span></div>
${inner}</div></body></html>`;

function interstitialPage(): string {
  return SHELL(
    "BigFleet — starting your demo",
    `<div class="spin"></div><h1>Spinning up your live demo…</h1>
<p>Standing up real Kubernetes clusters and a BigFleet shard just for you. This takes a few seconds.</p>
<p id="msg" style="display:none"></p>
<script>
(async function(){
  try{
    const r = await fetch('/_coordinator/assign',{method:'POST'});
    if(r.ok){ location.reload(); return; }
  }catch(e){}
  document.querySelector('.spin').style.display='none';
  var m=document.getElementById('msg'); m.style.display='block';
  m.innerHTML='All demo sessions are busy right now. <br><button onclick="location.reload()">Try again</button>';
})();
</script>`,
  );
}

function busyPage(): string {
  return SHELL(
    "BigFleet — all sessions busy",
    `<h1>All demo sessions are busy</h1>
<p>Every live session is in use right now. Each one frees up within the hour — please try again shortly.</p>
<button onclick="location.reload()">Try again</button>`,
  );
}
