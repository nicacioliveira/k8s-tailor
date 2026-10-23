// Dev-only mock transport: serves every TAILOR_* tool from deterministic fixtures so the
// dashboard can be built and reviewed without a cluster. Enabled by `?fixtures` in dev
// (`?fixtures=empty` for the empty states, `?fixtures=error` for the error states,
// `?fixtures=auth` for an engine whose writes need the token "demo"; the AI advisor is ON by
// default, `?fixtures=advisor-off` turns it off and `advisor-429|503|502|401` make every
// advisor call fail with that error). POST /api/workload follows the engine contract
// (docs/CONFIGURATION.md): codes invalid_mode, container_not_found, container_not_resizable,
// unsupported_kind, nothing_to_change, bad_request, workload_not_found; container
// `payments/audit-agent` is never found (a stale snapshot), and `?fixtures=auth` answers 401.
// Never shipped: context.tsx only imports this behind `import.meta.env.DEV`.
import type { ToolCaller } from "../context.tsx";
import { AdvisorApiError } from "../lib/advisor";
import { authErrorFor, getToken, requestAuth } from "../lib/auth";
import { CONTAINER_MODES, WorkloadApiError } from "../lib/container-write";
import type {
  Decision,
  GroupRuleView,
  ListResp,
  MemberRef,
  OverviewResp,
  PolicyView,
  Rec,
  Sample,
} from "../lib/types";

const MI = 1024 * 1024;
const now = () => Math.floor(Date.now() / 1000);

// Small deterministic PRNG so screenshots are stable between reloads.
function rng(seed: number) {
  let s = seed >>> 0;
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0;
    return s / 2 ** 32;
  };
}

const WINDOWS: Record<string, number> = {
  "5m": 300,
  "10m": 600,
  "15m": 900,
  "30m": 1800,
  "1h": 3600,
  "3h": 10800,
  "12h": 43200,
  "24h": 86400,
  "7d": 604800,
};

function span(a: Record<string, unknown>): { from: number; to: number; step: number } {
  if (typeof a.from === "number" && typeof a.to === "number") {
    return { from: a.from, to: a.to, step: Number(a.step) || 60 };
  }
  const w = WINDOWS[String(a.window ?? "30m")] ?? 1800;
  const to = now();
  const step = Math.max(Number(a.step) || 30, Math.ceil(w / 240));
  return { from: to - w, to, step };
}

/** A usage curve: base + daily-ish wave + noise, in the unit given. */
function curve(seed: number, base: number, amp: number, a: Record<string, unknown>): Sample[] {
  const { from, to, step } = span(a);
  const r = rng(seed);
  const out: Sample[] = [];
  for (let t = from - (from % step); t <= to; t += step) {
    const wave =
      Math.sin((t / 3600) * 0.9 + seed) * 0.5 + Math.sin((t / 600) * 1.3 + seed * 2) * 0.25;
    out.push({ T: t, V: Math.max(base * 0.15, base + amp * wave + amp * 0.35 * (r() - 0.5)) });
  }
  return out;
}

const flat = (v: number, a: Record<string, unknown>): Sample[] => {
  const { from, to } = span(a);
  return [
    { T: from, V: v },
    { T: to, V: v },
  ];
};

type Seed = {
  ns: string;
  name: string;
  kind: string;
  container?: string;
  cpu: [number, number, number]; // current, applied, recommended (cores)
  mem: [number, number, number]; // MiB
  limits?: { cpu?: number; mem: number; recCpu?: number; recMem: number };
  waste: number;
  extra?: Partial<Rec>;
};

