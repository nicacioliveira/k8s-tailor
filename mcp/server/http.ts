/**
 * HTTP front of the MCP server: routing, the Bearer gate and request hygiene.
 *
 * Kept apart from main.ts (tools/engine wiring) so the gate is unit-testable.
 *
 * Why node:http and not Bun.serve: Bun.serve hands the handler a NORMALIZED URL
 * (`POST /..\mcp` arrives as `/mcp`), so a path-based gate in front of it (the
 * nginx sidecar) and this server could disagree on which route a request hits —
 * the bypass of chart 0.2.0. node:http exposes the raw request-target, so this
 * server refuses anything that is not already canonical (400) and routes on the
 * exact string. The Bearer is required on /mcp whatever sits in front.
 */
import { createHash, timingSafeEqual } from "node:crypto";
import http from "node:http";

export interface GateConfig {
  /** Bearer token for /mcp. Empty only with allowOpen (dev). */
  token: string;
  /** MCP_ALLOW_UNAUTHENTICATED=true and no token: /mcp open (dev only). */
  allowOpen: boolean;
  /** Accepted Host names (no port). Empty: any with a token, loopback names when open. */
  allowedHosts: string[];
  /** Extra browser Origins accepted on /mcp besides the page's own origin. */
  allowedOrigins: string[];
  maxBodyBytes: number;
  maxBatch: number;
}

export interface Routes {
  /** One JSON-RPC message → response, or null for a notification. */
  rpc(msg: unknown): Promise<unknown | null>;
  ready(): Promise<boolean>;
  /** The dashboard HTML; throws when the UI is not built. */
  ui(): Promise<string>;
}

/**
 * Characters allowed in MCP_API_TOKEN (RFC 6750 token68). The same set the chart
 * enforces for the nginx sidecar, which embeds the token in its config: no quote,
 * semicolon, `$`, backslash, brace or whitespace can reach it.
 */
export const TOKEN_RE = /^[A-Za-z0-9._~+/=-]+$/;

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]", "::1"]);

const list = (v: string | undefined) =>
  (v ?? "")
    .split(",")
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean);

/** Gate configuration from the environment; throws on an unsafe setup (fail closed). */
export function configFromEnv(env: Record<string, string | undefined>): GateConfig {
  const token = env.MCP_API_TOKEN ?? "";
  const allowOpen = env.MCP_ALLOW_UNAUTHENTICATED === "true" && !token;
  if (!token && !allowOpen) {
    throw new Error(
      "refusing to start without MCP_API_TOKEN: /mcp forwards the engine's write token. " +
        "Set MCP_API_TOKEN (the chart does), or for local dev only MCP_ALLOW_UNAUTHENTICATED=true.",
    );
  }
  if (token && !TOKEN_RE.test(token)) {
    throw new Error("MCP_API_TOKEN may only contain A-Z a-z 0-9 . _ ~ + / = -");
  }
  const num = (v: string | undefined, def: number) => {
    const n = Number(v);
    return Number.isFinite(n) && n > 0 ? Math.floor(n) : def;
  };
  return {
    token,
    allowOpen,
    allowedHosts: list(env.MCP_ALLOWED_HOSTS),
    allowedOrigins: list(env.MCP_ALLOWED_ORIGINS).map((o) => o.replace(/\/+$/, "")),
    maxBodyBytes: num(env.MCP_MAX_BODY_BYTES, 1 << 20),
    maxBatch: num(env.MCP_MAX_BATCH, 20),
  };
}

/**
 * The path of a raw request-target when it is already canonical, else null:
 * origin-form only (no absolute-form / `*`), no backslash, no percent-encoding
 * (none of our routes needs it; `%2e`, `%2f`, `%5c` are the classic confusions),
 * no empty, `.` or `..` segment. The query string is ignored.
 */
export function canonicalPath(target: string): string | null {
  if (!target.startsWith("/")) return null;
  const q = target.indexOf("?");
  const path = q < 0 ? target : target.slice(0, q);
  if (/[\\%]/.test(path) || path.includes("//")) return null;
  for (const seg of path.split("/")) if (seg === "." || seg === "..") return null;
  return path;
}

