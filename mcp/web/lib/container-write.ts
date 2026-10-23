import { AuthError, authErrorFor } from "./auth";
import { classOf, containerAnnotation, isInjectedSidecar, isNeverSized } from "./containers";
import { AUTOMATED_MODES, type Rec } from "./types";

// Per-container writes (POST /api/workload with `container`): which controls apply, the
// request body, the optimistic cache edit and a human reading of every engine refusal.
// Pure functions and one tiny store, no React (see use of these in lib/mutations.ts).

export const CONTAINER_MODES = [
  "inherit",
  "Off",
  "Recommend",
  "Webhook",
  "InPlace",
  "Both",
] as const;
export type ContainerMode = (typeof CONTAINER_MODES)[number];

export const MODE_LABEL: Record<ContainerMode, string> = {
  inherit: "Inherit",
  Off: "Off",
  Recommend: "Recommend",
  Webhook: "Webhook",
  InPlace: "InPlace",
  Both: "Both",
};

/** Modes that make the engine change the container (the rest only observe or stop). */
export const ACTUATING_MODES: readonly ContainerMode[] = ["Webhook", "InPlace", "Both"];

export type ContainerRef = { namespace: string; kind: string; workload: string; container: string };
export type ContainerWrite = ContainerRef & { mode?: ContainerMode; exclude?: boolean };

export const containerRefOf = (r: ContainerRef): ContainerRef => ({
  namespace: r.namespace,
  kind: r.kind,
  workload: r.workload,
  container: r.container,
});
export const containerKey = (r: ContainerRef) =>
  `${r.namespace}/${r.kind}/${r.workload}/${r.container}`;

/** The engine writes per-container annotations on these kinds only. */
const UNSUPPORTED_KINDS = new Set(["ksvc", "group"]);

export type Availability =
  | { enabled: true }
  | { enabled: false; reason: "never-resized" | "unsupported-kind"; message: string };

/** Whether the mode / exclude controls apply to this container, and why not when they don't. */
export function controlsAvailability(r: Pick<Rec, "containerClass" | "kind">): Availability {
  if (isNeverSized(r as Rec)) {
    return {
      enabled: false,
      reason: "never-resized",
      message: `${classOf(r) === "init" ? "Init" : "Ephemeral"} containers are never resized, so there is no mode to set.`,
    };
  }
  if (UNSUPPORTED_KINDS.has(r.kind)) {
    return {
      enabled: false,
      reason: "unsupported-kind",
      message: `Per-container writes are not supported for this kind (${r.kind}). Use a policy rule, or copy the annotation.`,
    };
  }
  return { enabled: true };
}

/** What the mode select shows: an explicit container annotation, otherwise "inherit". */
export function currentChoice(r: Pick<Rec, "containerPolicy" | "mode">): ContainerMode {
  if (r.containerPolicy !== "annotation") return "inherit";
  return (CONTAINER_MODES as readonly string[]).includes(r.mode) && r.mode !== "inherit"
    ? (r.mode as ContainerMode)
    : "inherit";
}

/**
 * Choosing an actuating mode on an injected sidecar that is not actuated yet is the
 * opt-in (the engine caps it at Recommend until a rule or annotation sets a mode).
 */
export const needsOptInConfirm = (r: Rec, choice: ContainerMode) =>
  isInjectedSidecar(r) && ACTUATING_MODES.includes(choice) && !r.automated;

export const OPT_IN_WARNING = "Setting a mode opts this sidecar in to actuation";

/** Equivalent `kubectl annotate` entries for a write, for the GitOps copy button. */
export function writeEntries(w: Pick<ContainerWrite, "mode" | "exclude">): Record<string, string> {
  const e: Record<string, string> = {};
  if (w.mode && w.mode !== "inherit") e.mode = w.mode;
  if (w.exclude) e.exclude = "true";
  return e;
}
export const annotationFor = (container: string, field: "mode" | "exclude") =>
  containerAnnotation(container, field);

/** Optimistic edit of one container's row, mirroring what the engine reflects at once. */
export function applyContainerWrite(r: Rec, w: ContainerWrite): Rec {
  const next = { ...r };
  if (w.mode !== undefined) {
    if (w.mode === "inherit") {
      next.containerPolicy = "policy"; // the inherited mode arrives with the next refresh
    } else {
      next.mode = w.mode;
      next.automated = AUTOMATED_MODES.includes(w.mode);
      next.containerPolicy = "annotation";
      next.overridden = true;
    }
  }
  if (w.exclude !== undefined) next.excluded = w.exclude;
  return next;
}

