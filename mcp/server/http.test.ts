import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import net from "node:net";
import {
  type GateConfig,
  type Routes,
  bearerMatches,
  canonicalPath,
  configFromEnv,
  createHandler,
  hostAllowed,
  originAllowed,
  serve,
} from "./http.ts";

const TOKEN = "s3cretToken0123456789abcdefABCDEF";
const cfg = (over: Partial<GateConfig> = {}): GateConfig => ({
  token: TOKEN,
  allowOpen: false,
  allowedHosts: [],
  allowedOrigins: [],
  maxBodyBytes: 1024,
  maxBatch: 3,
  ...over,
});

let rpcCalls = 0;
const routes: Routes = {
  rpc: async (m: any) => {
    rpcCalls++;
    return m?.method === "notifications/initialized"
      ? null
      : { jsonrpc: "2.0", id: m?.id, result: {} };
  },
  ready: async () => true,
  ui: async () => "<html>ui</html>",
};

const body = JSON.stringify({ jsonrpc: "2.0", id: 1, method: "ping" });
const post = (headers: Record<string, string> = {}, b: string = body) =>
  new Request("http://x/mcp", {
    method: "POST",
    headers: {
      host: "tailor.example.com",
      "content-type": "application/json",
      authorization: `Bearer ${TOKEN}`,
      ...headers,
    },
    body: b,
  });

describe("canonicalPath", () => {
  test("accepts canonical paths, drops the query", () => {
    expect(canonicalPath("/mcp")).toBe("/mcp");
    expect(canonicalPath("/")).toBe("/");
    expect(canonicalPath("/mcp?x=1")).toBe("/mcp");
  });
  test.each([
    "/..\\mcp",
    "/x\\..\\mcp",
    "/x/..\\mcp",
    "//mcp",
    "/%2e%2e/mcp",
    "/%2E%2E/mcp",
    "/mcp/../mcp",
    "/./mcp",
    "/%5cmcp",
    "/%2fmcp",
    "/mcp/.",
    "/..",
    "http://evil/mcp",
    "*",
    "",
    "mcp",
  ])("refuses %p", (t) => {
    expect(canonicalPath(t)).toBeNull();
  });
});

describe("bearerMatches", () => {
  test("exact Bearer only", () => {
    expect(bearerMatches(`Bearer ${TOKEN}`, TOKEN)).toBe(true);
    expect(bearerMatches(`Bearer ${TOKEN}x`, TOKEN)).toBe(false);
    expect(bearerMatches(`bearer ${TOKEN}`, TOKEN)).toBe(false);
    expect(bearerMatches(TOKEN, TOKEN)).toBe(false);
    expect(bearerMatches(null, TOKEN)).toBe(false);
    expect(bearerMatches("Bearer ", "")).toBe(false);
  });
});

describe("configFromEnv", () => {
  test("token required unless explicitly open (any bind address)", () => {
    expect(() => configFromEnv({})).toThrow(/refusing to start/);
    expect(() => configFromEnv({ MCP_HOST: "127.0.0.1" })).toThrow(/refusing to start/);
    expect(configFromEnv({ MCP_ALLOW_UNAUTHENTICATED: "true" }).allowOpen).toBe(true);
  });
  test("a token always wins over allowOpen", () => {
    const c = configFromEnv({ MCP_API_TOKEN: TOKEN, MCP_ALLOW_UNAUTHENTICATED: "true" });
    expect(c.allowOpen).toBe(false);
    expect(c.token).toBe(TOKEN);
  });
  test.each(['a"b', "a;b", "a$b", "a b", "a\\b", "a{b", "a\nb"])(
    "refuses token with unsafe characters %p",
    (t) => {
      expect(() => configFromEnv({ MCP_API_TOKEN: t })).toThrow(/may only contain/);
    },
  );
  test("lists and limits", () => {
    const c = configFromEnv({
      MCP_API_TOKEN: "abc-._~+/=",
      MCP_ALLOWED_HOSTS: " Tailor.Example.com ,b ",
      MCP_ALLOWED_ORIGINS: "https://a.example/",
      MCP_MAX_BATCH: "5",
      MCP_MAX_BODY_BYTES: "junk",
    });
    expect(c.allowedHosts).toEqual(["tailor.example.com", "b"]);
    expect(c.allowedOrigins).toEqual(["https://a.example"]);
    expect(c.maxBatch).toBe(5);
    expect(c.maxBodyBytes).toBe(1 << 20);
  });
});

describe("host / origin guards", () => {
  test("open server: loopback names only (DNS rebinding)", () => {
    const open = cfg({ token: "", allowOpen: true });
    expect(hostAllowed("localhost:8020", open)).toBe(true);
    expect(hostAllowed("127.0.0.1:8020", open)).toBe(true);
    expect(hostAllowed("[::1]:8020", open)).toBe(true);
    expect(hostAllowed("evil.example:8020", open)).toBe(false);
    expect(hostAllowed(null, open)).toBe(false);
  });
  test("token server: any host unless an allowlist is set", () => {
    expect(hostAllowed("anything:1", cfg())).toBe(true);
    const c = cfg({ allowedHosts: ["tailor.example.com"] });
    expect(hostAllowed("Tailor.Example.com:443", c)).toBe(true);
    expect(hostAllowed("other.example.com", c)).toBe(false);
  });
  test("origin must match the Host or be listed; null refused", () => {
    expect(originAllowed(null, "h:1", cfg())).toBe(true);
    expect(originAllowed("http://h:1", "h:1", cfg())).toBe(true);
    expect(originAllowed("https://tailor.example.com", "tailor.example.com", cfg())).toBe(true);
    expect(originAllowed("http://evil", "h:1", cfg())).toBe(false);
    expect(originAllowed("null", "h:1", cfg())).toBe(false);
    expect(originAllowed("file://x", "x", cfg())).toBe(false);
    expect(originAllowed("http://localhost:5179", "localhost:8020", cfg())).toBe(false);
    expect(
      originAllowed(
        "http://localhost:5179",
        "localhost:8020",
        cfg({ allowedOrigins: ["http://localhost:5179"] }),
      ),
    ).toBe(true);
  });
});