/** Constant-time Bearer check (hashing first also hides the token length). */
export function bearerMatches(header: string | null, token: string): boolean {
  if (!token || !header?.startsWith("Bearer ")) return false;
  const a = createHash("sha256").update(header.slice(7)).digest();
  const b = createHash("sha256").update(token).digest();
  return timingSafeEqual(a, b);
}

function hostName(host: string): string {
  const v = host.trim().toLowerCase();
  if (v.startsWith("[")) {
    const i = v.indexOf("]");
    return i > 0 ? v.slice(0, i + 1) : v;
  }
  const i = v.lastIndexOf(":");
  return i >= 0 ? v.slice(0, i) : v;
}

/**
 * DNS-rebinding guard. With MCP_ALLOWED_HOSTS set, only those names. Otherwise an
 * OPEN server (no token) answers only loopback names — a rebound attacker page
 * sends its own name — and a token-gated one accepts any (the token, which such a
 * page cannot read, is the control).
 */
export function hostAllowed(host: string | null, cfg: GateConfig): boolean {
  if (!host) return false;
  const name = hostName(host);
  if (cfg.allowedHosts.length) return cfg.allowedHosts.includes(name);
  if (!cfg.token) return LOOPBACK.has(name);
  return true;
}

/**
 * CSRF guard for /mcp: a browser always sends Origin on a POST. Accept none (a
 * non-browser client), the page's own origin (Origin host == Host) or an
 * explicitly listed one. `null` (sandboxed iframe, file://) is refused.
 */
export function originAllowed(
  origin: string | null,
  host: string | null,
  cfg: GateConfig,
): boolean {
  if (origin === null || origin === "") return true;
  const o = origin.trim().toLowerCase().replace(/\/+$/, "");
  if (cfg.allowedOrigins.includes(o)) return true;
  let u: URL;
  try {
    u = new URL(o);
  } catch {
    return false;
  }
  if ((u.protocol !== "http:" && u.protocol !== "https:") || !host) return false;
  return u.host === host.trim().toLowerCase();
}

function isJson(ct: string | null): boolean {
  return (ct ?? "").split(";")[0].trim().toLowerCase() === "application/json";
}

const text = (body: string, status: number, headers: Record<string, string> = {}) =>
  new Response(body, {
    status,
    headers: { "content-type": "text/plain; charset=utf-8", ...headers },
  });
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const rpcError = (code: number, message: string) => ({
  jsonrpc: "2.0",
  id: null,
  error: { code, message },
});

// The shell holds the token dialog: never framed by another origin, never cached.
const PAGE_HEADERS = {
  "content-type": "text/html; charset=utf-8",
  "cache-control": "no-store",
  "x-content-type-options": "nosniff",
  "x-frame-options": "DENY",
  "content-security-policy": "frame-ancestors 'none'",
  "referrer-policy": "no-referrer",
};