const SEEDS: Seed[] = [
  {
    ns: "portal",
    name: "gateway",
    kind: "Deployment",
    cpu: [1, 1, 0.32],
    mem: [1024, 1024, 420],
    waste: 6.84,
    extra: {
      automated: true,
      mode: "Both",
      apply: "InPlace",
      policy: "apps",
      replicas: 6,
      autoscaler: {
        kind: "HPA",
        name: "gateway",
        managesCpu: true,
        managesMem: false,
        min: 2,
        max: 12,
      },
      cpuHeldByHpa: true,
    },
  },
  {
    ns: "portal",
    name: "sessions",
    kind: "Deployment",
    cpu: [0.25, 0.25, 0.24],
    mem: [256, 256, 248],
    waste: 0.01,
    extra: { automated: true, mode: "InPlace", policy: "apps", replicas: 4 },
  },
  {
    ns: "portal",
    name: "frontend",
    kind: "Deployment",
    cpu: [0.5, 0.3, 0.28],
    mem: [512, 384, 360],
    waste: 2.11,
    extra: {
      automated: true,
      mode: "Both",
      overridden: true,
      direction: "Down",
      specDrift: true,
      policy: "apps",
      replicas: 3,
    },
  },
  {
    ns: "data",
    name: "postgres",
    kind: "StatefulSet",
    cpu: [2, 2, 1.1],
    mem: [4096, 4096, 4480],
    waste: 3.9,
    limits: { mem: 4096 * MI, recMem: 4096 * MI },
    extra: {
      mode: "Recommend",
      policy: "databases",
      limitCapped: true,
      pdbProtected: true,
      pdbName: "postgres-pdb",
      pdbAllowed: 0,
      strategy: "OnDelete",
      replicas: 3,
    },
  },
  {
    ns: "data",
    name: "redis",
    kind: "StatefulSet",
    cpu: [0.1, 0.1, 0.38],
    mem: [128, 128, 310],
    waste: -1.42,
    extra: {
      mode: "Recommend",
      policy: "databases",
      oomRisk: true,
      pdbProtected: true,
      pdbName: "redis",
      pdbAllowed: 1,
      replicas: 3,
    },
  },
  {
    ns: "monitoring",
    name: "prometheus",
    kind: "StatefulSet",
    cpu: [2, 2, 0.74],
    mem: [8192, 8192, 3900],
    waste: 11.6,
    extra: { mode: "Recommend", policy: "default", replicas: 2 },
  },
  {
    ns: "kube-system",
    name: "coredns",
    kind: "Deployment",
    cpu: [0.1, 0.1, 0.1],
    mem: [70, 70, 64],
    waste: 0.0,
    extra: { automated: true, mode: "InPlace", policy: "system", replicas: 2 },
  },
  {
    ns: "serving",
    name: "api",
    kind: "ksvc",
    cpu: [0.5, 0.5, 0.21],
    mem: [512, 512, 300],
    waste: 1.73,
    extra: {
      automated: true,
      mode: "Both",
      apply: "Rollout",
      policy: "apps",
      knative: { service: "api", revision: "api-00042" },
      autoscaler: {
        kind: "KPA",
        name: "api",
        managesCpu: false,
        managesMem: false,
        min: 0,
        max: 20,
      },
      replicas: 2,
    },
  },
  {
    ns: "batch",
    name: "etl-worker",
    kind: "Deployment",
    cpu: [1, 1, 0.9],
    mem: [2048, 2048, 1900],
    waste: 0.3,
    extra: { confident: false, mode: "Recommend", policy: "default", replicas: 1 },
  },
  {
    ns: "ingress",
    name: "ingress-nginx",
    kind: "DaemonSet",
    cpu: [0.5, 0.5, 0.18],
    mem: [512, 512, 210],
    waste: 4.4,
    extra: { automated: true, mode: "Both", policy: "system", replicas: 5 },
  },
  // gateway: app + injected istio-proxy (native, Recommend-only) + native sidecar + init.
  {
    ns: "portal",
    name: "gateway",
    kind: "Deployment",
    container: "istio-proxy",
    cpu: [0.2, 0.2, 0.08],
    mem: [256, 256, 110],
    waste: 0.9,
    extra: {
      mode: "Recommend",
      policy: "apps",
      replicas: 6,
      containerClass: "injected-sidecar",
      nativeSidecar: true,
      inTemplate: false,
      mesh: "istio",
      containerPolicy: "policy:apps containers[1]",
      proxyAnnotations: true,
      actuationPath: "none",
    },
  },
  {
    ns: "portal",
    name: "gateway",
    kind: "Deployment",
    container: "fluent-bit",
    cpu: [0.1, 0.1, 0.04],
    mem: [128, 128, 72],
    waste: 0.35,
    extra: {
      automated: true,
      mode: "Both",
      policy: "apps",
      replicas: 6,
      containerClass: "native-sidecar",
      nativeSidecar: true,
      inTemplate: true,
      containerPolicy: "policy",
      actuationPath: "webhook+in-place",
    },
  },
  {
    ns: "portal",
    name: "gateway",
    kind: "Deployment",
    container: "istio-init",
    cpu: [0, 0, 0],
    mem: [0, 0, 0],
    waste: 0,
    // The engine does not list init containers today; kept so the UI stays correct if it does.
    extra: {
      mode: "Off",
      policy: "apps",
      replicas: 6,
      containerClass: "init",
      inTemplate: false,
      mesh: "istio",
      containerPolicy: "policy",
      actuationPath: "none",
    },
  },
  // sessions: istio-proxy opted in by annotation; durable path through the mesh annotations.
  {
    ns: "portal",
    name: "sessions",
    kind: "Deployment",
    container: "istio-proxy",
    cpu: [0.1, 0.1, 0.06],
    mem: [192, 192, 120],
    waste: 0.22,
    extra: {
      automated: true,
      mode: "InPlace",
      apply: "InPlacePreferred",
      overridden: true,
      policy: "apps",
      replicas: 4,
      containerClass: "injected-sidecar",
      nativeSidecar: false,
      inTemplate: false,
      mesh: "istio",
      containerPolicy: "annotation",
      proxyAnnotations: true,
      actuationPath: "in-place+fallback:proxy-annotations:istio",
    },
  },
  // payments: linkerd proxy at the safe default.
  {
    ns: "pay",
    name: "payments",
    kind: "Deployment",
    cpu: [0.5, 0.5, 0.31],
    mem: [512, 512, 380],
    waste: 1.2,
    extra: { automated: true, mode: "InPlace", policy: "default", replicas: 3 },
  },
  {
    ns: "pay",
    name: "payments",
    kind: "Deployment",
    container: "linkerd-proxy",
    cpu: [0.1, 0.1, 0.03],
    mem: [64, 64, 38],
    waste: 0.18,
    extra: {
      mode: "Recommend",
      policy: "default",
      replicas: 3,
      containerClass: "injected-sidecar",
      nativeSidecar: false,
      inTemplate: false,
      mesh: "linkerd",
      containerPolicy: "default:injected-sidecar",
      proxyAnnotations: false,
      actuationPath: "none",
    },
  },
  {
    ns: "pay",
    name: "payments",
    kind: "Deployment",
    container: "audit-agent",
    cpu: [0.05, 0.05, 0.02],
    mem: [64, 64, 40],
    waste: 0.06,
    // Seen by the UI but gone from the engine's last refresh: a write answers container_not_found.
    extra: {
      mode: "Recommend",
      policy: "default",
      replicas: 3,
      containerClass: "injected-sidecar",
      nativeSidecar: false,
      inTemplate: false,
      containerPolicy: "default:injected-sidecar",
      actuationPath: "none",
    },
  },
  // Per-resource confidence: CPU confident / memory still gathering, and the reverse
  // (etl-worker above is neither; every other seed has both).
  {
    ns: "data",
    name: "ingest-gateway",
    kind: "Deployment",
    cpu: [0.8, 0.8, 0.35],
    mem: [1024, 1024, 700],
    waste: 1.1,
    extra: {
      mode: "Recommend",
      policy: "databases",
      replicas: 2,
      cpuConfident: true,
      memConfident: false,
    },
  },
  {
    ns: "data",
    name: "report-builder",
    kind: "StatefulSet",
    cpu: [0.6, 0.6, 0.5],
    mem: [2048, 2048, 900],
    waste: 0.9,
    extra: {
      mode: "Recommend",
      policy: "databases",
      replicas: 1,
      cpuConfident: false,
      memConfident: true,
    },
  },
  {
    ns: "portal",
    name: "search-indexer",
    kind: "Deployment",
    container: "indexer",
    cpu: [0.2, 0.2, 0.62],
    mem: [256, 256, 512],
    waste: -2.05,
    extra: { mode: "Recommend", policy: "apps", direction: "Up", replicas: 2 },
  },
  {
    ns: "default",
    name: "debug-shell",
    kind: "Pod",
    cpu: [0.1, 0.1, 0.01],
    mem: [128, 128, 16],
    waste: 0.38,
    extra: { managed: false, mode: "Off", policy: "default", replicas: 1 },
  },
];

