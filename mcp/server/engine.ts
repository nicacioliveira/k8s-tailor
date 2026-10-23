/**
 * Thin HTTP client for the Go tailor engine's API.
 * The MCP layer never talks to Kubernetes directly — it proxies the engine,
 * which owns collection, recommendation and (in-cluster) actuation.
 *
 * Auth: the engine gates every mutating /api/* call behind
 * `Authorization: Bearer <TAILOR_API_TOKEN>` when that token is configured, and
 * also the /api/* reads when it runs with TAILOR_API_PROTECT_READS=true. Set the
 * same TAILOR_API_TOKEN on this MCP server and it is sent on every request, reads
 * included.
 */

export type Range = { window?: string; step?: number; from?: number; to?: number };

export interface EngineOptions {
  base?: string;
  token?: string;
  fetch?: (input: string, init?: RequestInit) => Promise<Response>;
  timeoutMs?: number;
}

/** Engine refused the call for lack of (or a wrong) bearer token. */
export class EngineAuthError extends Error {
  constructor(
    readonly path: string,
    readonly status: number,
    hasToken: boolean,
    /** The refused call was a read (GET): the engine protects reads (TAILOR_API_PROTECT_READS). */
    readonly read = false,
  ) {
    super(
      status === 503
        ? read
          ? `engine ${path} -> 503: the engine protects reads (TAILOR_API_PROTECT_READS) but has no token configured — set TAILOR_API_TOKEN on the engine`
          : `engine ${path} -> 503: the engine requires a write token (TAILOR_API_AUTH_REQUIRED) but has none configured — set TAILOR_API_TOKEN on the engine`
        : hasToken
          ? `engine ${path} -> 401: the engine rejected TAILOR_API_TOKEN — it does not match the engine's token`
          : read
            ? `engine ${path} -> 401: the engine requires a bearer token for reads (TAILOR_API_PROTECT_READS) — set TAILOR_API_TOKEN on the MCP server to the engine's token`
            : `engine ${path} -> 401: the engine requires a bearer token for changes — set TAILOR_API_TOKEN on the MCP server to the engine's token`,
    );
    this.name = "EngineAuthError";
  }
}

/**
 * The advisor endpoints answer errors as `{error, code}` (+ `Retry-After` on 429/503).
 * The message carries the code in a stable, machine-readable prefix so the dashboard
 * (which only sees the MCP tool-error text) can map it: `[advisor:<code>:<status>] msg`
 * and, when the engine sent one, ` [retry-after:<seconds>]`.
 */
export class EngineAdvisorError extends Error {
  constructor(
    readonly path: string,
    readonly status: number,
    readonly code: string,
    detail: string,
    readonly retryAfter?: number,
  ) {
    super(
      `[advisor:${code}:${status}] ${detail}${retryAfter != null ? ` [retry-after:${retryAfter}]` : ""}`,
    );
    this.name = "EngineAdvisorError";
  }
}

/**
 * `/api/workload` answers refusals as `{error, code}` (400 bad_request | invalid_mode |
 * nothing_to_change | unsupported_kind | container_not_resizable, 404 workload_not_found |
 * container_not_found). Same stable prefix idea as the advisor: `[engine:<code>:<status>] msg`.
 * 401 / auth-related 503 stay EngineAuthError; the 413/415 guards carry no code.
 */
export class EngineWriteError extends Error {
  constructor(
    readonly path: string,
    readonly status: number,
    readonly code: string,
    detail: string,
  ) {
    super(`[engine:${code}:${status}] ${detail}`);
    this.name = "EngineWriteError";
  }
}

function enc(s: string) {
  return encodeURIComponent(s);
}

function rangeQS(r: Range = {}): string {
  const p = new URLSearchParams();
  if (r.window) p.set("window", r.window);
  if (r.step) p.set("step", String(r.step));
  if (r.from) p.set("from", String(r.from));
  if (r.to) p.set("to", String(r.to));
  const s = p.toString();
  return s ? `?${s}` : "";
}

