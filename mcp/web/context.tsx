import type { App } from "@modelcontextprotocol/ext-apps";
import { useApp } from "@modelcontextprotocol/ext-apps/react";
import { createContext, useContext, useEffect, useRef, useState } from "react";
import { authErrorFor, authHeaders, getToken, requestAuth } from "./lib/auth";
import { createRestShim } from "./lib/rest-transport";

// Only the slice of App our hooks use.
export interface ToolCaller {
  callServerTool: App["callServerTool"];
}

const McpAppContext = createContext<ToolCaller | null>(null);

// Auth (standalone only — inside an MCP host the host proxies it): see lib/auth.ts.
export { getToken, setToken } from "./lib/auth";

async function parseMcp(res: Response): Promise<any> {
  const ct = res.headers.get("content-type") ?? "";
  if (ct.includes("text/event-stream")) {
    const lines = (await res.text()).split("\n").filter((l) => l.startsWith("data: "));
    const last = lines[lines.length - 1];
    if (last) return JSON.parse(last.slice(6));
    throw new Error("empty SSE response");
  }
  return res.json();
}

const API_BASE =
  import.meta.env.VITE_API_URL ?? (import.meta.env.DEV ? "http://localhost:8020" : "");

// Direct JSON-RPC shim — standalone (not embedded in an MCP host).
function createStandaloneShim(): ToolCaller {
  let nextId = 1;
  let inited: Promise<void> | null = null;
  const post = (body: unknown) =>
    fetch(`${API_BASE}/mcp`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json, text/event-stream",
        ...authHeaders(),
      },
      body: JSON.stringify(body),
    });

  // A 401 from the auth sidecar → prompt for a token and reset the session so
  // the next call re-initializes with the new one.
  function guard(res: Response, hadToken: boolean) {
    if (res.status === 401) {
      inited = null;
      const err = authErrorFor(401, hadToken, "mcp");
      requestAuth({ reason: "rejected", message: err.message, target: "mcp" });
      throw err;
    }
  }

  async function ensureSession() {
    inited ??= (async () => {
      const hadToken = !!getToken();
      const res = await post({
        jsonrpc: "2.0",
        id: nextId++,
        method: "initialize",
        params: {
          protocolVersion: "2025-03-26",
          clientInfo: { name: "tailor-web", version: "1.0.0" },
          capabilities: {},
        },
      });
      guard(res, hadToken);
      await parseMcp(res);
    })().catch((e) => {
      inited = null;
      throw e;
    });
    return inited;
  }

  return {
    callServerTool: async (params) => {
      await ensureSession();
      const hadToken = !!getToken();
      const res = await post({
        jsonrpc: "2.0",
        id: nextId++,
        method: "tools/call",
        params: { name: params.name, arguments: params.arguments },
      });
      guard(res, hadToken);
      const data = await parseMcp(res);
      if (data.error) throw new Error(data.error.message ?? "tool call failed");
      return data.result;
    },
  };
}

export const REST_MODE = typeof window !== "undefined" && (window as any).__TAILOR_REST__ === true;
/** Engine REST mode (or its dev fixture) — writes may need the engine's API token. */
export const ENGINE_AUTH_UI =
  REST_MODE ||
  (import.meta.env.DEV &&
    typeof location !== "undefined" &&
    new URLSearchParams(location.search).get("fixtures") === "auth");
export const isIframe = typeof window !== "undefined" && window.parent !== window;

function McpAppInner({ children }: { children: React.ReactNode }) {
  const { app } = useApp({ appInfo: { name: "k8s-tailor", version: "0.1.0" }, capabilities: {} });
  const [fallback, setFallback] = useState<ToolCaller | null>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    // Dev only: `?fixtures` serves mock data (see dev-fixtures/). Stripped from builds.
    if (import.meta.env.DEV) {
      const variant = new URLSearchParams(location.search).get("fixtures");
      if (variant !== null) {
        import("./dev-fixtures/fixtures.ts").then((m) => setFallback(m.createFixtureShim(variant)));
        return;
      }
    }
    if (REST_MODE) {
      setFallback(createRestShim(API_BASE));
      return;
    } // engine serves us standalone
    if (isIframe) return; // in-host: CSP blocks HTTP; wait for the host app
    if (app) {
      if (timer.current) clearTimeout(timer.current);
      setFallback(null);
      return;
    }
    timer.current = setTimeout(() => setFallback(createStandaloneShim()), 500);
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, [app]);

  // In REST mode (engine-served), always use the REST shim, never an MCP host app.
  const fixtures = import.meta.env.DEV && new URLSearchParams(location.search).has("fixtures");
  return (
    <McpAppContext.Provider value={REST_MODE || fixtures ? fallback : (app ?? fallback)}>
      {children}
    </McpAppContext.Provider>
  );
}

export function McpProvider({ children }: { children: React.ReactNode }) {
  return <McpAppInner>{children}</McpAppInner>;
}

export function useMcpApp(): ToolCaller | null {
  return useContext(McpAppContext);
}
