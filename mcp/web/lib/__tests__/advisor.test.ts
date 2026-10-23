import { beforeEach, describe, expect, test } from "bun:test";
import type { ProblemKind } from "../advisor";
import { MemStorage } from "./fixtures";

Object.assign(globalThis, { sessionStorage: new MemStorage(), localStorage: new MemStorage() });

const adv = await import("../advisor");
const auth = await import("../auth");
const { createRestShim, route } = await import("../rest-transport");

beforeEach(() => {
  sessionStorage.clear();
  localStorage.clear();
});

type Call = { url: string; init?: RequestInit };
const fakeFetch =
  (status: number, body: string, calls: Call[] = [], headers: Record<string, string> = {}) =>
  async (url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return new Response(body, { status, headers });
  };

describe("advisor tool routing", () => {
  test("status is a GET; review/summary are POSTs with only the needed fields", () => {
    expect(route("TAILOR_ADVISOR_STATUS", {})).toEqual(["GET", "/api/advisor/status"]);
    expect(
      route("TAILOR_ADVISOR_REVIEW", {
        namespace: "portal",
        kind: "Deployment",
        workload: "api",
        container: "",
        extra: "dropped",
      }),
    ).toEqual([
      "POST",
      "/api/advisor/review",
      { namespace: "portal", kind: "Deployment", workload: "api" },
    ]);
    expect(
      route("TAILOR_ADVISOR_REVIEW", {
        namespace: "a",
        kind: "K",
        workload: "w",
        container: "c",
      })[2],
    ).toEqual({ namespace: "a", kind: "K", workload: "w", container: "c" });
    expect(route("TAILOR_ADVISOR_SUMMARY", {})[2]).toEqual({});
    expect(route("TAILOR_ADVISOR_SUMMARY", { namespace: "portal", limit: 5 })[2]).toEqual({
      namespace: "portal",
      limit: 5,
    });
  });

  test("the POST carries the bearer token", async () => {
    auth.setToken("tok");
    const calls: Call[] = [];
    const shim = createRestShim("", fakeFetch(200, '{"review":{}}', calls));
    await shim.callServerTool({
      name: "TAILOR_ADVISOR_REVIEW",
      arguments: { namespace: "a", kind: "K", workload: "w", container: "c" },
    });
    expect(calls[0].url).toBe("/api/advisor/review");
    expect(calls[0].init?.method).toBe("POST");
    expect((calls[0].init?.headers as Record<string, string>).Authorization).toBe("Bearer tok");
  });
});

async function failWith(status: number, body: string, headers: Record<string, string> = {}) {
  const shim = createRestShim("", fakeFetch(status, body, [], headers));
  return shim
    .callServerTool({
      name: "TAILOR_ADVISOR_SUMMARY",
      arguments: {},
    })
    .catch((e: Error) => e);
}