export function createEngine(opts: EngineOptions = {}) {
  const base = (opts.base ?? "http://127.0.0.1:8080").replace(/\/$/, "");
  const token = opts.token ?? "";
  const doFetch = opts.fetch ?? ((i: string, init?: RequestInit) => fetch(i, init));
  const timeoutMs = opts.timeoutMs ?? 15_000;

  async function advisorError(res: Response, path: string): Promise<Error> {
    const text = await res.text();
    let body: { error?: unknown; code?: unknown } = {};
    try {
      body = JSON.parse(text);
    } catch {
      /* not JSON (e.g. an older engine without the advisor routes) */
    }
    const msg = typeof body.error === "string" ? body.error : text.trim() || res.statusText;
    const ra = Number.parseInt(res.headers.get("retry-after") ?? "", 10);
    const retryAfter = Number.isFinite(ra) && ra > 0 ? ra : undefined;
    let code = typeof body.code === "string" ? body.code : "";
    if (!code) {
      if (res.status === 401) code = "unauthorized";
      else if (res.status === 503 && text.includes("TAILOR_API")) code = "writes_disabled";
      else code = "upstream_error";
    }
    return new EngineAdvisorError(path, res.status, code, msg, retryAfter);
  }

  async function writeError(res: Response, path: string): Promise<Error> {
    const text = await res.text();
    let body: { error?: unknown; code?: unknown } = {};
    try {
      body = JSON.parse(text);
    } catch {
      /* guard responses (413/415) are plain text */
    }
    const msg = typeof body.error === "string" ? body.error : text.trim() || res.statusText;
    const code = typeof body.code === "string" && body.code ? body.code : "upstream_error";
    return new EngineWriteError(path, res.status, code, msg);
  }

  async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {};
    if (body !== undefined) headers["content-type"] = "application/json";
    if (token) headers.authorization = `Bearer ${token}`;
    const res = await doFetch(`${base}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok && path.startsWith("/api/advisor/")) throw await advisorError(res, path);
    if (!res.ok && path === "/api/workload" && res.status !== 401 && res.status !== 503) {
      throw await writeError(res, path);
    }
    if (res.status === 401 || res.status === 503) {
      const text = await res.text();
      // A 503 is only an auth problem when the engine says so (write token required,
      // or reads protected) — otherwise it is an ordinary upstream failure.
      if (res.status === 401 || text.includes("TAILOR_API")) {
        throw new EngineAuthError(path, res.status, !!token, method === "GET");
      }
      throw new Error(`engine ${path} -> ${res.status}: ${text}`);
    }
    if (!res.ok) throw new Error(`engine ${path} -> ${res.status}: ${await res.text()}`);
    const text = await res.text();
    return (text ? JSON.parse(text) : {}) as T;
  }

  const get = <T>(path: string) => request<T>("GET", path);
  const post = <T>(path: string, body: unknown) => request<T>("POST", path, body);
  const del = <T>(path: string) => request<T>("DELETE", path);

  return {
    base,
    hasToken: !!token,
    overview: (r?: Range) => get<unknown>(`/api/overview${rangeQS(r)}`),
    recommendations: () => get<unknown>("/api/recommendations"),
    actuations: (r?: Range & { q?: string; limit?: number }) => {
      const p = new URLSearchParams();
      if (r?.window) p.set("window", r.window);
      if (r?.from) p.set("from", String(r.from));
      if (r?.to) p.set("to", String(r.to));
      if (r?.q) p.set("q", r.q);
      if (r?.limit) p.set("limit", String(r.limit));
      const s = p.toString();
      return get<unknown>(`/api/actuations${s ? `?${s}` : ""}`);
    },
    detail: (ns: string, kind: string, workload: string, container: string, r?: Range) =>
      get<unknown>(
        `/api/detail/${enc(ns)}/${enc(kind)}/${enc(workload)}/${enc(container)}${rangeQS(r)}`,
      ),
    patch: (ns: string, kind: string, workload: string, container: string) =>
      get<unknown>(`/api/patch/${enc(ns)}/${enc(kind)}/${enc(workload)}/${enc(container)}`),
    economy: () => get<unknown>("/api/economy"),
    health: () => get<unknown>("/healthz"),
    setPaused: (paused: boolean) => post<unknown>("/api/pause", { paused }),
    profiles: () => get<unknown>("/api/profiles"),
    setWorkload: (b: unknown) => post<unknown>("/api/workload", b),
    policies: () => get<unknown>("/api/policies"),
    setPolicy: (p: unknown) => post<unknown>("/api/policies", p),
    deletePolicy: (name: string) => del<unknown>(`/api/policies/${enc(name)}`),
    groups: () => get<unknown>("/api/groups"),
    groupMembers: (name: string) => get<unknown>(`/api/groups/${enc(name)}/members`),
    setGroup: (g: unknown) => post<unknown>("/api/groups", g),
    deleteGroup: (name: string) => del<unknown>(`/api/groups/${enc(name)}`),
    advisorStatus: () => get<unknown>("/api/advisor/status"),
    advisorReview: (b: { namespace: string; kind: string; workload: string; container?: string }) =>
      post<unknown>("/api/advisor/review", b),
    advisorSummary: (b: { namespace?: string; limit?: number }) =>
      post<unknown>("/api/advisor/summary", b),
    healthy: async () => {
      try {
        const res = await doFetch(`${base}/healthz`, { signal: AbortSignal.timeout(5_000) });
        return res.ok;
      } catch {
        return false;
      }
    },
  };
}

export const engine = createEngine({
  base: process.env.TAILOR_API,
  token: process.env.TAILOR_API_TOKEN,
});

export const ENGINE_BASE = engine.base;
