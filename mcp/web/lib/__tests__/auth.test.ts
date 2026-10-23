import { beforeEach, describe, expect, test } from "bun:test";
import type { AuthError, AuthRequest } from "../auth";
import { MemStorage } from "./fixtures";

// Storage must exist before lib/auth reads it.
Object.assign(globalThis, { sessionStorage: new MemStorage(), localStorage: new MemStorage() });

const auth = await import("../auth");
const { createRestShim, route } = await import("../rest-transport");
const { step } = await import("../shortcuts");

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();
});

describe("token storage", () => {
  test("session by default, local only when remembered; trims; clear removes both", () => {
    auth.setToken("  abc  ");
    expect(sessionStorage.getItem("tailor_token")).toBe("abc");
    expect(auth.isRemembered()).toBe(false);
    auth.setToken("xyz", true);
    expect(sessionStorage.getItem("tailor_token")).toBeNull();
    expect(auth.getToken()).toBe("xyz");
    expect(auth.isRemembered()).toBe(true);
    auth.clearToken();
    expect(auth.getToken()).toBe("");
  });
  test("headers only when a token exists", () => {
    expect(auth.authHeaders()).toEqual({});
    auth.setToken("t");
    expect(auth.authHeaders()).toEqual({ Authorization: "Bearer t" });
  });
  test("change listeners fire on set and clear", () => {
    let n = 0;
    const off = auth.onTokenChange(() => n++);
    auth.setToken("a");
    auth.clearToken();
    off();
    auth.setToken("b");
    expect(n).toBe(2);
  });
});

describe("consumeUrlToken (fragment only: a query string reaches proxy logs)", () => {
  const run = (href: string) => {
    let replaced: string | null = null;
    const prev = (globalThis as any).history;
    (globalThis as any).history = {
      replaceState: (_: unknown, __: string, u: string) => {
        replaced = u;
      },
    };
    try {
      auth.consumeUrlToken({ href } as Location);
    } finally {
      (globalThis as any).history = prev;
    }
    return replaced as string | null;
  };

  test("#token=<t> is stored for the session and removed from the address bar", () => {
    expect(run("https://t.example/#token=frag123")).toBe("https://t.example/");
    expect(sessionStorage.getItem("tailor_token")).toBe("frag123");
    expect(localStorage.getItem("tailor_token")).toBeNull();
  });
  test("#/<view>?token=<t> keeps the view and the other params", () => {
    expect(run("https://t.example/#/workloads?ns=a&token=t2")).toBe(
      "https://t.example/#/workloads?ns=a",
    );
    expect(auth.getToken()).toBe("t2");
  });
  test("?token=<t> is NOT accepted, only stripped", () => {
    expect(run("https://t.example/?token=leaked&x=1#/overview")).toBe(
      "https://t.example/?x=1#/overview",
    );
    expect(auth.getToken()).toBe("");
  });
  test("no token anywhere: the URL is left alone", () => {
    expect(run("https://t.example/#/overview?ns=a")).toBeNull();
    expect(auth.getToken()).toBe("");
  });
});