describe("REST transport keeps advisor errors structured", () => {
  test("429 rate_limited with Retry-After", async () => {
    const err = await failWith(
      429,
      '{"error":"advisor hourly call limit reached","code":"rate_limited"}',
      { "Retry-After": "60" },
    );
    expect(err).toBeInstanceOf(adv.AdvisorApiError);
    const p = adv.mapAdvisorError(err);
    expect(p.kind).toBe("rate_limited");
    expect(p.retryAfter).toBe(60);
    expect(p.description).toContain("1 min");
    expect(p.variant).toBe("warning");
  });

  test("503 circuit_open is an advisor error, not an auth problem", async () => {
    const seen: unknown[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const err = await failWith(503, '{"error":"circuit breaker open","code":"circuit_open"}', {
      "Retry-After": "30",
    });
    off();
    const p = adv.mapAdvisorError(err);
    expect(p.kind).toBe("circuit_open");
    expect(p.retryAfter).toBe(30);
    expect(seen).toHaveLength(0);
  });

  test("503 auth-required (no code) stays a non-fixable AuthError", async () => {
    const err = await failWith(503, '{"error":"TAILOR_API_AUTH_REQUIRED is set but token empty"}');
    expect(err).toBeInstanceOf(auth.AuthError);
    expect(adv.mapAdvisorError(err).kind).toBe("writes_disabled");
  });

  test("a refused read (protected reads) is not reported as a locked advisor", async () => {
    const err503 = auth.authErrorFor(503, false, "engine", true);
    const p503 = adv.mapAdvisorError(err503);
    expect(p503.kind).toBe("reads_protected");
    expect(p503.description).toContain("TAILOR_API_PROTECT_READS");
    expect(p503.description).not.toContain("TAILOR_API_AUTH_REQUIRED");
    const p401 = adv.mapAdvisorError(auth.authErrorFor(401, false, "engine", true));
    expect(p401.kind).toBe("reads_protected");
    expect(p401.description).toContain("reading needs");
    // a refused write keeps its own wording
    expect(adv.mapAdvisorError(auth.authErrorFor(503, false, "engine")).kind).toBe(
      "writes_disabled",
    );
  });

  test("401 asks for the token and maps to unauthorized", async () => {
    const seen: unknown[] = [];
    const off = auth.onAuthRequired((r) => seen.push(r));
    const err = await failWith(401, '{"error":"unauthorized"}');
    off();
    expect(adv.mapAdvisorError(err).kind).toBe("unauthorized");
    expect(seen).toHaveLength(1);
  });

  test("404 disabled, 502 invalid_response", async () => {
    expect(
      adv.mapAdvisorError(await failWith(404, '{"error":"advisor is disabled","code":"disabled"}')),
    ).toMatchObject({ kind: "disabled", variant: "info", retryable: false });
    expect(
      adv.mapAdvisorError(await failWith(502, '{"error":"x","code":"invalid_response"}')),
    ).toMatchObject({ kind: "invalid_response", variant: "danger", retryable: true });
  });
});

describe("mapAdvisorError", () => {
  test("covers every documented engine code", () => {
    const cases: [number, string, ProblemKind][] = [
      [404, "disabled", "disabled"],
      [404, "not_found", "not_found"],
      [404, "no_workloads", "unknown"],
      [400, "invalid_input", "invalid_input"],
      [400, "container_required", "container_required"],
      [429, "rate_limited", "rate_limited"],
      [429, "budget_exceeded", "budget_exceeded"],
      [503, "circuit_open", "circuit_open"],
      [502, "invalid_response", "invalid_response"],
      [502, "timeout", "timeout"],
      [502, "upstream_error", "upstream_error"],
    ];
    for (const [status, code, kind] of cases) {
      expect(adv.mapAdvisorError(new adv.AdvisorApiError(status, code, "m")).kind).toBe(kind);
    }
  });

  test("falls back to the HTTP status when the code is unknown", () => {
    expect(adv.mapAdvisorError(new adv.AdvisorApiError(429, "", "m")).kind).toBe("rate_limited");
    expect(adv.mapAdvisorError(new adv.AdvisorApiError(503, "weird", "m")).kind).toBe(
      "circuit_open",
    );
    expect(adv.mapAdvisorError(new adv.AdvisorApiError(418, "", "teapot")).kind).toBe("unknown");
  });

  test("rate limit without a header defaults to 60 s; budget has its own copy", () => {
    const p = adv.mapAdvisorError(new adv.AdvisorApiError(429, "budget_exceeded", "m"));
    expect(p.retryAfter).toBe(60);
    expect(p.title).toContain("token budget");
  });

  test("parses the MCP tool-error text format", () => {
    const p = adv.mapAdvisorError(
      new Error(
        "Error: [advisor:rate_limited:429] advisor hourly call limit reached [retry-after:42]",
      ),
    );
    expect(p.kind).toBe("rate_limited");
    expect(p.retryAfter).toBe(42);
    const q = adv.mapAdvisorError(new Error("[advisor:circuit_open:503] breaker open"));
    expect(q.kind).toBe("circuit_open");
    expect(q.retryAfter).toBe(60);
    expect(adv.mapAdvisorError(new Error("[advisor:unauthorized:401] nope")).kind).toBe(
      "unauthorized",
    );
  });

  test("anything else is an unknown, loud error that keeps the text", () => {
    const p = adv.mapAdvisorError(new Error("socket hang up"));
    expect(p).toMatchObject({ kind: "unknown", variant: "danger", description: "socket hang up" });
  });

  test("Retry-After accepts only delta-seconds", () => {
    expect(adv.parseRetryAfter("60")).toBe(60);
    expect(adv.parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT")).toBeUndefined();
    expect(adv.parseRetryAfter("0")).toBeUndefined();
    expect(adv.parseRetryAfter(null)).toBeUndefined();
    expect(adv.formatRetry(42)).toBe("42 s");
    expect(adv.formatRetry(60)).toBe("1 min");
    expect(adv.formatRetry(120)).toBe("2 min");
  });
});

describe("response parsing (LLM output is untrusted)", () => {
  test("maps the wire shape and keeps markup as literal text", () => {
    const r = adv.parseReview({
      namespace: "portal",
      workload: "api",
      container: "api",
      review: {
        verdict: "risky",
        confidence: "high",
        rationale: "<img src=x onerror=alert(1)> **bold**",
        risks: ["<script>x</script>", ""],
        suggested_action: "wait",
        cited_figures: [{ label: "CPU", value: 250, unit: "m" }],
        model: "m",
        cached: true,
      },
    });
    expect(r.review.rationale).toBe("<img src=x onerror=alert(1)> **bold**");
    expect(r.review.risks).toEqual(["<script>x</script>"]);
    expect(r.review.figures).toEqual([{ label: "CPU", value: 250, unit: "m" }]);
    expect(r.review.cached).toBe(true);
    expect(r.review.suggestedAction).toBe("wait");
  });

  test("unknown verdict/confidence degrade cautiously, never to ok", () => {
    const r = adv.parseReview({ review: { verdict: "totally-fine", confidence: "certain" } });
    expect(r.review.verdict).toBe("review");
    expect(r.review.confidence).toBe("low");
    expect(r.review.risks).toEqual([]);
    expect(adv.parseReview(null).review.verdict).toBe("review");
  });

  test("summary and status", () => {
    const s = adv.parseSummary({
      considered: 20,
      total: 31,
      summary: {
        overview: "o",
        verdict: "ok",
        confidence: "medium",
        highlights: ["a"],
        attention: [{ workload: "portal/api", reason: "r" }, null],
        cited_figures: [],
        omitted: 11,
      },
    });
    expect(s).toMatchObject({ considered: 20, total: 31 });
    expect(s.summary.attention).toHaveLength(2);
    expect(s.summary.omitted).toBe(11);
    expect(adv.parseStatus({ enabled: true, model: "m", breaker_state: "open" })).toEqual({
      enabled: true,
      model: "m",
      breakerState: "open",
    });
    expect(adv.parseStatus({}).enabled).toBe(false);
  });
});
