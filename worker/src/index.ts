/**
 * BigFleet demo coordinator — the Cloudflare Worker at bigfleet-demo.lucy.sh.
 *
 * Public front door + the only thing that creates sessions. A visitor arrives from the
 * BigFleet homepage's "Demo" button carrying an invisible reCAPTCHA v3 token. The Worker
 * scores the token and checks fleet capacity, and only then spends a live session on them.
 * A low score, a missing token, or no free capacity all fall through to the static
 * /demo tour instead of a dead end. Once assigned, the visitor (keyed by IP) is
 * reverse-proxied to their session until it ends; ending sends them back to the front
 * door to re-enter through the gate rather than silently handing out another slot.
 *
 * Config:
 *   RUNNERS             (var)    JSON array of runner base URLs, e.g.
 *                                ["https://r1-bigfleet-demo.lucy.sh","https://r2-bigfleet-demo.lucy.sh"]
 *                                (entries may also be objects {"url":"...","name":"..."}).
 *   RECAPTCHA_MIN_SCORE (var)    minimum reCAPTCHA v3 score (0..1) to earn a slot; default 0.5.
 *   DEMOHOST_KEY        (secret) the coordinator key — sent as X-Demo-Key to each runner's /v1/*.
 *   RECAPTCHA_SECRET    (secret) the reCAPTCHA v3 secret — sent to Google's siteverify.
 *   SESSIONS            (KV)     ip -> {runner,id,expiresAt}; auto-expires at session end.
 */

interface Env {
  RUNNERS: string;
  RECAPTCHA_MIN_SCORE?: string;
  DEMOHOST_KEY: string;
  RECAPTCHA_SECRET: string;
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

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/_coordinator/health") {
      return new Response("ok\n", { headers: { "content-type": "text/plain" } });
    }

    const ip = visitorIP(request);
    const kvKey = KV_PREFIX + ip;

    // Already in a session -> proxy to it. When the session has ended the runner serves 410
    // (or it's unreachable, 502); we drop the assignment and send the visitor back to the
    // front door. We deliberately do NOT silently mint another session — re-entry is rescored
    // and rechecked for capacity through the gate like any new visit, so one IP can't camp a
    // scarce slot indefinitely.
    const assignment = await readAssignment(env, kvKey);
    if (assignment) {
      const resp = await proxyToSession(request, url, assignment);
      if (resp.status === 410 || resp.status === 502) {
        await env.SESSIONS.delete(kvKey);
        return Response.redirect(HOME_URL, 302);
      }
      return resp;
    }

    // New visitor: the gate. Earn a live slot with a good reCAPTCHA score AND free capacity;
    // anything short of that falls through to the static tour (never a dead end).
    const token = url.searchParams.get("rc") || "";
    const score = token ? await verifyRecaptcha(env, token, ip) : -1;
    const minScore = parseFloat(env.RECAPTCHA_MIN_SCORE || "") || 0.5;
    if (!(score >= minScore)) {
      // missing / invalid / forged token, or low reputation (likely a bot) -> tour
      return Response.redirect(TOUR_URL, 302);
    }
    const fresh = await assignNew(env);
    if (!fresh) {
      // good human, but every runner is full right now -> tour
      return Response.redirect(TOUR_URL, 302);
    }
    await writeAssignment(env, kvKey, fresh);
    // Assigned — bounce to the clean URL so the one-time token leaves the address bar; the
    // next request finds the session in KV and proxies straight into the live demo.
    return Response.redirect(DEMO_URL, 302);
  },
};

function visitorIP(request: Request): string {
  return (
    request.headers.get("CF-Connecting-IP") ||
    request.headers.get("X-Forwarded-For")?.split(",")[0].trim() ||
    "anon"
  );
}

/**
 * verifyRecaptcha scores an invisible reCAPTCHA v3 token via Google's siteverify. Returns the
 * 0..1 reputation score, or -1 if the token is missing/invalid/forged, the secret is unset, or
 * the token was minted for a different action. The caller compares it to RECAPTCHA_MIN_SCORE.
 */
async function verifyRecaptcha(env: Env, token: string, ip: string): Promise<number> {
  if (!env.RECAPTCHA_SECRET) return -1;
  try {
    const body = new URLSearchParams({ secret: env.RECAPTCHA_SECRET, response: token });
    if (ip && ip !== "anon") body.set("remoteip", ip);
    const r = await fetch("https://www.google.com/recaptcha/api/siteverify", {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body,
    });
    if (!r.ok) return -1;
    const v = (await r.json()) as {
      success?: boolean;
      score?: number;
      action?: string;
    };
    if (!v.success) return -1;
    if (v.action && v.action !== "demo") return -1; // token minted for a different action
    return typeof v.score === "number" ? v.score : -1;
  } catch {
    return -1;
  }
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
