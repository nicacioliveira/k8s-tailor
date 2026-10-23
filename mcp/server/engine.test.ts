import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { EngineAdvisorError, EngineAuthError, EngineWriteError, createEngine } from "./engine.ts";

// A fake engine that mirrors internal/server requireWriteAuth: reads open, writes
// need `Authorization: Bearer <token>`.
const TOKEN = "s3cret";
let server: ReturnType<typeof Bun.serve>;
const seen: { method: string; path: string; auth: string | null }[] = [];

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const url = new URL(req.url);
      seen.push({ method: req.method, path: url.pathname, auth: req.headers.get("authorization") });
      if (req.method !== "GET" && url.pathname.startsWith("/api/")) {
        if (req.headers.get("authorization") !== `Bearer ${TOKEN}`) {
          return new Response('{"error":"unauthorized"}', { status: 401 });
        }
      }
      if (url.pathname === "/api/advisor/status")
        return Response.json({ enabled: true, model: "m", breaker_state: "closed", stats: {} });
      if (url.pathname === "/api/advisor/review") {
        return new Response('{"error":"advisor hourly call limit reached","code":"rate_limited"}', {
          status: 429,
          headers: { "Retry-After": "42", "Content-Type": "application/json" },
        });
      }
      if (url.pathname === "/api/advisor/summary") {
        return Response.json({
          considered: 1,
          total: 1,
          summary: { overview: "x" },
          body: await req.json(),
        });
      }
      if (url.pathname === "/api/pause") return Response.json({ paused: true });
      if (url.pathname === "/healthz") return Response.json({ ok: true, vmHealthy: true });
      return Response.json({ path: url.pathname, search: url.search });
    },
  });
});
afterAll(() => server.stop(true));

const base = () => `http://127.0.0.1:${server.port}`;

describe("engine client auth", () => {
  test("sends the bearer token on writes and reads", async () => {
    const e = createEngine({ base: base(), token: TOKEN });
    expect(await e.setPaused(true)).toEqual({ paused: true });
    await e.policies();
    const last = seen.slice(-2);
    expect(last.map((s) => s.auth)).toEqual([`Bearer ${TOKEN}`, `Bearer ${TOKEN}`]);
  });

  test("without a token, reads work and writes fail with a clear auth error", async () => {
    const e = createEngine({ base: base() });
    expect(await e.health()).toEqual({ ok: true, vmHealthy: true });
    const err = (await e.setPaused(true).catch((x: Error) => x)) as Error;
    expect(err).toBeInstanceOf(EngineAuthError);
    expect(String(err.message)).toContain("set TAILOR_API_TOKEN on the MCP server");
  });

  test("a wrong token says it does not match", async () => {
    const e = createEngine({ base: base(), token: "nope" });
    const err = (await e.deletePolicy("x").catch((x: Error) => x)) as Error;
    expect(err).toBeInstanceOf(EngineAuthError);
    expect(String(err.message)).toContain("does not match");
  });

  test("encodes path segments and range query", async () => {
    const e = createEngine({ base: `${base()}/`, token: TOKEN });
    const out = (await e.detail("ns a", "Deployment", "w/1", "c", { window: "1h", step: 30 })) as {
      path: string;
      search: string;
    };
    expect(out.search).toBe("?window=1h&step=30");
    expect(seen.at(-1)?.path).toBe("/api/detail/ns%20a/Deployment/w%2F1/c");
  });
});

describe("advisor tools", () => {
  test("status is a read; summary forwards the body with the bearer", async () => {
    const e = createEngine({ base: base(), token: TOKEN });
    expect(((await e.advisorStatus()) as any).enabled).toBe(true);
    const out: any = await e.advisorSummary({ namespace: "portal", limit: 5 });
    expect(out.body).toEqual({ namespace: "portal", limit: 5 });
    expect(seen[seen.length - 1].auth).toBe(`Bearer ${TOKEN}`);
  });

  test("errors carry a machine-readable code, status and Retry-After", async () => {
    const e = createEngine({ base: base(), token: TOKEN });
    const err = (await e
      .advisorReview({ namespace: "a", kind: "Deployment", workload: "w" })
      .catch((x: Error) => x)) as EngineAdvisorError;
    expect(err).toBeInstanceOf(EngineAdvisorError);
    expect(err.code).toBe("rate_limited");
    expect(err.status).toBe(429);
    expect(err.retryAfter).toBe(42);
    expect(err.message).toBe(
      "[advisor:rate_limited:429] advisor hourly call limit reached [retry-after:42]",
    );
  });

  test("a missing token on the advisor POST is code unauthorized", async () => {
    const e = createEngine({ base: base() });
    const err = (await e.advisorSummary({}).catch((x: Error) => x)) as EngineAdvisorError;
    expect(err.code).toBe("unauthorized");
    expect(err.status).toBe(401);
  });
});