function buildRec(s: Seed): Rec {
  const lim = s.limits ?? { mem: s.mem[0] * 2 * MI, recMem: s.mem[2] * 2 * MI };
  const base: Rec = {
    namespace: s.ns,
    workload: s.name,
    kind: s.kind,
    container: s.container ?? s.name,
    replicas: 1,
    current: {
      cpuRequest: s.cpu[0],
      memRequest: s.mem[0] * MI,
      memLimit: lim.mem,
      cpuLimit: lim.cpu,
    },
    applied: {
      cpuRequest: s.cpu[1],
      memRequest: s.mem[1] * MI,
      memLimit: lim.mem,
      cpuLimit: lim.cpu,
    },
    recommended: {
      cpuRequest: s.cpu[2],
      memRequest: s.mem[2] * MI,
      memLimit: lim.recMem,
      cpuLimit: lim.recCpu,
    },
    specDrift: false,
    signals: {
      cpuQuantile: s.cpu[2] / 1.15,
      cpuPeak: s.cpu[2] * 1.4,
      cpuForecast: s.cpu[2] * 1.05,
      memQuantile: (s.mem[2] / 1.15) * MI,
      memPeak: s.mem[2] * 1.1 * MI,
      memForecastBytes: s.mem[2] * 1.02 * MI,
      memTrendBytes: s.mem[2] * 0.98 * MI,
      samples: 2880,
      headroomBreachCpu: s.waste < 0 ? 0.12 : 0.004,
      headroomBreachMem: s.waste < 0 ? 0.08 : 0.0,
    },
    mode: "Recommend",
    apply: "InPlace",
    direction: "",
    policy: "default",
    profile: "",
    automated: false,
    overridden: false,
    managed: true,
    strategy: s.kind === "StatefulSet" ? "RollingUpdate (ordered)" : "RollingUpdate 25%/25%",
    pdbName: "",
    pdbProtected: false,
    pdbAllowed: 0,
    autoscaler: { kind: "", name: "", managesCpu: false, managesMem: false, min: 0, max: 0 },
    knative: { service: "", revision: "" },
    cpuHeldByHpa: false,
    memHeldByHpa: false,
    confident: true,
    oomRisk: false,
    limitCapped: false,
    wasteUsdWeek: s.waste,
    containerClass: "app",
    nativeSidecar: false,
    inTemplate: true,
    mesh: "",
    containerPolicy: "policy",
    proxyAnnotations: false,
    actuationPath: "none",
  };
  const merged = { ...base, ...s.extra };
  const partial = s.extra?.cpuConfident !== s.extra?.memConfident && s.extra?.cpuConfident != null;
  return {
    ...merged,
    cpuConfident: s.extra?.cpuConfident ?? merged.confident,
    memConfident: s.extra?.memConfident ?? merged.confident,
    actuationPath: s.extra?.actuationPath ?? (merged.automated ? "in-place" : "none"),
    signals: {
      ...base.signals,
      ...(s.extra?.confident === false ? { samples: 42 } : partial ? { samples: 640 } : {}),
    },
  };
}

