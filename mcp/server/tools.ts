import { z } from "zod";
import { engine } from "./engine.ts";

const WORKLOAD_MODES = ["Off", "Recommend", "Webhook", "InPlace", "Both", "", "inherit"] as const;

/**
 * TAILOR_SET_WORKLOAD input. Backwards compatible: the original workload-level call
 * ({namespace, kind, workload, mode?, profile?}) is unchanged. Container-level writes need
 * mode and/or exclude, and never a profile (the engine answers 400 for it).
 */
export const setWorkloadSchema = z
  .object({
    namespace: z.string().min(1),
    kind: z.string().min(1).describe("Deployment | StatefulSet | DaemonSet | Pod"),
    workload: z.string().min(1),
    container: z
      .string()
      .min(1)
      .optional()
      .describe("Container name: switches to a per-container write"),
    mode: z.enum(WORKLOAD_MODES).optional(),
    profile: z.string().optional().describe("TailorProfile name; '' clears. Workload-level only"),
    exclude: z
      .boolean()
      .optional()
      .describe("Container-level only: true excludes the container, false removes the exclusion"),
  })
  .superRefine((a, ctx) => {
    if (a.container === undefined) {
      if (a.exclude !== undefined)
        ctx.addIssue({ code: "custom", path: ["exclude"], message: "exclude requires container" });
      return;
    }
    if (a.profile !== undefined)
      ctx.addIssue({
        code: "custom",
        path: ["profile"],
        message: "profile is workload-level; omit it when container is set",
      });
    if (a.mode === undefined && a.exclude === undefined)
      ctx.addIssue({
        code: "custom",
        path: ["container"],
        message: "container requires mode and/or exclude",
      });
  });

/** A minimal MCP tool definition. */
export interface McpTool {
  name: string;
  description: string;
  readOnly?: boolean;
  inputSchema: z.ZodTypeAny;
  execute: (args: any) => Promise<unknown>;
}

const workloadRef = z.object({
  namespace: z.string().describe("Kubernetes namespace"),
  kind: z.string().describe("Workload kind: Deployment | StatefulSet | DaemonSet"),
  workload: z.string().describe("Workload name"),
  container: z.string().describe("Container name"),
});

const resSpec = z
  .object({
    quantile: z.number().optional(),
    margin: z.number().optional(),
    min: z.string().optional(),
    max: z.string().optional(),
    limitFactor: z.number().optional(),
  })
  .describe("per-container sizing override (same shape as a TailorProfile cpu/memory block)");

/** TailorPolicy spec.containers[]: first matching rule wins (docs/CONTAINERS.md). */
const containerRule = z.object({
  match: z.object({
    names: z.array(z.string()).optional().describe("container-name globs (empty = any)"),
    classes: z
      .array(z.enum(["app", "injected-sidecar", "native-sidecar"]))
      .optional()
      .describe("container classes (empty = any)"),
  }),
  exclude: z.boolean().optional().describe("no recommendation and no actuation"),
  mode: z.enum(["Off", "Recommend", "Webhook", "InPlace", "Both"]).optional(),
  apply: z.enum(["InPlace", "Rollout", "InPlacePreferred"]).optional(),
  direction: z.enum(["Both", "Down", "Up"]).optional(),
  profileRef: z.string().optional(),
  proxyAnnotations: z
    .boolean()
    .optional()
    .describe(
      "durable path for a mesh proxy: write the mesh's resource annotations on the pod template",
    ),
  cpu: resSpec.optional(),
  memory: resSpec.optional(),
});