describe("REST transport auth handling", () => {
  type Call = { url: string; init?: RequestInit };
  const fakeFetch =
    (status: number, body: string, calls: Call[]) => async (url: string, init?: RequestInit) => {
      calls.push({ url, init });
      return new Response(body, { status });
    };

  test("sends the bearer token and maps tools to the engine API", async () => {
    auth.setToken("s3cret");
    const calls: Call[] = [];
    const shim = createRestShim("", fakeFetch(200, '{"paused":true}', calls));
    const out: any = await shim.callServerTool({
      name: "TAILOR_SET_PAUSED",
      arguments: { paused: true },
    });
    expect(out.structuredContent).toEqual({ paused: true });
    expect(calls[0].url).toBe("/api/pause");
    expect((calls[0].init?.headers as Record<string, string>).Authorization).toBe("Bearer s3cret");
  });

  test("401 without a token: AuthError asking for it + auth requested", async () => {
    const seen: AuthRequest[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const shim = createRestShim("", fakeFetch(401, '{"error":"unauthorized"}', []));
    const err = (await shim
      .callServerTool({
        name: "TAILOR_SET_WORKLOAD",
        arguments: { namespace: "a", kind: "b", workload: "c" },
      })
      .catch((e: Error) => e)) as AuthError;
    off();
    expect(err).toBeInstanceOf(auth.AuthError);
    expect(err.status).toBe(401);
    expect(err.message).toContain("TAILOR_API_TOKEN");
    expect(seen).toEqual([{ reason: "rejected", message: err.message, target: "engine" }]);
  });

  test("401 with a token says it was rejected", async () => {
    auth.setToken("old");
    const shim = createRestShim("", fakeFetch(401, "", []));
    const err = (await shim
      .callServerTool({ name: "TAILOR_SET_PAUSED", arguments: {} })
      .catch((e: Error) => e)) as Error;
    expect(err.message).toContain("was rejected");
  });

  test("503 auth-required is a server misconfiguration: no prompt", async () => {
    const seen: unknown[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const shim = createRestShim(
      "",
      fakeFetch(
        503,
        '{"error":"write API disabled: TAILOR_API_AUTH_REQUIRED is set but TAILOR_API_TOKEN is empty"}',
        [],
      ),
    );
    const err = (await shim
      .callServerTool({ name: "TAILOR_DELETE_POLICY", arguments: { name: "x" } })
      .catch((e: Error) => e)) as AuthError;
    off();
    expect(err.fixable).toBe(false);
    expect(err.message).toContain("Set TAILOR_API_TOKEN on the engine");
    expect(seen).toHaveLength(0);
  });

  test("reads carry the bearer token too (TAILOR_API_PROTECT_READS)", async () => {
    auth.setToken("s3cret");
    const calls: Call[] = [];
    const shim = createRestShim("", fakeFetch(200, '{"recommendations":[]}', calls));
    await shim.callServerTool({ name: "TAILOR_LIST_RECOMMENDATIONS", arguments: {} });
    expect(calls[0].init?.method).toBe("GET");
    expect((calls[0].init?.headers as Record<string, string>).Authorization).toBe("Bearer s3cret");
  });

  test("401 on a read: AuthError about reading + the token dialog is requested", async () => {
    const seen: AuthRequest[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const shim = createRestShim("", fakeFetch(401, '{"error":"unauthorized"}', []));
    const err = (await shim
      .callServerTool({ name: "TAILOR_OVERVIEW", arguments: {} })
      .catch((e: Error) => e)) as AuthError;
    off();
    expect(err).toBeInstanceOf(auth.AuthError);
    expect(err.fixable).toBe(true);
    expect(err.message).toContain("reading needs");
    expect(seen).toEqual([
      { reason: "rejected", message: err.message, target: "engine", read: true },
    ]);
  });

  test("503 reads disabled (no token on the engine): not fixable, no prompt", async () => {
    const seen: unknown[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const shim = createRestShim(
      "",
      fakeFetch(
        503,
        '{"error":"read API disabled: TAILOR_API_PROTECT_READS is set but TAILOR_API_TOKEN is empty"}',
        [],
      ),
    );
    const err = (await shim
      .callServerTool({ name: "TAILOR_LIST_POLICIES", arguments: {} })
      .catch((e: Error) => e)) as AuthError;
    off();
    expect(err).toBeInstanceOf(auth.AuthError);
    expect(err.fixable).toBe(false);
    expect(err.message).toContain("Reading is disabled");
    expect(seen).toHaveLength(0);
  });

  test("an ordinary 503 on a read stays an upstream error", async () => {
    const shim = createRestShim("", fakeFetch(503, '{"error":"vm unavailable"}', []));
    const err = (await shim
      .callServerTool({ name: "TAILOR_ECONOMY", arguments: {} })
      .catch((e: Error) => e)) as Error;
    expect(err).not.toBeInstanceOf(auth.AuthError);
    expect(err.message).toBe("/api/economy: 503 vm unavailable");
  });

  test("other errors surface the engine's message", async () => {
    const shim = createRestShim("", fakeFetch(500, '{"error":"etcd timeout"}', []));
    const err = (await shim
      .callServerTool({ name: "TAILOR_LIST_POLICIES", arguments: {} })
      .catch((e: Error) => e)) as Error;
    expect(err.message).toBe("/api/policies: 500 etcd timeout");
  });

  test("route encodes path segments and drops empty query args", () => {
    expect(
      route("TAILOR_GET_DETAIL", {
        namespace: "a b",
        kind: "K",
        workload: "w/1",
        container: "c",
        window: "1h",
        step: "",
      })[1],
    ).toBe("/api/detail/a%20b/K/w%2F1/c?window=1h");
    expect(route("TAILOR_HEALTH", {})).toEqual(["GET", "/healthz"]);
    expect(() => route("NOPE", {})).toThrow("unknown tool");
  });
});

describe("keyboard shortcut state machine", () => {
  test("g then a key navigates; anything else resets", () => {
    let s = step(false, "g");
    expect(s).toEqual({ action: null, pendingG: true });
    expect(step(s.pendingG, "w").action).toEqual({ type: "go", view: "workloads" });
    expect(step(true, "x")).toEqual({ action: null, pendingG: false });
    expect(step(false, "/").action).toEqual({ type: "focus-filter" });
    expect(step(false, "?").action).toEqual({ type: "help" });
    expect(step(false, "w").action).toBeNull();
  });
});