describe("setWorkload (workload and container writes)", () => {
  const fake = (status: number, body: string) => {
    const calls: { url: string; init?: RequestInit }[] = [];
    const fetch = async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      return new Response(body, { status });
    };
    return { calls, fetch };
  };
  const ref = { namespace: "portal", kind: "Deployment", workload: "gateway" };

  test("forwards the container body and bearer, returns the engine echo", async () => {
    const f = fake(
      200,
      '{"ok":true,"namespace":"portal","kind":"Deployment","workload":"gateway","container":"istio-proxy","mode":"inherit"}',
    );
    const e = createEngine({ base: "http://e", token: "t", fetch: f.fetch });
    const out: any = await e.setWorkload({ ...ref, container: "istio-proxy", mode: "inherit" });
    expect(out.mode).toBe("inherit");
    expect(f.calls[0].url).toBe("http://e/api/workload");
    expect(JSON.parse(String(f.calls[0].init?.body))).toEqual({
      ...ref,
      container: "istio-proxy",
      mode: "inherit",
    });
    expect((f.calls[0].init?.headers as Record<string, string>).authorization).toBe("Bearer t");
  });

  test.each([
    [400, "invalid_mode"],
    [400, "nothing_to_change"],
    [400, "unsupported_kind"],
    [400, "container_not_resizable"],
    [404, "workload_not_found"],
    [404, "container_not_found"],
  ])("maps %d %s to [engine:code:status]", async (status, code) => {
    const f = fake(status, JSON.stringify({ error: "nope", code }));
    const e = createEngine({ base: "http://e", fetch: f.fetch });
    const err = (await e.setWorkload(ref).catch((x: Error) => x)) as EngineWriteError;
    expect(err).toBeInstanceOf(EngineWriteError);
    expect(err.code).toBe(code);
    expect(err.message).toBe(`[engine:${code}:${status}] nope`);
  });

  test("guard refusals without a code (413/415) keep a stable prefix", async () => {
    const f = fake(415, "unsupported media type\n");
    const e = createEngine({ base: "http://e", fetch: f.fetch });
    const err = (await e.setWorkload(ref).catch((x: Error) => x)) as EngineWriteError;
    expect(err.message).toBe("[engine:upstream_error:415] unsupported media type");
  });

  test("protected reads (TAILOR_API_PROTECT_READS): 401 / 503 on a GET are auth errors", async () => {
    const e1 = createEngine({
      base: "http://e",
      fetch: fake(401, '{"error":"unauthorized"}').fetch,
    });
    const err = (await e1.recommendations().catch((x: Error) => x)) as EngineAuthError;
    expect(err).toBeInstanceOf(EngineAuthError);
    expect(err.read).toBe(true);
    expect(err.message).toContain("for reads (TAILOR_API_PROTECT_READS)");
    const e2 = createEngine({
      base: "http://e",
      token: "t",
      fetch: fake(
        503,
        '{"error":"read API disabled: TAILOR_API_PROTECT_READS is set but TAILOR_API_TOKEN is empty"}',
      ).fetch,
    });
    const err2 = (await e2.overview().catch((x: Error) => x)) as EngineAuthError;
    expect(err2).toBeInstanceOf(EngineAuthError);
    expect(err2.message).toContain("protects reads");
    // any other 503 on a read is a plain upstream error
    const e3 = createEngine({ base: "http://e", fetch: fake(503, "vm down").fetch });
    const err3 = (await e3.economy().catch((x: Error) => x)) as Error;
    expect(err3).not.toBeInstanceOf(EngineAuthError);
    expect(err3.message).toBe("engine /api/economy -> 503: vm down");
  });

  test("the token is sent on reads", async () => {
    const f = fake(200, "{}");
    const e = createEngine({ base: "http://e", token: "t0k", fetch: f.fetch });
    await e.recommendations();
    expect((f.calls[0].init?.headers as Record<string, string>).authorization).toBe("Bearer t0k");
  });

  test("401 and the auth 503 stay EngineAuthError", async () => {
    const e1 = createEngine({
      base: "http://e",
      fetch: fake(401, '{"error":"unauthorized"}').fetch,
    });
    expect(await e1.setWorkload(ref).catch((x: Error) => x)).toBeInstanceOf(EngineAuthError);
    const e2 = createEngine({
      base: "http://e",
      fetch: fake(503, "writes disabled: TAILOR_API_TOKEN not set").fetch,
    });
    expect(await e2.setWorkload(ref).catch((x: Error) => x)).toBeInstanceOf(EngineAuthError);
  });
});
