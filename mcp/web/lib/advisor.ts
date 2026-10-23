// Client side of the optional LLM advisor (engine: internal/server/advisor.go).
//
// SECURITY: everything the model wrote is untrusted. The parsers below only coerce
// shapes (strings stay strings, no markup is interpreted) and the UI renders every
// field as a plain React text node — never innerHTML / markdown.

import { AuthError } from "./auth";

export type Verdict = "ok" | "review" | "risky";
export type Confidence = "low" | "medium" | "high";

export interface Figure {
  label: string;
  value: number | string;
  unit: string;
}

export interface Review {
  verdict: Verdict;
  confidence: Confidence;
  rationale: string;
  risks: string[];
  suggestedAction: string;
  figures: Figure[];
  model: string;
  cached: boolean;
}

export interface Summary {
  overview: string;
  verdict: Verdict;
  confidence: Confidence;
  highlights: string[];
  attention: { workload: string; reason: string }[];
  figures: Figure[];
  omitted: number;
  model: string;
  cached: boolean;
}

export interface ReviewResult {
  namespace: string;
  workload: string;
  container: string;
  review: Review;
}

export interface SummaryResult {
  namespace: string;
  considered: number;
  total: number;
  summary: Summary;
}

export interface AdvisorStatus {
  enabled: boolean;
  model: string;
  breakerState: string;
}

/** Shown permanently next to anything the advisor produced. */
export const ADVISOR_DISCLAIMER = "AI-generated — advisory only, never applies changes";

// --- response parsing -------------------------------------------------------------

const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : v == null ? "" : String(v));
const strs = (v: unknown): string[] => (Array.isArray(v) ? v.map(str).filter(Boolean) : []);

/** Unknown verdicts degrade to the cautious "review", never to "ok". */
export function asVerdict(v: unknown): Verdict {
  return v === "ok" || v === "risky" || v === "review" ? v : "review";
}
export function asConfidence(v: unknown): Confidence {
  return v === "high" || v === "medium" || v === "low" ? v : "low";
}

function figures(v: unknown): Figure[] {
  if (!Array.isArray(v)) return [];
  return v.map((f) => {
    const o = rec(f);
    return {
      label: str(o.label),
      value: typeof o.value === "number" || typeof o.value === "string" ? o.value : str(o.value),
      unit: str(o.unit),
    };
  });
}

export function parseReview(raw: unknown): ReviewResult {
  const o = rec(raw);
  const r = rec(o.review);
  return {
    namespace: str(o.namespace),
    workload: str(o.workload),
    container: str(o.container),
    review: {
      verdict: asVerdict(r.verdict),
      confidence: asConfidence(r.confidence),
      rationale: str(r.rationale),
      risks: strs(r.risks),
      suggestedAction: str(r.suggested_action),
      figures: figures(r.cited_figures),
      model: str(r.model),
      cached: r.cached === true,
    },
  };
}

export function parseSummary(raw: unknown): SummaryResult {
  const o = rec(raw);
  const s = rec(o.summary);
  return {
    namespace: str(o.namespace),
    considered: Number(o.considered) || 0,
    total: Number(o.total) || 0,
    summary: {
      overview: str(s.overview),
      verdict: asVerdict(s.verdict),
      confidence: asConfidence(s.confidence),
      highlights: strs(s.highlights),
      attention: Array.isArray(s.attention)
        ? s.attention.map((a) => ({
            workload: str(rec(a).workload),
            reason: str(rec(a).reason),
          }))
        : [],
      figures: figures(s.cited_figures),
      omitted: Number(s.omitted) || 0,
      model: str(s.model),
      cached: s.cached === true,
    },
  };
}

export function parseStatus(raw: unknown): AdvisorStatus {
  const o = rec(raw);
  return {
    enabled: o.enabled === true,
    model: str(o.model),
    breakerState: str(o.breaker_state),
  };
}

// --- errors -----------------------------------------------------------------------

/** HTTP error from an /api/advisor/* call (engine REST transport). */
export class AdvisorApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    /** Seconds from the Retry-After header, when the engine sent one. */
    readonly retryAfter?: number,
  ) {
    super(message);
    this.name = "AdvisorApiError";
  }
}

/** Retry-After as seconds. HTTP-dates and garbage yield undefined. */
export function parseRetryAfter(h: string | null | undefined): number | undefined {
  if (!h || !/^\d+$/.test(h.trim())) return undefined;
  const n = Number.parseInt(h.trim(), 10);
  return n > 0 ? n : undefined;
}

export type ProblemKind =
  | "disabled"
  | "not_found"
  | "container_required"
  | "invalid_input"
  | "unauthorized"
  | "writes_disabled"
  | "reads_protected"
  | "rate_limited"
  | "budget_exceeded"
  | "circuit_open"
  | "invalid_response"
  | "timeout"
  | "upstream_error"
  | "unknown";

export interface AdvisorProblem {
  kind: ProblemKind;
  /** Banner variant (UI kit): how loud it should be. */
  variant: "info" | "warning" | "danger";
  title: string;
  description: string;
  /** Seconds to wait before retrying (429 / 503 breaker). */
  retryAfter?: number;
  /** Retrying the same request later can succeed. */
  retryable: boolean;
}

const KNOWN: ProblemKind[] = [
  "disabled",
  "not_found",
  "container_required",
  "invalid_input",
  "unauthorized",
  "writes_disabled",
  "reads_protected",
  "rate_limited",
  "budget_exceeded",
  "circuit_open",
  "invalid_response",
  "timeout",
  "upstream_error",
];

