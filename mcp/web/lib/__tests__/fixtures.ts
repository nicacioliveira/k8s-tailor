import type { Decision, Rec } from "../types";

const MI = 1024 * 1024;

/** Minimal valid Rec; override what the test cares about. */
export function rec(over: Partial<Rec> = {}): Rec {
  return {
    namespace: "portal",
    workload: "api",
    kind: "Deployment",
    container: "api",
    replicas: 1,
    current: { cpuRequest: 1, memRequest: 512 * MI, memLimit: 1024 * MI },
    applied: { cpuRequest: 1, memRequest: 512 * MI, memLimit: 1024 * MI },
    recommended: { cpuRequest: 0.5, memRequest: 256 * MI, memLimit: 512 * MI },
    specDrift: false,
    signals: {
      cpuQuantile: 0.4,
      cpuPeak: 0.6,
      cpuForecast: null,
      memQuantile: 200 * MI,
      memPeak: 220 * MI,
      memForecastBytes: null,
      memTrendBytes: null,
      samples: 1000,
      headroomBreachCpu: 0,
      headroomBreachMem: 0,
    },
    mode: "Recommend",
    apply: "InPlace",
    direction: "",
    policy: "default",
    profile: "",
    automated: false,
    overridden: false,
    managed: true,
    strategy: "",
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
    wasteUsdWeek: 1,
    ...over,
  };
}

export function decision(over: Partial<Decision> = {}): Decision {
  return {
    time: 1000,
    mode: "inplace",
    namespace: "portal",
    kind: "Deployment",
    workload: "api",
    container: "api",
    fromCpu: 1,
    toCpu: 0.5,
    fromMem: 1024 * MI,
    toMem: 512 * MI,
    ...over,
  };
}

/** In-memory Web Storage for code that touches session/localStorage under bun test. */
export class MemStorage implements Storage {
  private m = new Map<string, string>();
  get length() {
    return this.m.size;
  }
  clear() {
    this.m.clear();
  }
  getItem(k: string) {
    return this.m.get(k) ?? null;
  }
  key(i: number) {
    return [...this.m.keys()][i] ?? null;
  }
  removeItem(k: string) {
    this.m.delete(k);
  }
  setItem(k: string, v: string) {
    this.m.set(k, String(v));
  }
}