export const tools: McpTool[] = [
  {
    name: "TAILOR_OVERVIEW",
    description:
      "Cluster rightsizing overview: weekly cost, wasted %, active savings, automated %, over/under-provisioned counts, and aggregate CPU/mem usage-vs-reserved trend. Optional window/step select the metric time range.",
    readOnly: true,
    inputSchema: z.object({
      window: z.string().optional().describe("relative time range, e.g. 1h, 3h, 24h, 7d"),
      step: z.number().optional().describe("resolution in seconds"),
      from: z.number().optional().describe("absolute range start (unix seconds)"),
      to: z.number().optional().describe("absolute range end (unix seconds)"),
    }),
    execute: (a) => engine.overview(a),
  },
  {
    name: "TAILOR_LIST_RECOMMENDATIONS",
    description:
      "List rightsizing recommendations for every in-scope workload container, ranked by weekly $ waste. Includes current vs recommended requests/limits and the percentile/decay/peak signals behind each.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.recommendations(),
  },
  {
    name: "TAILOR_GET_DETAIL",
    description:
      "Drill into one workload container: recommendation, CPU/memory usage trend samples, p90/p95 signals, predict_linear projection, and the mutations already applied to it. Optional window/step select the chart time range.",
    readOnly: true,
    inputSchema: workloadRef.extend({
      window: z.string().optional(),
      step: z.number().optional(),
      from: z.number().optional(),
      to: z.number().optional(),
    }),
    execute: (a) => engine.detail(a.namespace, a.kind, a.workload, a.container, a),
  },
  {
    name: "TAILOR_LIST_ACTUATIONS",
    description:
      "Decisions the tailor applied (in-place resize, controller rollout, ksvc, webhook) with from→to CPU/memory, read from the durable decision log (ClickHouse). Optional time range (window or from/to), free-text filter q, and limit for pagination.",
    readOnly: true,
    inputSchema: z.object({
      window: z.string().optional(),
      from: z.number().optional(),
      to: z.number().optional(),
      q: z.string().optional().describe("free-text filter (workload, namespace, mode)"),
      limit: z.number().optional(),
    }),
    execute: (a) => engine.actuations(a),
  },
  {
    name: "TAILOR_PREVIEW_PATCH",
    description:
      "Show the dry-run patch (strategic-merge + in-place resize hint) the tailor would apply for a workload container. Read-only — never mutates the cluster.",
    readOnly: true,
    inputSchema: workloadRef,
    execute: (a) => engine.patch(a.namespace, a.kind, a.workload, a.container),
  },
  {
    name: "TAILOR_ECONOMY",
    description:
      "Cluster savings ($/week) trend over the recent window plus the active pricing basis (static | opencost | aws) and $/core-hr, $/GB-hr.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.economy(),
  },
  {
    name: "TAILOR_HEALTH",
    description:
      "Engine health (GET /healthz): metrics source, actuation posture (active | dry-run | paused), the VictoriaMetrics URL and whether it answers.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.health(),
  },
  {
    name: "TAILOR_ADVISOR_STATUS",
    description:
      "Optional LLM advisor status: whether it is enabled, the configured model, circuit-breaker state (closed | open | half-open) and call/cache/token counters. Check this before TAILOR_ADVISOR_REVIEW / TAILOR_ADVISOR_SUMMARY.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.advisorStatus(),
  },
  {
    name: "TAILOR_ADVISOR_REVIEW",
    description:
      "Ask the LLM advisor for a second opinion on ONE workload container's current recommendation: verdict (ok | review | risky), confidence, rationale, risks, suggested action and the figures it cites. ADVISORY ONLY — it never applies or changes anything, and the model output is untrusted text. Consumes LLM budget (rate/token limits apply); results are cached. Omit container only when the workload has a single container.",
    readOnly: true,
    inputSchema: z.object({
      namespace: z.string().describe("Kubernetes namespace"),
      kind: z.string().describe("Workload kind: Deployment | StatefulSet | DaemonSet"),
      workload: z.string().describe("Workload name"),
      container: z
        .string()
        .optional()
        .describe("Container name (required when the workload has more than one)"),
    }),
    execute: (a) => engine.advisorReview(a),
  },
  {
    name: "TAILOR_ADVISOR_SUMMARY",
    description:
      "Ask the LLM advisor to summarize the engine's current decisions (top-N workloads by absolute weekly $ waste, optionally one namespace): overview, verdict, highlights and workloads needing attention, with cited figures. ADVISORY ONLY — never applies anything; output is untrusted text. Consumes LLM budget; results are cached.",
    readOnly: true,
    inputSchema: z.object({
      namespace: z.string().optional().describe("limit to one namespace (default: all)"),
      limit: z
        .number()
        .int()
        .min(0)
        .max(100)
        .optional()
        .describe("how many top workloads to send (default 20, max 100)"),
    }),
    execute: (a) => engine.advisorSummary(a),
  },
  {
    name: "TAILOR_SET_PAUSED",
    description:
      "Global kill switch. When paused, no actuation happens anywhere (webhook + in-place). Writes TailorConfig.spec.paused in Kubernetes (etcd) — no database.",
    readOnly: false,
    inputSchema: z.object({
      paused: z.boolean().describe("true to pause all actuation, false to resume"),
    }),
    execute: (a) => engine.setPaused(a.paused),
  },
  {
    name: "TAILOR_LIST_POLICIES",
    description:
      "List rightsizing groups (TailorPolicy CRs): selector (namespaces/labels), mode, headroom margins, min/max guardrails, priority, whether scheduled.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.policies(),
  },
  {
    name: "TAILOR_SET_POLICY",
    description:
      "Create or update a rightsizing group. Scope it to namespaces/labels (e.g. protect a critical database group), set the mode, the headroom margin (how far above measured usage the request sits), and min/max guardrails. Persists as a TailorPolicy CR in Kubernetes.",
    readOnly: false,
    inputSchema: z.object({
      name: z.string().describe("group name (CR name, DNS-1123)"),
      mode: z.enum(["Off", "Recommend", "Webhook", "InPlace", "Both"]).optional(),
      apply: z
        .enum(["InPlace", "Rollout", "InPlacePreferred"])
        .optional()
        .describe("actuation method"),
      priority: z.number().int().optional().describe("higher wins when groups overlap"),
      namespaces: z.array(z.string()).optional(),
      matchLabels: z.record(z.string(), z.string()).optional(),
      profileRef: z.string().optional().describe("reuse a TailorProfile as base"),
      cpuMargin: z
        .number()
        .optional()
        .describe("CPU headroom fraction, e.g. 0.15 = 15% above usage"),
      memMargin: z.number().optional().describe("memory headroom fraction"),
      cpuMin: z.string().optional(),
      cpuMax: z.string().optional(),
      memMin: z.string().optional(),
      memMax: z.string().optional(),
      cpuLimitFactor: z
        .number()
        .optional()
        .describe("CPU limit = request * factor (0 = no CPU limit)"),
      memLimitFactor: z.number().optional().describe("memory limit = request * factor"),
      halfLife: z
        .string()
        .optional()
        .describe("decay half-life, e.g. 24h, 3d — shorter reacts faster"),
      season: z
        .enum(["daily", "weekly", "both", "off"])
        .optional()
        .describe("seasonal forecast basis"),
      allowExceedLimit: z
        .boolean()
        .optional()
        .describe(
          "allow growing requests/limits above the current limit (default false — keeps workloads healthy/bounded)",
        ),
      direction: z.enum(["Both", "Down", "Up"]).optional().describe("shrink-only / grow-only"),
      oomOverridesDirection: z.boolean().optional(),
      containers: z
        .array(containerRule)
        .optional()
        .describe(
          "per-container rules (first match wins). OMIT to keep the rules already on the CR; an empty array removes them all.",
        ),
      schedule: z
        .object({
          timezone: z.string().optional(),
          outsideMode: z.enum(["Off", "Recommend", "Webhook", "InPlace", "Both"]).optional(),
          windows: z
            .array(
              z.object({
                days: z.array(z.string()).optional(),
                start: z.string().optional(),
                end: z.string().optional(),
                from: z
                  .string()
                  .optional()
                  .describe("YYYY-MM-DD campaign start (e.g. a seasonal event)"),
                until: z.string().optional().describe("YYYY-MM-DD campaign end"),
              }),
            )
            .optional(),
        })
        .optional()
        .describe("actuation windows / campaigns; outside them the mode falls back to outsideMode"),
    }),
    execute: (a) => engine.setPolicy(a),
  },
  {
    name: "TAILOR_DELETE_POLICY",
    description: "Delete a rightsizing group (TailorPolicy CR) by name.",
    readOnly: false,
    inputSchema: z.object({ name: z.string() }),
    execute: (a) => engine.deletePolicy(a.name),
  },
  {
    name: "TAILOR_LIST_GROUPS",
    description:
      "List collapse rules (TailorGroup CRs): how workloads are folded into a single logical unit — by a shared label, by Knative service, or by namespace (groupBy: namespace, folds a whole namespace like per-env StatefulSets). A group only collapses; it never actuates.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.groups(),
  },
  {
    name: "TAILOR_SET_GROUP",
    description:
      "Create or update a collapse rule (TailorGroup CR). Folds many workloads into one unit so they share one aggregate profile. Use groupBy: namespace to collapse a whole namespace (e.g. all per-env StatefulSets in a site namespace, which share no common label). This changes how units are grouped/recommended — it does NOT actuate (that's a TailorPolicy).",
    readOnly: false,
    inputSchema: z.object({
      name: z.string().describe("group name (CR name, DNS-1123)"),
      groupBy: z
        .string()
        .describe(
          'collapse key: a pod label, or the sentinel "namespace" to fold a whole namespace',
        ),
      namespaces: z
        .array(z.string())
        .optional()
        .describe('namespace globs this rule applies to (empty = all), e.g. ["apps-*"]'),
      actuation: z
        .enum(["direct", "ksvc", "webhook"])
        .optional()
        .describe(
          "method for the collapsed unit; default direct (in-place). ksvc = Knative Service patch",
        ),
      cohortBy: z
        .string()
        .optional()
        .describe("generation label (Knative revision, pod-template-hash) to select a cohort"),
      cohortSelect: z.enum(["all", "latest", "activeByTraffic"]).optional(),
    }),
    execute: (a) => engine.setGroup(a),
  },
  {
    name: "TAILOR_DELETE_GROUP",
    description: "Delete a collapse rule (TailorGroup CR) by name.",
    readOnly: false,
    inputSchema: z.object({ name: z.string() }),
    execute: (a) => engine.deleteGroup(a.name),
  },
  {
    name: "TAILOR_GROUP_MEMBERS",
    description:
      "Drill-down: list the raw workloads a collapse rule folds into its unit(s) — namespace, kind, name, replicas, and the unit each belongs to. Answers 'what is inside this group?'.",
    readOnly: true,
    inputSchema: z.object({ name: z.string().describe("the TailorGroup name") }),
    execute: (a) => engine.groupMembers(a.name),
  },
  {
    name: "TAILOR_LIST_PROFILES",
    description:
      "List reusable scaling profiles (TailorProfile names) — the options for a workload's Policy dropdown.",
    readOnly: true,
    inputSchema: z.object({}),
    execute: () => engine.profiles(),
  },
  {
    name: "TAILOR_SET_WORKLOAD",
    description:
      "Write a per-workload or per-container override as annotations (takes precedence over group policies). Without `container`: sets the workload mode (tailor.k8s-tailor.dev/mode) and/or profile (/profile); returns {ok:true}. With `container`: sets tailor.k8s-tailor.dev/container.<name>.mode and/or .exclude and returns {ok, namespace, kind, workload, container, mode?, exclude?}. mode: Off|Recommend|Webhook|InPlace|Both; '' or 'inherit' removes the override. exclude: true writes .exclude=\"true\", false removes it. `profile` is workload-level only; a container call needs `mode` and/or `exclude`. Explicit mode on an injected sidecar (istio-proxy…) IS the opt-in to actuation. Not supported for ksvc/group kinds (per-container) nor for init/ephemeral containers. Containers come from the engine's last refresh (<=60s old). Engine refusals surface as `[engine:<code>:<status>] message`.",
    readOnly: false,
    inputSchema: setWorkloadSchema,
    execute: (a) => engine.setWorkload(a),
  },
];

/** Tool whose host view is the embedded dashboard (linked via _meta ui/resourceUri). */
export const DASHBOARD_TOOL_ID = "TAILOR_OVERVIEW";
