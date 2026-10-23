import { classOf, groupSiblings } from "./containers";
import { type StatusId, statusId } from "./status";
import type { Rec } from "./types";

// Workloads table filters + sort, serialised into the URL hash so a filtered view can
// be linked and survives reloads: #/workloads?q=pay&ns=portal&status=over&oom=1&sort=waste.

export type SortKey = "workload" | "cpu" | "mem" | "waste";
export type SortDir = "asc" | "desc";

export interface WorkloadFilters {
  q: string;
  ns: string;
  status: StatusId | "";
  kind: string;
  mode: string;
  /** Container class (app, injected-sidecar, native-sidecar, init, ephemeral). */
  cls: string;
  /** Service mesh that injected the container (istio, linkerd). */
  mesh: string;
  oom: boolean;
  pinned: boolean;
  sort: SortKey | "";
  dir: SortDir;
}

export const EMPTY_FILTERS: WorkloadFilters = {
  q: "",
  ns: "",
  status: "",
  kind: "",
  mode: "",
  cls: "",
  mesh: "",
  oom: false,
  pinned: false,
  sort: "",
  dir: "desc",
};

const SORTS: SortKey[] = ["workload", "cpu", "mem", "waste"];
const STATUS_IDS: StatusId[] = ["oom", "limit", "gathering", "over", "under", "ok"];

export function parseFilters(p: URLSearchParams): WorkloadFilters {
  const sort = p.get("sort") as SortKey;
  const status = p.get("status") as StatusId;
  return {
    q: p.get("q") ?? "",
    ns: p.get("ns") ?? "",
    status: STATUS_IDS.includes(status) ? status : "",
    kind: p.get("kind") ?? "",
    mode: p.get("mode") ?? "",
    cls: p.get("class") ?? "",
    mesh: p.get("mesh") ?? "",
    oom: p.get("oom") === "1",
    pinned: p.get("pinned") === "1",
    sort: SORTS.includes(sort) ? sort : "",
    dir: p.get("dir") === "asc" ? "asc" : "desc",
  };
}

/** Only non-default values are written, so a clean view keeps a clean URL. */
export function serializeFilters(f: WorkloadFilters): URLSearchParams {
  const p = new URLSearchParams();
  if (f.q.trim()) p.set("q", f.q);
  if (f.ns) p.set("ns", f.ns);
  if (f.status) p.set("status", f.status);
  if (f.kind) p.set("kind", f.kind);
  if (f.mode) p.set("mode", f.mode);
  if (f.cls) p.set("class", f.cls);
  if (f.mesh) p.set("mesh", f.mesh);
  if (f.oom) p.set("oom", "1");
  if (f.pinned) p.set("pinned", "1");
  if (f.sort) {
    p.set("sort", f.sort);
    if (f.dir === "asc") p.set("dir", "asc");
  }
  return p;
}

/** True when anything narrows the rows (sort alone doesn't). */
export const isFiltered = (f: WorkloadFilters) =>
  !!(f.q.trim() || f.ns || f.status || f.kind || f.mode || f.cls || f.mesh || f.oom || f.pinned);

/** Effective mode as shown in the table: an empty mode means recommend-only. */
export const modeOf = (r: Rec) => r.mode || "Recommend";

export function filterRecs(recs: Rec[], f: WorkloadFilters): Rec[] {
  const needle = f.q.trim().toLowerCase();
  return recs.filter(
    (r) =>
      (!needle ||
        `${r.namespace}/${r.workload} ${r.container} ${r.kind}`.toLowerCase().includes(needle)) &&
      (!f.ns || r.namespace === f.ns) &&
      (!f.status || statusId(r) === f.status) &&
      (!f.kind || r.kind === f.kind) &&
      (!f.mode || modeOf(r) === f.mode) &&
      (!f.cls || classOf(r) === f.cls) &&
      (!f.mesh || (r.mesh ?? "") === f.mesh) &&
      (!f.oom || r.oomRisk) &&
      (!f.pinned || r.overridden),
  );
}

const SORT_VALUE: Record<SortKey, (r: Rec) => number | string> = {
  workload: (r) => `${r.namespace}/${r.workload}/${r.container}`.toLowerCase(),
  // Size of the proposed cut (running − recommended): descending puts the biggest
  // reductions first and the biggest raises (negative) last, like $/week.
  cpu: (r) => r.applied.cpuRequest - r.recommended.cpuRequest,
  mem: (r) => r.applied.memRequest - r.recommended.memRequest,
  waste: (r) => r.wasteUsdWeek,
};

/** Stable sort; no key = engine order (ranked by weekly $ waste) with each workload's containers kept together. */
export function sortRecs(recs: Rec[], key: SortKey | "", dir: SortDir): Rec[] {
  if (!key) return groupSiblings(recs);
  const val = SORT_VALUE[key];
  const sign = dir === "asc" ? 1 : -1;
  return recs
    .map((r, i) => ({ r, i, v: val(r) }))
    .sort((a, b) => {
      const c = typeof a.v === "string" ? a.v.localeCompare(b.v as string) : a.v - (b.v as number);
      return c !== 0 ? c * sign : a.i - b.i;
    })
    .map((x) => x.r);
}

/** Distinct, sorted values for a filter's options. */
export const distinct = (recs: Rec[], pick: (r: Rec) => string) =>
  [...new Set(recs.map(pick).filter(Boolean))].sort((a, b) => a.localeCompare(b));