describe("handler", () => {
  const h = createHandler(cfg(), routes);
  test("canonical POST /mcp with the token reaches the RPC", async () => {
    const before = rpcCalls;
    const r = await h("/mcp", post());
    expect(r.status).toBe(200);
    expect(rpcCalls).toBe(before + 1);
  });
  test("missing / wrong token: 401, RPC untouched", async () => {
    const before = rpcCalls;
    expect((await h("/mcp", post({ authorization: "" }))).status).toBe(401);
    expect((await h("/mcp", post({ authorization: "Bearer nope" }))).status).toBe(401);
    expect(rpcCalls).toBe(before);
  });
  test("open dev mode needs no token", async () => {
    const open = createHandler(cfg({ token: "", allowOpen: true }), routes);
    expect((await open("/mcp", post({ authorization: "", host: "localhost:8020" }))).status).toBe(
      200,
    );
    expect((await open("/mcp", post({ authorization: "", host: "rebound.evil" }))).status).toBe(
      403,
    );
  });
  test("method, content type, origin", async () => {
    for (const method of ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"]) {
      const r = await h("/mcp", new Request("http://x/mcp", { method }));
      expect(r.status).toBe(405);
    }
    expect((await h("/mcp", post({ "content-type": "text/plain" }))).status).toBe(415);
    expect(
      (await h("/mcp", post({ "content-type": "application/x-www-form-urlencoded" }))).status,
    ).toBe(415);
    expect(
      (await h("/mcp", post({ "content-type": "application/json; charset=utf-8" }))).status,
    ).toBe(200);
    expect((await h("/mcp", post({ origin: "https://evil.example" }))).status).toBe(403);
    expect((await h("/mcp", post({ origin: "https://tailor.example.com" }))).status).toBe(200);
  });
  test("batch limits", async () => {
    const msgs = (n: number) =>
      JSON.stringify(
        Array.from({ length: n }, (_, i) => ({ jsonrpc: "2.0", id: i, method: "ping" })),
      );
    expect((await h("/mcp", post({}, msgs(3)))).status).toBe(200);
    expect((await h("/mcp", post({}, msgs(4)))).status).toBe(413);
    expect((await h("/mcp", post({}, "[]"))).status).toBe(400);
    expect((await (await h("/mcp", post({}, "{"))).json()).error.code).toBe(-32700);
  });
  test("UI shell: GET only, hardened headers; unknown paths 404", async () => {
    const r = await h("/", new Request("http://x/", { headers: { host: "h" } }));
    expect(r.status).toBe(200);
    expect(r.headers.get("x-frame-options")).toBe("DENY");
    expect(r.headers.get("cache-control")).toBe("no-store");
    expect((await h("/", new Request("http://x/", { method: "POST", body: "{}" }))).status).toBe(
      405,
    );
    expect((await h("/MCP", post())).status).toBe(404);
    expect((await h("/healthz", new Request("http://x/healthz"))).status).toBe(200);
  });
});

describe("serve (raw sockets: the request-target the client really sent)", () => {
  let server: ReturnType<typeof serve>;
  let port = 0;
  beforeAll(async () => {
    server = serve(cfg(), routes, "127.0.0.1", 0);
    await new Promise<void>((r) => server.once("listening", () => r()));
    port = (server.address() as { port: number }).port;
  });
  afterAll(() => server.close());

  const raw = (target: string, opts: { token?: boolean; body?: string; extra?: string } = {}) =>
    new Promise<number>((resolve) => {
      const b = opts.body ?? body;
      const c = net.connect(port, "127.0.0.1", () =>
        c.write(
          `POST ${target} HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n` +
            (opts.token ? `Authorization: Bearer ${TOKEN}\r\n` : "") +
            (opts.extra ?? "") +
            `Content-Length: ${Buffer.byteLength(b)}\r\nConnection: close\r\n\r\n${b}`,
        ),
      );
      let out = "";
      c.on("data", (d) => {
        out += d;
      });
      c.on("end", () => resolve(Number(out.split(" ")[1])));
      c.on("error", () => resolve(0));
    });

  test.each([
    "/..\\mcp",
    "/x\\..\\mcp",
    "/%2e%2e/mcp",
    "/mcp/../mcp",
    "//mcp",
    "/./mcp",
    "http://evil/mcp",
  ])("%p is 400 even WITH the token (never routed to /mcp)", async (t) => {
    const before = rpcCalls;
    expect(await raw(t, { token: true })).toBe(400);
    expect(rpcCalls).toBe(before);
  });
  test("canonical /mcp: 401 without, 200 with the token", async () => {
    expect(await raw("/mcp")).toBe(401);
    expect(await raw("/mcp", { token: true })).toBe(200);
  });
  test("body cap: 413", async () => {
    expect(await raw("/mcp", { token: true, body: "x".repeat(2048) })).toBe(413);
  });
});