// --- exclusion memory ---------------------------------------------------------------
// The engine does not list excluded containers (docs/DESIGN.md, backend gaps), so the
// toggle remembers what this session wrote. Reactive through subscribe().
const excluded = new Map<string, boolean>();
const subs = new Set<() => void>();
let version = 0;
export const subscribeExcluded = (fn: () => void) => {
  subs.add(fn);
  return () => subs.delete(fn);
};
export const excludedVersion = () => version;
export function rememberExcluded(r: ContainerRef, v: boolean | undefined) {
  if (v === undefined) excluded.delete(containerKey(r));
  else excluded.set(containerKey(r), v);
  version++;
  for (const fn of subs) fn();
}
export const recallExcluded = (r: ContainerRef): boolean | undefined =>
  excluded.get(containerKey(r));
export const isExcluded = (r: Rec): boolean => recallExcluded(r) ?? r.excluded ?? false;

// --- errors ---------------------------------------------------------------------------

/** The engine refused a /api/workload write with a status and (usually) a stable code. */
export class WorkloadApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "WorkloadApiError";
  }
}

const HUMAN: Record<string, string> = {
  bad_request: "The engine could not read this request. Reload the dashboard and try again.",
  invalid_mode: "That mode is not valid. Pick Inherit, Off, Recommend, Webhook, InPlace or Both.",
  nothing_to_change: "Nothing to change: pick a mode or switch Exclude.",
  unsupported_kind: "Per-container writes are not supported for this kind.",
  container_not_resizable:
    "This container is never resized (init and ephemeral containers), so it has no mode to set.",
  workload_not_found:
    "The engine does not see this workload any more. It may have been deleted, or the engine has not refreshed yet. Reload and try again.",
  container_not_found:
    "The engine has not seen this container yet. It refreshes at most once a minute: try again shortly.",
};

const BY_STATUS: Record<number, string> = {
  403: "The engine refused this request as cross-site. Open the dashboard from the engine's own address.",
  413: "The request was too large for the engine.",
  415: "The engine refused the request's content type.",
  429: "Too many changes in a short time. Wait a moment and try again.",
  503: "The engine is not accepting changes right now.",
};

export type WriteFailure = {
  kind: "auth" | "api" | "network" | "unknown";
  code: string;
  status: number;
  /** Human sentence for the Banner. */
  message: string;
  /** Raw engine text, when it adds something. */
  detail?: string;
};

const ENGINE_PREFIX = /\[engine:([a-z_]+):(\d{3})\]\s*([\s\S]*)$/;

function fromCode(status: number, code: string, detail: string): WriteFailure {
  const message = HUMAN[code] ?? BY_STATUS[status] ?? `The engine refused the change (${status}).`;
  return {
    kind: "api",
    code,
    status,
    message,
    detail: detail && detail !== message ? detail : undefined,
  };
}

/** Maps anything a container write can throw to a human failure (REST error or MCP text). */
export function classifyWriteError(err: unknown): WriteFailure {
  if (err instanceof AuthError)
    return { kind: "auth", code: "unauthorized", status: err.status, message: err.message };
  if (err instanceof WorkloadApiError) return fromCode(err.status, err.code, err.message);
  const text = err instanceof Error ? err.message : String(err);
  const m = ENGINE_PREFIX.exec(text);
  if (m) return fromCode(Number(m[2]), m[1], m[3].trim());
  if (/->\s*401\b|\b401\b.*token/i.test(text)) {
    const a = authErrorFor(401, false, "engine");
    return { kind: "auth", code: "unauthorized", status: 401, message: a.message };
  }
  if (/TAILOR_API_AUTH_REQUIRED|->\s*503/.test(text)) {
    const a = authErrorFor(503, false, "engine");
    return { kind: "auth", code: "writes_disabled", status: 503, message: a.message };
  }
  if (
    err instanceof TypeError ||
    /failed to fetch|networkerror|unreachable|ECONNREFUSED/i.test(text)
  )
    return {
      kind: "network",
      code: "network",
      status: 0,
      message: "Could not reach the engine. Check the connection and try again.",
    };
  return {
    kind: "unknown",
    code: "unknown",
    status: 0,
    message: "The change was not saved.",
    detail: text,
  };
}

/** Spoken/visible confirmation of a saved write. */
export function successText(w: ContainerWrite): string {
  const parts: string[] = [];
  if (w.mode !== undefined)
    parts.push(
      w.mode === "inherit" ? `mode set to inherit (policy decides)` : `mode set to ${w.mode}`,
    );
  if (w.exclude !== undefined) parts.push(w.exclude ? "excluded" : "no longer excluded");
  return `Saved: ${w.container} ${parts.join(", ")}.`;
}
