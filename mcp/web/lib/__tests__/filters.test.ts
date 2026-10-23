import { describe, expect, test } from "bun:test";
import { buildHash, parseHash } from "../hash-route";
import { statusId } from "../status";
import {
  EMPTY_FILTERS,
  filterRecs,
  isFiltered,
  parseFilters,
  serializeFilters,
  sortRecs,
} from "../workload-filter";
import { rec } from "./fixtures";

const recs = [
  rec({ workload: "gateway", wasteUsdWeek: 6.8, mode: "Both", automated: true }),
  rec({ namespace: "data", workload: "postgres", kind: "StatefulSet", wasteUsdWeek: 3.9 }),
  rec({ workload: "sessions", oomRisk: true, wasteUsdWeek: -1, overridden: true, mode: "InPlace" }),
  rec({ namespace: "batch", workload: "etl", confident: false, wasteUsdWeek: 0.3, mode: "" }),
];

describe("hash route", () => {
  test("parses view and params, tolerates a trailing slash", () => {
    const r = parseHash("#/workloads/?ns=portal&oom=1");
    expect(r.view).toBe("workloads");
    expect(r.params.get("ns")).toBe("portal");
    expect(parseHash("").view).toBe("");
  });
  test("round-trips through buildHash", () => {
    const p = new URLSearchParams({ q: "a b", status: "over" });
    expect(parseHash(buildHash("workloads", p)).params.get("q")).toBe("a b");
    expect(buildHash("health")).toBe("#/health");
  });
});

describe("filters <-> URL", () => {
  test("defaults keep a clean URL", () => {
    expect(serializeFilters(EMPTY_FILTERS).toString()).toBe("");
  });
  test("round-trip every field", () => {
    const f = {
      ...EMPTY_FILTERS,
      q: "pay",
      ns: "portal",
      status: "over" as const,
      kind: "Deployment",
      mode: "Both",
      oom: true,
      pinned: true,
      sort: "cpu" as const,
      dir: "asc" as const,
    };
    expect(parseFilters(serializeFilters(f))).toEqual(f);
  });
  test("ignores unknown status / sort values from a hand-edited URL", () => {
    const f = parseFilters(new URLSearchParams("status=bogus&sort=nope&dir=sideways"));
    expect(f.status).toBe("");
    expect(f.sort).toBe("");
    expect(f.dir).toBe("desc");
  });
  test("sort alone is not 'filtered'", () => {
    expect(isFiltered({ ...EMPTY_FILTERS, sort: "waste" })).toBe(false);
    expect(isFiltered({ ...EMPTY_FILTERS, oom: true })).toBe(true);
  });
});

describe("filterRecs", () => {
  const names = (f: Partial<typeof EMPTY_FILTERS>) =>
    filterRecs(recs, { ...EMPTY_FILTERS, ...f }).map((r) => r.workload);
  test("text matches namespace/workload, container and kind, case-insensitive", () => {
    expect(names({ q: "DATA/post" })).toEqual(["postgres"]);
    expect(names({ q: "statefulset" })).toEqual(["postgres"]);
  });
  test("namespace, kind, status, mode", () => {
    expect(names({ ns: "portal" })).toEqual(["gateway", "sessions"]);
    expect(names({ kind: "StatefulSet" })).toEqual(["postgres"]);
    expect(names({ status: "gathering" })).toEqual(["etl"]);
    expect(names({ mode: "Recommend" })).toEqual(["postgres", "etl"]); // "" counts as Recommend
  });
  test("only-OOM and only-pinned combine with AND", () => {
    expect(names({ oom: true })).toEqual(["sessions"]);
    expect(names({ pinned: true, ns: "data" })).toEqual([]);
  });
  test("status precedence: OOM beats under-provisioned", () => {
    expect(statusId(recs[2])).toBe("oom");
  });
});

describe("sortRecs", () => {
  test("no key keeps engine order", () => {
    expect(sortRecs(recs, "", "desc")).toEqual(recs);
  });
  test("waste desc / asc", () => {
    expect(sortRecs(recs, "waste", "desc").map((r) => r.workload)).toEqual([
      "gateway",
      "postgres",
      "etl",
      "sessions",
    ]);
    expect(sortRecs(recs, "waste", "asc")[0].workload).toBe("sessions");
  });
  test("workload sorts by namespace/name", () => {
    expect(sortRecs(recs, "workload", "asc").map((r) => r.namespace)).toEqual([
      "batch",
      "data",
      "portal",
      "portal",
    ]);
  });
  test("cpu sorts by the size of the cut; ties keep original order", () => {
    const a = rec({ workload: "a", recommended: { ...recs[0].recommended, cpuRequest: 0.9 } });
    const b = rec({ workload: "b", recommended: { ...recs[0].recommended, cpuRequest: 0.1 } });
    const c = rec({ workload: "c", recommended: { ...recs[0].recommended, cpuRequest: 0.9 } });
    expect(sortRecs([a, b, c], "cpu", "desc").map((r) => r.workload)).toEqual(["b", "a", "c"]);
  });
});