function state() {
  const recs = SEEDS.map(buildRec);
  const policies: PolicyView[] = [
    {
      name: "databases",
      priority: 300,
      mode: "Recommend",
      apply: "Rollout",
      profileRef: "",
      namespaces: ["data"],
      matchLabels: { tier: "database" },
      cpuMargin: 0.4,
      memMargin: 0.4,
      cpuMin: "250m",
      cpuMax: "",
      memMin: "512Mi",
      memMax: "",
      cpuLimitFactor: 0,
      memLimitFactor: 1,
      halfLife: "72h",
      season: "weekly",
      allowExceedLimit: false,
      scheduled: false,
      schedule: { timezone: "UTC", outsideMode: "Off", windows: [] },
    },
    {
      name: "apps",
      priority: 200,
      mode: "Both",
      apply: "InPlace",
      profileRef: "web",
      namespaces: ["portal", "serving"],
      matchLabels: {},
      cpuMargin: 0.15,
      memMargin: 0.15,
      cpuMin: "10m",
      cpuMax: "4",
      memMin: "64Mi",
      memMax: "8Gi",
      cpuLimitFactor: 0,
      memLimitFactor: 1.2,
      halfLife: "24h",
      season: "daily",
      allowExceedLimit: false,
      containers: [
        { match: { names: ["vault-agent*"] }, exclude: true },
        {
          match: { classes: ["injected-sidecar"] },
          proxyAnnotations: true,
          cpu: { min: "20m" },
          memory: { min: "96Mi", max: "1Gi" },
        },
      ],
      scheduled: true,
      schedule: {
        timezone: "America/Sao_Paulo",
        outsideMode: "Recommend",
        windows: [{ days: ["Mon", "Tue", "Wed", "Thu", "Fri"], start: "09:00", end: "18:00" }],
      },
    },
    {
      name: "system",
      priority: 100,
      mode: "InPlace",
      apply: "InPlacePreferred",
      profileRef: "",
      namespaces: ["kube-system", "ingress"],
      matchLabels: {},
      cpuMargin: 0.25,
      memMargin: 0.25,
      cpuMin: "",
      cpuMax: "",
      memMin: "",
      memMax: "",
      cpuLimitFactor: 0,
      memLimitFactor: 1,
      halfLife: "",
      season: "both",
      allowExceedLimit: true,
      scheduled: false,
      schedule: { timezone: "UTC", outsideMode: "Off", windows: [] },
    },
  ];
  const groups: GroupRuleView[] = [
    {
      name: "site-envs",
      groupBy: "namespace",
      namespaces: ["site-*"],
      actuation: "direct",
      cohortBy: "",
      cohortSelect: "",
    },
    {
      name: "kn-revisions",
      groupBy: "serving.knative.dev/service",
      namespaces: ["serving"],
      actuation: "ksvc",
      cohortBy: "serving.knative.dev/revision",
      cohortSelect: "latest",
    },
  ];
  const t = now();
  const events: Decision[] = [
    {
      time: t - 95,
      mode: "inplace",
      namespace: "portal",
      kind: "Deployment",
      workload: "sessions",
      container: "sessions",
      fromCpu: 0.3,
      toCpu: 0.25,
      fromMem: 300 * MI,
      toMem: 256 * MI,
    },
    {
      time: t - 760,
      mode: "rollout",
      namespace: "serving",
      kind: "ksvc",
      workload: "api",
      container: "api",
      fromCpu: 0.6,
      toCpu: 0.5,
      fromMem: 640 * MI,
      toMem: 512 * MI,
    },
    {
      time: t - 1500,
      mode: "inplace",
      namespace: "ingress",
      kind: "DaemonSet",
      workload: "ingress-nginx",
      container: "ingress-nginx",
      fromCpu: 0.5,
      toCpu: 0.35,
      fromMem: 512 * MI,
      toMem: 512 * MI,
    },
    {
      time: t - 5200,
      mode: "webhook",
      namespace: "portal",
      kind: "Deployment",
      workload: "frontend",
      container: "frontend",
      fromCpu: 0.5,
      toCpu: 0.3,
      fromMem: 512 * MI,
      toMem: 384 * MI,
    },
    {
      time: t - 9100,
      mode: "inplace",
      namespace: "kube-system",
      kind: "Deployment",
      workload: "coredns",
      container: "coredns",
      fromCpu: 0.1,
      toCpu: 0.12,
      fromMem: 64 * MI,
      toMem: 70 * MI,
    },
  ];
  const counts: ListResp["actuationCounts"] = {};
  for (const e of events) {
    const k = `${e.namespace}/${e.kind}/${e.workload}/${e.container}`;
    counts[k] = {
      count: (counts[k]?.count ?? 0) + 1,
      last: Math.max(counts[k]?.last ?? 0, e.time),
    };
  }
  return { recs, policies, groups, events, counts };
}