const DEFAULT_RETRY = 60;

/** `[advisor:<code>:<status>] msg [retry-after:N]` — what the MCP server puts in tool errors. */
const MCP_ERR = /\[advisor:([a-z_]+)(?::(\d{3}))?\]\s*([\s\S]*?)(?:\s*\[retry-after:(\d+)\])?\s*$/;

function kindFor(code: string | undefined, status: number | undefined): ProblemKind {
  if (code && (KNOWN as string[]).includes(code)) return code as ProblemKind;
  switch (status) {
    case 401:
      return "unauthorized";
    case 429:
      return "rate_limited";
    case 503:
      return "circuit_open";
    case 504:
      return "timeout";
    case 502:
      return "upstream_error";
    default:
      return "unknown";
  }
}

export function formatRetry(sec: number): string {
  if (sec < 60) return `${Math.max(1, Math.round(sec))} s`;
  const m = Math.round(sec / 60);
  return `${m} min`;
}

function build(kind: ProblemKind, detail: string, retryAfter?: number): AdvisorProblem {
  const wait = (retryAfter ?? DEFAULT_RETRY) > 0 ? (retryAfter ?? DEFAULT_RETRY) : DEFAULT_RETRY;
  switch (kind) {
    case "disabled":
      return {
        kind,
        variant: "info",
        title: "AI advisor is turned off",
        description:
          "The engine has no LLM advisor configured. Nothing was sent to a model; enable it on the engine to use this.",
        retryable: false,
      };
    case "not_found":
      return {
        kind,
        variant: "warning",
        title: "Nothing to analyze",
        description: detail || "That workload is no longer reported by the engine.",
        retryable: false,
      };
    case "container_required":
      return {
        kind,
        variant: "warning",
        title: "Choose a container",
        description: "This workload has more than one container; pick the one to analyze.",
        retryable: false,
      };
    case "invalid_input":
      return {
        kind,
        variant: "warning",
        title: "The request was not accepted",
        description: detail || "The engine rejected the request as invalid.",
        retryable: false,
      };
    case "unauthorized":
      return {
        kind,
        variant: "warning",
        title: "API token needed",
        description:
          "Running the advisor spends LLM budget, so the engine requires its API token (TAILOR_API_TOKEN). Save the token, then try again.",
        retryable: true,
      };
    case "writes_disabled":
      return {
        kind,
        variant: "danger",
        title: "The advisor is locked",
        description:
          "The engine requires an API token (TAILOR_API_AUTH_REQUIRED) but none is configured. Set TAILOR_API_TOKEN on the engine.",
        retryable: false,
      };
    case "reads_protected":
      return {
        kind,
        variant: "warning",
        title: "The engine protects its reads",
        description:
          detail ||
          "The engine requires its API token for reads (TAILOR_API_PROTECT_READS). Enter the token, or set TAILOR_API_TOKEN on the MCP server.",
        retryable: true,
      };
    case "rate_limited":
      return {
        kind,
        variant: "warning",
        title: "Hourly AI call limit reached",
        description: `Too many advisor calls this hour. Try again in about ${formatRetry(wait)}.`,
        retryAfter: wait,
        retryable: true,
      };
    case "budget_exceeded":
      return {
        kind,
        variant: "warning",
        title: "Hourly AI token budget used up",
        description: `The advisor's token budget for this hour is exhausted. Try again in about ${formatRetry(wait)}.`,
        retryAfter: wait,
        retryable: true,
      };
    case "circuit_open":
      return {
        kind,
        variant: "warning",
        title: "AI provider temporarily unavailable",
        description: `The circuit breaker is open after repeated provider failures, so no request was sent. It retries on its own; try again in about ${formatRetry(wait)}.`,
        retryAfter: wait,
        retryable: true,
      };
    case "invalid_response":
      return {
        kind,
        variant: "danger",
        title: "The model's answer was discarded",
        description:
          "It did not pass validation (malformed, or it cited numbers that are not in the data), so nothing is shown. Trying again may work.",
        retryable: true,
      };
    case "timeout":
      return {
        kind,
        variant: "danger",
        title: "The AI provider timed out",
        description: "No answer arrived in time. Try again in a moment.",
        retryable: true,
      };
    case "upstream_error":
      return {
        kind,
        variant: "danger",
        title: "The AI provider returned an error",
        description: "The provider failed or is unreachable from the engine. Try again later.",
        retryable: true,
      };
    default:
      return {
        kind: "unknown",
        variant: "danger",
        title: "Could not run the AI advisor",
        description: detail || "Unexpected error.",
        retryable: true,
      };
  }
}

/**
 * Map any failure from an advisor call (REST AdvisorApiError / AuthError, or the
 * `[advisor:code:status]` text of an MCP tool error) to something the UI can show.
 * Server text is only used where it is the engine's own message (not LLM output).
 */
export function mapAdvisorError(err: unknown): AdvisorProblem {
  if (err instanceof AdvisorApiError) {
    return build(kindFor(err.code, err.status), err.message, err.retryAfter);
  }
  if (err instanceof AuthError) {
    if (err.read) return build("reads_protected", err.message);
    return build(err.fixable ? "unauthorized" : "writes_disabled", err.message);
  }
  const text = err instanceof Error ? err.message : String(err ?? "");
  const m = MCP_ERR.exec(text);
  if (m) {
    const status = m[2] ? Number(m[2]) : undefined;
    return build(kindFor(m[1], status), m[3].trim(), m[4] ? Number.parseInt(m[4], 10) : undefined);
  }
  return build("unknown", text);
}
