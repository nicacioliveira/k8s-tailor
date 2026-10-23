// Access-token handling for the standalone dashboard.
//
// Two places can ask for a bearer token:
//  - the Go engine (:8080, REST transport): every mutating /api/* call needs
//    `Authorization: Bearer <TAILOR_API_TOKEN>` when the engine has one; reads are open
//    unless the engine runs with TAILOR_API_PROTECT_READS=true (then they need it too);
//  - the MCP server's /mcp endpoint (standalone JSON-RPC transport): every call needs
//    MCP_API_TOKEN when set.
// Inside an MCP host the host proxies auth, so none of this runs there.
//
// Storage: sessionStorage by default (gone when the tab closes); localStorage only when
// the user ticks "remember on this device". A link may carry the token in the URL
// FRAGMENT (`#token=<t>` or `#/<view>?token=<t>`): a fragment is never sent to the
// server, so it can't land in an access log, proxy log or Referer. It is accepted
// once, stored for the session and stripped from the address bar. `?token=` (query
// string) is NOT accepted any more — it reaches every proxy log on the way — and is
// only stripped from the address bar.

const KEY = "tailor_token";

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">;
const safe = (get: () => Store | undefined): Store | undefined => {
  try {
    return get();
  } catch {
    return undefined; // storage disabled (sandboxed iframe, privacy mode)
  }
};
const session = () => safe(() => globalThis.sessionStorage);
const local = () => safe(() => globalThis.localStorage);

export function getToken(): string {
  try {
    return session()?.getItem(KEY) || local()?.getItem(KEY) || "";
  } catch {
    return "";
  }
}

export function isRemembered(): boolean {
  try {
    return !!local()?.getItem(KEY);
  } catch {
    return false;
  }
}

// Subscribers to "the stored token changed" (header indicator).
const tokenListeners = new Set<() => void>();
export function onTokenChange(fn: () => void): () => void {
  tokenListeners.add(fn);
  return () => tokenListeners.delete(fn);
}
const notify = () => {
  for (const fn of tokenListeners) fn();
};

function removeStored(): void {
  try {
    session()?.removeItem(KEY);
    local()?.removeItem(KEY);
  } catch {
    /* ignore */
  }
}

export function setToken(token: string, remember = false): void {
  const t = token.trim();
  removeStored();
  try {
    if (t) (remember ? local() : session())?.setItem(KEY, t);
  } catch {
    /* storage unavailable: the token simply won't persist */
  }
  notify();
}

export function clearToken(): void {
  removeStored();
  notify();
}

/**
 * Accept a token from the URL fragment once (`#token=<t>` or `#/<view>?token=<t>`),
 * store it for this session and remove it from the address bar. A `?token=` query
 * parameter is ignored (it was already sent to the server) and removed as well.
 */
export function consumeUrlToken(loc: Location | undefined = globalThis.location): void {
  if (!loc) return;
  const url = new URL(loc.href);
  let changed = false;
  if (url.searchParams.has("token")) {
    url.searchParams.delete("token");
    changed = true;
  }
  const hash = url.hash.replace(/^#/, "");
  let token = "";
  if (hash.startsWith("token=")) {
    token = new URLSearchParams(hash).get("token") ?? "";
    url.hash = "";
    changed = true;
  } else {
    const i = hash.indexOf("?");
    if (i >= 0) {
      const params = new URLSearchParams(hash.slice(i + 1));
      if (params.has("token")) {
        token = params.get("token") ?? "";
        params.delete("token");
        const qs = params.toString();
        url.hash = `${hash.slice(0, i)}${qs ? `?${qs}` : ""}`;
        changed = true;
      }
    }
  }
  if (token) setToken(token, false);
  if (!changed) return;
  try {
    globalThis.history?.replaceState(null, "", url.toString());
  } catch {
    /* ignore */
  }
}

export function authHeaders(): Record<string, string> {
  const t = getToken();
  return t ? { Authorization: `Bearer ${t}` } : {};
}

export type AuthTarget = "engine" | "mcp";

/** The server refused the call: missing/wrong token (401) or writes/reads disabled (503). */
export class AuthError extends Error {
  readonly status: number;
  /** True when entering a token can fix it (false = server-side misconfiguration). */
  readonly fixable: boolean;
  /** The refused call was a read (GET): the engine runs with TAILOR_API_PROTECT_READS. */
  readonly read: boolean;
  constructor(status: number, message: string, fixable: boolean, read = false) {
    super(message);
    this.name = "AuthError";
    this.status = status;
    this.fixable = fixable;
    this.read = read;
  }
}

/**
 * Human message for an auth failure — what happened and what to do. `read` = the refused
 * call was a read (GET): only an engine with TAILOR_API_PROTECT_READS refuses those.
 */
export function authErrorFor(
  status: number,
  hadToken: boolean,
  target: AuthTarget,
  read = false,
): AuthError {
  if (status === 503) {
    return new AuthError(
      503,
      read
        ? "Reading is disabled: the engine protects reads (TAILOR_API_PROTECT_READS) but has no API token configured. Set TAILOR_API_TOKEN on the engine."
        : "Changes are disabled: the engine requires an API token (TAILOR_API_AUTH_REQUIRED) but none is configured. Set TAILOR_API_TOKEN on the engine.",
      false,
      read,
    );
  }
  const what =
    target === "engine" ? "API token (TAILOR_API_TOKEN)" : "access token (MCP_API_TOKEN)";
  return new AuthError(
    401,
    hadToken
      ? `The saved ${what} was rejected. Enter the current token to continue.`
      : target === "engine"
        ? read
          ? `This engine protects its data: reading needs the engine's ${what}. Enter it to continue.`
          : `Making changes needs the engine's ${what}. Enter it to continue — reading stays open.`
        : `This K8s Tailor instance is protected. Enter the ${what} to continue.`,
    true,
    read,
  );
}

// --- "auth required" signal -------------------------------------------------------
// The transport raises it on a fixable 401; the UI (login screen / token dialog)
// listens. Also raised by the header's "Access token" button (reason: "manual").

export type AuthRequest = {
  reason: "rejected" | "manual";
  message?: string;
  target: AuthTarget;
  /** The refused call was a read (engine with TAILOR_API_PROTECT_READS). */
  read?: boolean;
};
type Listener = (r: AuthRequest) => void;
const listeners = new Set<Listener>();

export function onAuthRequired(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function requestAuth(r: AuthRequest): void {
  for (const fn of listeners) fn(r);
}
