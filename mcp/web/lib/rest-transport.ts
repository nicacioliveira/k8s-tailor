import type { ToolCaller } from "../context.tsx";
import { AdvisorApiError, parseRetryAfter } from "./advisor";
import { authErrorFor, authHeaders, getToken, requestAuth } from "./auth";
import { WorkloadApiError } from "./container-write";

// REST transport — used when the Go engine serves this same dashboard standalone
// (it injects window.__TAILOR_REST__). Maps each MCP tool to the engine's HTTP API,
// so the ONE dashboard runs unchanged against either the MCP server or the engine.

const enc = encodeURIComponent;

function qs(a: Record<string, unknown>, keys: string[]): string {
  const p = new URLSearchParams();
  for (const k of keys) if (a?.[k] != null && a[k] !== "") p.set(k, String(a[k]));
  const s = p.toString();
  return s ? `?${s}` : "";
}

const RANGE = ["window", "step", "from", "to"];

/** Tool name + args -> [method, path, body]. Exported for tests. */
export function route(name: string, a: Record<string, unknown>): [string, string, unknown?] {
  const ref = () =>
    `${enc(String(a.namespace))}/${enc(String(a.kind))}/${enc(String(a.workload))}/${enc(String(a.container))}`;
  switch (name) {
    case "TAILOR_OVERVIEW":
      return ["GET", `/api/overview${qs(a, RANGE)}`];
    case "TAILOR_LIST_RECOMMENDATIONS":
      return ["GET", "/api/recommendations"];
    case "TAILOR_GET_DETAIL":
      return ["GET", `/api/detail/${ref()}${qs(a, RANGE)}`];
    case "TAILOR_LIST_ACTUATIONS":
      return ["GET", `/api/actuations${qs(a, ["window", "from", "to", "q", "limit"])}`];
    case "TAILOR_PREVIEW_PATCH":
      return ["GET", `/api/patch/${ref()}`];
    case "TAILOR_ECONOMY":
      return ["GET", "/api/economy"];
    case "TAILOR_HEALTH":
      return ["GET", "/healthz"];
    case "TAILOR_SET_PAUSED":
      return ["POST", "/api/pause", { paused: a.paused }];
    case "TAILOR_LIST_POLICIES":
      return ["GET", "/api/policies"];
    case "TAILOR_SET_POLICY":
      return ["POST", "/api/policies", a];
    case "TAILOR_DELETE_POLICY":
      return ["DELETE", `/api/policies/${enc(String(a.name))}`];
    case "TAILOR_LIST_GROUPS":
      return ["GET", "/api/groups"];
    case "TAILOR_SET_GROUP":
      return ["POST", "/api/groups", a];
    case "TAILOR_DELETE_GROUP":
      return ["DELETE", `/api/groups/${enc(String(a.name))}`];
    case "TAILOR_GROUP_MEMBERS":
      return ["GET", `/api/groups/${enc(String(a.name))}/members`];
    case "TAILOR_LIST_PROFILES":
      return ["GET", "/api/profiles"];
    case "TAILOR_SET_WORKLOAD":
      return ["POST", "/api/workload", a];
    case "TAILOR_ADVISOR_STATUS":
      return ["GET", "/api/advisor/status"];
    case "TAILOR_ADVISOR_REVIEW":
      return [
        "POST",
        "/api/advisor/review",
        {
          namespace: a.namespace,
          kind: a.kind,
          workload: a.workload,
          ...(a.container ? { container: a.container } : {}),
        },
      ];
    case "TAILOR_ADVISOR_SUMMARY":
      return [
        "POST",
        "/api/advisor/summary",
        {
          ...(a.namespace ? { namespace: a.namespace } : {}),
          ...(typeof a.limit === "number" ? { limit: a.limit } : {}),
        },
      ];
    default:
      throw new Error(`unknown tool: ${name}`);
  }
}

/** The engine answers errors as `{"error": "..."}`; surface that text, not just the code. */
async function errorBody(res: Response): Promise<{ text: string; code: string }> {
  const text = await res.text().catch(() => "");
  try {
    const j = JSON.parse(text);
    if (j && typeof j.error === "string")
      return { text: j.error, code: typeof j.code === "string" ? j.code : "" };
  } catch {
    /* not JSON */
  }
  return { text: text.trim() || res.statusText, code: "" };
}
const errorText = async (res: Response) => (await errorBody(res)).text;

type Fetch = (input: string, init?: RequestInit) => Promise<Response>;

export function createRestShim(
  base = "",
  doFetch: Fetch = (i, init) => fetch(i, init),
): ToolCaller {
  const req = async (method: string, path: string, body?: unknown) => {
    const hadToken = !!getToken();
    const res = await doFetch(`${base}${path}`, {
      method,
      headers: { "Content-Type": "application/json", ...authHeaders() },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    // Advisor failures carry a stable `code` (+ Retry-After): keep them structured so the
    // UI can say exactly what happened. 401 and the auth-required 503 stay AuthErrors.
    if (!res.ok && path.startsWith("/api/advisor/") && res.status !== 401) {
      const { text, code } = await errorBody(res.clone());
      if (code || res.status !== 503)
        throw new AdvisorApiError(
          res.status,
          code,
          text,
          parseRetryAfter(res.headers.get("Retry-After")),
        );
    }
    // 401 on any call (reads too, when the engine runs with TAILOR_API_PROTECT_READS),
    // or a 503 whose body names the auth setting (writes or reads disabled for lack of
    // a token on the engine). Any other 503 is an ordinary upstream error.
    if (res.status === 401 || res.status === 503) {
      const msg = await errorText(res);
      if (res.status === 401 || msg.includes("TAILOR_API")) {
        const read = method === "GET";
        const err = authErrorFor(res.status, hadToken, "engine", read);
        if (err.fixable)
          requestAuth({
            reason: "rejected",
            message: err.message,
            target: "engine",
            ...(read ? { read } : {}),
          });
        throw err;
      }
      throw new Error(`${path}: ${res.status} ${msg}`);
    }
    // Workload / container writes answer {error, code}: keep the code for the UI.
    if (!res.ok && path === "/api/workload") {
      const { text, code } = await errorBody(res);
      throw new WorkloadApiError(res.status, code, text);
    }
    if (!res.ok) throw new Error(`${path}: ${res.status} ${await errorText(res)}`);
    const text = await res.text();
    return text ? JSON.parse(text) : {};
  };
  return {
    callServerTool: async (params) => {
      const [method, path, body] = route(
        params.name,
        (params.arguments ?? {}) as Record<string, unknown>,
      );
      const out = await req(method, path, body);
      return { structuredContent: out, content: [] } as never;
    },
  };
}