/** Request handler on the CANONICAL path (see canonicalPath) and a body already capped. */
export function createHandler(cfg: GateConfig, routes: Routes) {
  return async (path: string, req: Request): Promise<Response> => {
    const m = req.method;
    const read = m === "GET" || m === "HEAD";
    switch (path) {
      case "/healthz":
        return read ? text("ok", 200) : text("method not allowed", 405, { allow: "GET, HEAD" });
      case "/readyz": {
        if (!read) return text("method not allowed", 405, { allow: "GET, HEAD" });
        const ok = await routes.ready();
        return text(ok ? "ok" : "engine down", ok ? 200 : 503);
      }
      case "/":
      case "/index.html": {
        if (!read) return text("method not allowed", 405, { allow: "GET, HEAD" });
        if (!hostAllowed(req.headers.get("host"), cfg)) return text("host not allowed", 403);
        try {
          return new Response(await routes.ui(), { headers: PAGE_HEADERS });
        } catch {
          return text("UI not built — run `bun run build:web`", 503);
        }
      }
      case "/mcp":
        return mcp(req);
      default:
        return text("not found", 404);
    }
  };

  async function mcp(req: Request): Promise<Response> {
    if (req.method !== "POST") return text("method not allowed", 405, { allow: "POST" });
    const host = req.headers.get("host");
    if (!hostAllowed(host, cfg)) return text("host not allowed", 403);
    if (!originAllowed(req.headers.get("origin"), host, cfg)) {
      return text("cross-origin request refused", 403);
    }
    if (!cfg.allowOpen && !bearerMatches(req.headers.get("authorization"), cfg.token)) {
      return text("unauthorized", 401, { "www-authenticate": "Bearer" });
    }
    if (!isJson(req.headers.get("content-type"))) {
      return text("Content-Type must be application/json", 415);
    }
    let msg: unknown;
    try {
      msg = JSON.parse(await req.text());
    } catch {
      return json(rpcError(-32700, "parse error"));
    }
    if (Array.isArray(msg)) {
      if (msg.length === 0) return json(rpcError(-32600, "empty batch"), 400);
      if (msg.length > cfg.maxBatch) {
        return json(rpcError(-32600, `batch larger than ${cfg.maxBatch}`), 413);
      }
      const out = (await Promise.all(msg.map((x) => routes.rpc(x)))).filter((r) => r !== null);
      return out.length ? json(out) : new Response(null, { status: 202 });
    }
    const res = await routes.rpc(msg);
    return res === null ? new Response(null, { status: 202 }) : json(res);
  }
}

const BODY_DEADLINE_MS = 30_000;

/**
 * node:http server around createHandler: rejects a non-canonical request-target
 * (400) and an oversized body (413) before any routing, with timeouts against
 * slow clients.
 */
export function serve(cfg: GateConfig, routes: Routes, host: string, port: number): http.Server {
  const handler = createHandler(cfg, routes);
  const server = http.createServer({ maxHeaderSize: 16 * 1024 }, async (req, res) => {
    const send = async (r: Response) => {
      const headers: Record<string, string> = {};
      r.headers.forEach((v, k) => {
        headers[k] = v;
      });
      res.writeHead(r.status, headers);
      res.end(req.method === "HEAD" ? undefined : Buffer.from(await r.arrayBuffer()));
    };
    try {
      const path = canonicalPath(req.url ?? "");
      if (path === null) return await send(text("bad request: non-canonical path", 400));
      const declared = Number(req.headers["content-length"] ?? 0);
      if (declared > cfg.maxBodyBytes) {
        res.setHeader("connection", "close");
        return await send(text("request body too large", 413));
      }
      const chunks: Buffer[] = [];
      let size = 0;
      // Body deadline (a trickled body would otherwise hold the socket forever:
      // Bun's node:http ignores requestTimeout).
      const deadline = setTimeout(() => req.destroy(), BODY_DEADLINE_MS);
      try {
        for await (const c of req) {
          size += (c as Buffer).length;
          if (size > cfg.maxBodyBytes) {
            res.setHeader("connection", "close");
            return await send(text("request body too large", 413));
          }
          chunks.push(c as Buffer);
        }
      } finally {
        clearTimeout(deadline);
      }
      const headers = new Headers();
      for (const [k, v] of Object.entries(req.headers)) {
        if (v !== undefined) headers.set(k, Array.isArray(v) ? v.join(", ") : v);
      }
      const method = req.method ?? "GET";
      const hasBody = method !== "GET" && method !== "HEAD";
      const request = new Request(`http://mcp.invalid${path}`, {
        method,
        headers,
        body: hasBody ? Buffer.concat(chunks) : undefined,
      });
      await send(await handler(path, request));
    } catch (e) {
      console.error("request failed:", e);
      if (!res.headersSent) res.writeHead(500, { "content-type": "text/plain" });
      res.end("internal error");
    }
  });
  // Node semantics; under Bun the header phase is bounded by Bun's own ~10 s idle
  // timeout (verified) and the body by BODY_DEADLINE_MS above. A slow HANDLER
  // (advisor calls, up to the engine's 3 min write timeout) is not cut.
  server.headersTimeout = 10_000;
  server.requestTimeout = BODY_DEADLINE_MS;
  server.keepAliveTimeout = 5_000;
  server.setTimeout(240_000);
  server.listen(port, host);
  return server;
}