export function createFixtureShim(variant: string): ToolCaller {
  const db = state();
  const empty = variant === "empty";
  const fail = variant === "error";
  const auth = variant === "auth";
  let paused = false;
  const WRITES = /^TAILOR_(SET|DELETE)_/;
  const advOff = variant === "advisor-off";
  const advErr = /^advisor-(401|429|502|503)$/.exec(variant)?.[1];
  const reviewed = new Set<string>();
  let summarized = false;
  const advisorFail = (): never => {
    switch (advErr) {
      case "401":
        throw authErrorFor(401, false, "engine");
      case "429":
        throw new AdvisorApiError(429, "rate_limited", "advisor hourly call limit reached", 42);
      case "503":
        throw new AdvisorApiError(
          503,
          "circuit_open",
          "advisor provider temporarily unavailable (circuit breaker open)",
          60,
        );
      default:
        throw new AdvisorApiError(
          502,
          "invalid_response",
          "advisor model returned an invalid response",
        );
    }
  };
  const bad = (status: number, code: string, msg: string): never => {
    throw new WorkloadApiError(status, code, msg);
  };
  const MODES_OK = [...CONTAINER_MODES, ""] as string[];
  // Mirrors internal/server postWorkload + recommender SetWorkload / SetContainer.
  const setWorkload = (a: Record<string, unknown>) => {
    const { namespace, kind, workload, container } = a as Record<string, string | undefined>;
    if (!namespace || !kind || !workload)
      bad(400, "bad_request", "namespace, kind, workload required");
    const mode = typeof a.mode === "string" ? a.mode : undefined;
    const exclude = typeof a.exclude === "boolean" ? a.exclude : undefined;
    const sameWorkload = (r: Rec) =>
      r.namespace === namespace && r.kind === kind && r.workload === workload;
    if (!container) {
      if (exclude !== undefined) bad(400, "bad_request", "exclude requires container");
      if (mode !== undefined && !MODES_OK.includes(mode))
        bad(400, "invalid_mode", `invalid mode "${mode}"`);
      for (const r of db.recs.filter(sameWorkload)) {
        if (typeof a.profile === "string") r.profile = a.profile;
        // Like the engine: the workload toggle never enables an injected sidecar.
        if (mode !== undefined && r.containerClass !== "injected-sidecar") {
          const m = mode === "inherit" ? "" : mode;
          r.overridden = m !== "";
          r.mode = m || "Recommend";
          r.automated = ["Both", "InPlace", "Webhook"].includes(r.mode);
        }
      }
      return { ok: true };
    }
    if (a.profile !== undefined)
      bad(400, "bad_request", "profile is workload-level; omit it when container is set");
    if (mode === undefined && exclude === undefined)
      bad(400, "nothing_to_change", "container requires mode and/or exclude");
    if (mode !== undefined && !MODES_OK.includes(mode))
      bad(400, "invalid_mode", `invalid mode "${mode}"`);
    if (!["Deployment", "StatefulSet", "DaemonSet", "Pod"].includes(kind as string))
      bad(
        400,
        "unsupported_kind",
        `unsupported kind "${kind}": per-container overrides are written on Deployment, StatefulSet, DaemonSet or Pod`,
      );
    const siblings = db.recs.filter(sameWorkload);
    if (!siblings.length)
      bad(
        404,
        "workload_not_found",
        `${kind} ${namespace}/${workload} is not in the last snapshot`,
      );
    const rec = siblings.find((r) => r.container === container);
    if (!rec || (namespace === "pay" && workload === "payments" && container === "audit-agent"))
      bad(404, "container_not_found", `"${container}" in ${kind} ${namespace}/${workload}`);
    if (rec?.containerClass === "init" || rec?.containerClass === "ephemeral")
      bad(400, "container_not_resizable", `"${container}" is ${rec.containerClass}`);
    if (rec) {
      if (mode !== undefined) {
        if (mode === "" || mode === "inherit") {
          rec.containerPolicy = "policy";
          rec.overridden = false;
          if (rec.containerClass === "injected-sidecar") rec.mode = "Recommend";
        } else {
          rec.mode = mode;
          rec.containerPolicy = "annotation";
          rec.overridden = true;
        }
        rec.automated = ["Both", "InPlace", "Webhook"].includes(rec.mode);
        rec.actuationPath = rec.automated ? "in-place" : "none";
      }
      if (exclude !== undefined) {
        rec.excluded = exclude;
        if (exclude) {
          rec.mode = "Off";
          rec.automated = false;
          rec.actuationPath = "none";
        }
      }
    }
    return {
      ok: true,
      namespace,
      kind,
      workload,
      container,
      ...(mode !== undefined ? { mode: mode === "" ? "inherit" : mode } : {}),
      ...(exclude !== undefined ? { exclude } : {}),
    };
  };
  const PRICES = { priceCpuCoreHr: 0.031611, priceGbHr: 0.004237 };

  const handle = (name: string, a: Record<string, unknown>): unknown => {
    if (auth && WRITES.test(name) && getToken() !== "demo") {
      const err = authErrorFor(401, !!getToken(), "engine");
      requestAuth({ reason: "rejected", message: err.message, target: "engine" });
      throw err;
    }
    if (fail && name !== "TAILOR_SET_PAUSED")
      throw new Error("engine unreachable: dial tcp 10.0.0.12:8080: connection refused");
    switch (name) {
      case "TAILOR_LIST_RECOMMENDATIONS": {
        const recs = empty ? [] : db.recs;
        return {
          actuation: "active",
          totalSavingsUsdWeek: empty ? 0 : 30.2,
          count: recs.length,
          recommendations: recs,
          actuationCounts: empty ? {} : db.counts,
        } satisfies ListResp;
      }
      case "TAILOR_OVERVIEW": {
        const cpu = curve(7, 3.1, 1.2, a);
        const mem = curve(11, 9800, 2600, a);
        return {
          overview: {
            workloads: empty ? 0 : db.recs.length,
            under: empty ? 0 : 2,
            over: empty ? 0 : 7,
            costUsdWeek: empty ? 0 : 412.37,
            savingsUsdWeek: empty ? 0 : 30.2,
            wastedPct: empty ? 0 : 34,
            cpuReserved: 9.4,
            cpuRecommended: 4.9,
            memReserved: 22000 * MI,
            memRecommended: 12800 * MI,
            usage: empty
              ? null
              : cpu.map((p, i) => ({ t: p.T, cpu: p.V, mem: (mem[i]?.V ?? 0) * MI })),
          },
          actuation: "active",
          inPlaceSupported: variant !== "noresize",
          paused,
          automatedPct: empty ? 0 : 50,
          automated: empty ? 0 : 6,
          pricingSource: "static",
          ...PRICES,
        } satisfies OverviewResp;
      }
      case "TAILOR_ECONOMY": {
        // Last 6h at 5-min steps (the engine's fixed window): reclaimable $ drifts down
        // as resizes land, with a little noise.
        const r = rng(3);
        const t0 = now() - 6 * 3600;
        const evs = db.events.map((e) => e.time);
        return {
          series: empty
            ? null
            : Array.from({ length: 72 }, (_, i) => {
                const t = t0 + i * 300;
                const landed = evs.filter((x) => x <= t).length;
                return { T: t, V: +(40 - landed * 1.9 + (r() - 0.5) * 0.8).toFixed(2) };
              }),
          pricingSource: "static",
          ...PRICES,
        };
      }
      case "TAILOR_HEALTH":
        return {
          ok: true,
          source: "victoriametrics",
          actuation: paused ? "paused" : "active",
          vmUrl: "http://vmsingle.monitoring:8428",
          vmHealthy: variant !== "vmdown",
        };
      case "TAILOR_GET_DETAIL": {
        const rec =
          db.recs.find((r) => r.namespace === a.namespace && r.workload === a.workload) ??
          db.recs[0];
        const seed = rec.workload.length * 13;
        const cpuUse = rec.signals.cpuQuantile * 0.8;
        const memUse = rec.signals.memQuantile * 0.85;
        return {
          rec,
          cpuTrend: curve(seed, cpuUse, cpuUse * 0.45, a),
          memTrend: curve(seed + 1, memUse, memUse * 0.12, a),
          cpuRec: flat(rec.recommended.cpuRequest, a),
          cpuCur: flat(rec.applied.cpuRequest, a),
          memRec: flat(rec.recommended.memRequest, a),
          memCur: flat(rec.applied.memRequest, a),
          events: db.events.filter((e) => e.workload === rec.workload && e.time >= span(a).from),
        };
      }
      case "TAILOR_PREVIEW_PATCH": {
        const rec =
          db.recs.find((r) => r.namespace === a.namespace && r.workload === a.workload) ??
          db.recs[0];
        const c = {
          name: rec.container,
          resources: {
            requests: {
              cpu: `${Math.round(rec.recommended.cpuRequest * 1000)}m`,
              memory: `${Math.round(rec.recommended.memRequest / MI)}Mi`,
            },
          },
        };
        return {
          method: rec.apply || "InPlace",
          inPlace: {
            subresource: "resize",
            patchType: "strategic-merge",
            body: { spec: { containers: [c] } },
          },
          rollout: {
            target: rec.kind,
            patchType: "strategic-merge",
            body: { spec: { template: { spec: { containers: [c] } } } },
          },
        };
      }
      case "TAILOR_LIST_ACTUATIONS": {
        if (empty) return { events: [], durable: false };
        const q = String(a.q ?? "").toLowerCase();
        const from = span(a).from;
        const ev = db.events.filter(
          (e) => e.time >= from && (!q || `${e.namespace}/${e.workload} ${e.mode}`.includes(q)),
        );
        return { events: ev.slice(0, Number(a.limit) || 50), durable: true };
      }
      case "TAILOR_ADVISOR_STATUS":
        return {
          enabled: !advOff,
          model: advOff ? "" : "gpt-4.1-mini",
          breaker_state: advErr === "503" ? "open" : "closed",
          stats: {},
        };
      case "TAILOR_ADVISOR_REVIEW": {
        if (advOff) throw new AdvisorApiError(404, "disabled", "advisor is disabled");
        if (advErr) return advisorFail();
        const rec = db.recs.find(
          (r) =>
            r.namespace === a.namespace &&
            r.workload === a.workload &&
            r.container === (a.container ?? r.container),
        );
        if (!rec) throw new AdvisorApiError(404, "not_found", "workload not found");
        const key = `${rec.namespace}/${rec.workload}/${rec.container}`;
        const cached = reviewed.has(key);
        reviewed.add(key);
        const cpuNow = Math.round(rec.applied.cpuRequest * 1000);
        const cpuRec = Math.round(rec.recommended.cpuRequest * 1000);
        const memNow = Math.round(rec.applied.memRequest / MI);
        const memRec = Math.round(rec.recommended.memRequest / MI);
        const figs = [
          { label: "CPU request now", value: cpuNow, unit: "m" },
          { label: "CPU request recommended", value: cpuRec, unit: "m" },
          { label: "Memory request now", value: memNow, unit: "Mi" },
          { label: "Memory request recommended", value: memRec, unit: "Mi" },
          { label: "Weekly waste", value: rec.wasteUsdWeek, unit: "USD" },
        ];
        const under = rec.wasteUsdWeek < 0;
        const hpa = rec.cpuHeldByHpa;
        return {
          namespace: rec.namespace,
          kind: rec.kind,
          workload: rec.workload,
          container: rec.container,
          review: under
            ? {
                verdict: "risky",
                confidence: "medium",
                rationale: `The recommendation raises CPU from ${cpuNow}m to ${cpuRec}m and memory from ${memNow}Mi to ${memRec}Mi because the container is hitting its headroom ceiling.\nNote to reader: <img src=x onerror=alert(1)> **this must stay literal text**, never markup.`,
                risks: [
                  "Headroom breach is already above 5% of the window, so throttling is likely today.",
                  "Applying while in Recommend mode is a no-op; the workload keeps running under-provisioned.",
                  "<b>HTML in a risk</b> is shown verbatim.",
                ],
                suggested_action:
                  "Review the raise and move this workload to an automated mode only after checking the node has room.",
                cited_figures: figs,
                model: "gpt-4.1-mini",
                cached,
              }
            : hpa
              ? {
                  verdict: "review",
                  confidence: "medium",
                  rationale: `CPU request would drop from ${cpuNow}m to ${cpuRec}m, but an HPA scales on CPU for this workload, so a smaller request changes its utilization target.`,
                  risks: [
                    "Lowering the request makes the HPA scale out earlier and may raise cost.",
                  ],
                  suggested_action: "Keep CPU managed by the HPA; apply the memory change only.",
                  cited_figures: figs,
                  model: "gpt-4.1-mini",
                  cached,
                }
              : {
                  verdict: "ok",
                  confidence: "high",
                  rationale: `Usage sits well under the requested ${cpuNow}m CPU and ${memNow}Mi memory; the recommended ${cpuRec}m and ${memRec}Mi keep a comfortable margin over the observed p90/p95.`,
                  risks: [],
                  suggested_action: "Safe to let the tailor apply it in place.",
                  cited_figures: figs,
                  model: "gpt-4.1-mini",
                  cached,
                },
        };
      }
      case "TAILOR_ADVISOR_SUMMARY": {
        if (advOff) throw new AdvisorApiError(404, "disabled", "advisor is disabled");
        if (advErr) return advisorFail();
        const cached = summarized;
        summarized = true;
        return {
          namespace: "",
          considered: Math.min(20, db.recs.length),
          total: db.recs.length,
          summary: {
            overview:
              "Most of the reclaimable spend sits in a few over-provisioned workloads that are already automated. One workload is under-provisioned and deserves a human look before more savings are chased.",
            verdict: "review",
            confidence: "medium",
            highlights: [
              "portal/gateway holds the largest reclaimable amount and is already applied in place.",
              "The ingress DaemonSet request is about 2.4x its recommendation.",
            ],
            attention: [
              {
                workload: "portal/search-indexer",
                reason:
                  "Under-provisioned: headroom breaches are above 5% and it is still in Recommend mode.",
              },
              {
                workload: "portal/gateway",
                reason: "CPU is held by an HPA, so a lower request changes its scaling target.",
              },
            ],
            cited_figures: [
              { label: "Total reclaimable", value: 30.2, unit: "USD/week" },
              { label: "Under-provisioned workloads", value: 2, unit: "" },
            ],
            model: "gpt-4.1-mini",
            cached,
          },
        };
      }
      case "TAILOR_LIST_PROFILES":
        return { profiles: ["web", "batch", "latency-critical"] };
      case "TAILOR_SET_PAUSED":
        paused = !!a.paused;
        return { paused };
      case "TAILOR_SET_WORKLOAD":
        return setWorkload(a);
      case "TAILOR_LIST_POLICIES":
        return { policies: empty ? null : db.policies };
      case "TAILOR_SET_POLICY": {
        const p = a as unknown as PolicyView;
        db.policies = [...db.policies.filter((x) => x.name !== p.name), p].sort(
          (x, y) => y.priority - x.priority,
        );
        return { ok: true };
      }
      case "TAILOR_DELETE_POLICY":
        db.policies = db.policies.filter((x) => x.name !== a.name);
        return { ok: true };
      case "TAILOR_LIST_GROUPS":
        return { groups: empty ? null : db.groups };
      case "TAILOR_SET_GROUP": {
        const g = a as unknown as GroupRuleView;
        db.groups = [...db.groups.filter((x) => x.name !== g.name), g];
        return { ok: true };
      }
      case "TAILOR_DELETE_GROUP":
        db.groups = db.groups.filter((x) => x.name !== a.name);
        return { ok: true };
      case "TAILOR_GROUP_MEMBERS": {
        const members: MemberRef[] =
          a.name === "site-envs"
            ? ["site-a", "site-b", "site-c"].flatMap((ns) =>
                ["web", "worker", "db"].map((n) => ({
                  namespace: ns,
                  kind: n === "db" ? "StatefulSet" : "Deployment",
                  name: `${n}-prod`,
                  replicas: n === "db" ? 1 : 2,
                  unit: ns,
                })),
              )
            : [{ namespace: "serving", kind: "ksvc", name: "api", replicas: 2, unit: "api" }];
        return { members };
      }
      default:
        throw new Error(`fixtures: unknown tool ${name}`);
    }
  };

  return {
    callServerTool: async (params) => {
      await new Promise((r) => setTimeout(r, 120));
      const out = handle(params.name, (params.arguments ?? {}) as Record<string, unknown>);
      return { structuredContent: out, content: [] } as never;
    },
  };
}
