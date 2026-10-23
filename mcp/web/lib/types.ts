// Types mirroring the engine API (structuredContent of the TAILOR_* tools).

export interface Resources {
  cpuRequest: number;
  cpuLimit?: number;
  memRequest: number;
  memLimit: number;
}

export interface Signals {
  cpuQuantile: number;
  cpuPeak: number;
  cpuForecast: number | null;
  memQuantile: number;
  memPeak: number;
  memForecastBytes: number | null;
  memTrendBytes: number | null;
  samples: number;
  headroomBreachCpu: number;
  headroomBreachMem: number;
  /** p99 / p90 of the CPU window; null until 12 h of samples exist. */
  burstiness?: number | null;
  /** The CPU quantile the request was sized from (0.95 when autoQuantile lifted it). */
  cpuQuantileUsed?: number;
  burstQuantileRaised?: boolean;
  /** Plain-language reason for the quantile choice (always present). */
  burstReason?: string;
  /** Usage is pinned at the CPU limit or the kernel throttles it. */
  cpuCensored?: boolean;
  /** The CPU limit was parked after repeated raises the container consumed. */
  cpuGreedy?: boolean;
  /** Raises of the current run (counted from the limit first seen censored). */
  cpuCensorSteps?: number;
}

export interface Rec {
  namespace: string;
  workload: string;
  kind: string;
  container: string;
  replicas: number;
  current: Resources;
  applied: Resources;
  recommended: Resources;
  specDrift: boolean;
  signals: Signals;
  mode: string;
  apply: string;
  direction: string;
  policy: string;
  profile: string;
  automated: boolean;
  overridden: boolean;
  managed: boolean;
  strategy: string;
  pdbName: string;
  pdbProtected: boolean;
  pdbAllowed: number;
  autoscaler: {
    kind: string;
    name: string;
    managesCpu: boolean;
    managesMem: boolean;
    min: number;
    max: number;
  };
  knative: { service: string; revision: string };
  cpuHeldByHpa: boolean;
  memHeldByHpa: boolean;
  /** Enough samples to actuate at least one resource (cpuConfident || memConfident). */
  confident: boolean;
  /** Per resource; absent on an engine from before the split (= unknown, never "confident"). */
  cpuConfident?: boolean;
  memConfident?: boolean;
  /** Set locally after an optimistic exclusion; the engine does not list excluded containers. */
  excluded?: boolean;
  oomRisk: boolean;
  limitCapped: boolean;
  wasteUsdWeek: number;
  // Container role and effective per-container policy (docs/CONTAINERS.md). Optional:
  // an engine from before container classification omits all of them (= plain app).
  containerClass?: ContainerClass;
  nativeSidecar?: boolean;
  inTemplate?: boolean;
  mesh?: string;
  containerPolicy?: string;
  proxyAnnotations?: boolean;
  actuationPath?: string;
}

/** `init` and `ephemeral` are never sized, so the engine does not list them today. */
export type ContainerClass = "app" | "injected-sidecar" | "native-sidecar" | "init" | "ephemeral";

export interface Stat {
  count: number;
  last: number;
}

export interface ListResp {
  /** Time the engine answered (request time, not the last recompute). */
  generatedAt?: string;
  actuation: string;
  totalSavingsUsdWeek: number;
  count: number;
  recommendations: Rec[];
  actuationCounts: Record<string, Stat>;
}

export interface UsagePoint {
  t: number;
  cpu: number;
  mem: number;
}

export interface Overview {
  workloads: number;
  under: number;
  over: number;
  costUsdWeek: number;
  savingsUsdWeek: number;
  wastedPct: number;
  cpuReserved: number;
  cpuRecommended: number;
  memReserved: number;
  memRecommended: number;
  usage: UsagePoint[] | null;
}

export interface OverviewResp {
  overview: Overview;
  actuation: string;
  inPlaceSupported: boolean;
  paused: boolean;
  automatedPct: number;
  automated: number;
  pricingSource: string;
  priceCpuCoreHr?: number;
  priceGbHr?: number;
}

/** GET /healthz (tool TAILOR_HEALTH). */
export interface HealthResp {
  ok: boolean;
  source: string;
  actuation: string;
  vmUrl: string;
  vmHealthy: boolean;
}

/** GET /api/economy (tool TAILOR_ECONOMY): reclaimable $/week over the last 6h. */
export interface EconomyResp {
  series: Sample[] | null;
  pricingSource?: string;
  priceCpuCoreHr?: number;
  priceGbHr?: number;
}

export interface ActuationsResp {
  events: Decision[] | null;
  durable?: boolean;
}

export interface Sample {
  T: number;
  V: number;
}

export interface Decision {
  time: number;
  mode: string;
  namespace: string;
  kind: string;
  workload: string;
  container: string;
  fromCpu: number;
  toCpu: number;
  fromMem: number;
  toMem: number;
}

export interface Detail {
  rec: Rec;
  cpuTrend: Sample[] | null;
  memTrend: Sample[] | null;
  events: Decision[] | null;
  cpuRec: Sample[] | null;
  cpuCur: Sample[] | null;
  memRec: Sample[] | null;
  memCur: Sample[] | null;
}

export interface WindowView {
  days?: string[];
  start?: string;
  end?: string;
  from?: string;
  until?: string;
}

export interface ScheduleView {
  timezone?: string;
  outsideMode?: string;
  windows?: WindowView[];
}

/** One TailorPolicy spec.containers[] rule. First matching rule wins (engine side). */
export interface ContainerRuleView {
  match: { names?: string[]; classes?: string[] };
  exclude?: boolean;
  mode?: string;
  apply?: string;
  direction?: string;
  profileRef?: string;
  proxyAnnotations?: boolean;
  cpu?: ContainerResSpec;
  memory?: ContainerResSpec;
}

export interface ContainerResSpec {
  quantile?: number;
  margin?: number;
  min?: string;
  max?: string;
  limitFactor?: number;
}

export interface PolicyView {
  name: string;
  priority: number;
  mode: string;
  apply: string;
  profileRef: string;
  namespaces: string[] | null;
  matchLabels: Record<string, string> | null;
  cpuMargin: number;
  memMargin: number;
  cpuMin: string;
  cpuMax: string;
  memMin: string;
  memMax: string;
  cpuLimitFactor: number;
  memLimitFactor: number;
  halfLife: string;
  season: string;
  allowExceedLimit: boolean;
  direction?: string;
  oomOverridesDirection?: boolean;
  scheduled: boolean;
  schedule: ScheduleView;
  /** Per-container rules. Omitted/undefined on save = the engine keeps the CR's rules. */
  containers?: ContainerRuleView[];
}

export interface GroupRuleView {
  name: string;
  groupBy: string;
  namespaces: string[];
  actuation: string;
  cohortBy: string;
  cohortSelect: string;
}

export interface MemberRef {
  namespace: string;
  kind: string;
  name: string;
  replicas: number;
  unit: string;
}

export const MODES = ["Off", "Recommend", "Webhook", "InPlace", "Both"];
export const APPLY_METHODS = ["InPlace", "Rollout", "InPlacePreferred"];
export const SEASONS = ["daily", "weekly", "both", "off"];
export const DIRECTIONS = ["Both", "Down", "Up"];
export const CONTAINER_CLASSES = ["app", "injected-sidecar", "native-sidecar"] as const;
export const AUTOMATED_MODES = ["Both", "InPlace", "Webhook"];

export const LIST_KEY = ["TAILOR_LIST_RECOMMENDATIONS", {}];

/** Radix Select forbids "" as a value: this sentinel means "use the policy default". */
export const GROUP_DEFAULT = "__group__";
