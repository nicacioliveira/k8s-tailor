import type { Decision, Rec } from "./types";

// Savings maths, mirroring the engine (internal/recommender): $/week =
// (Δcpu cores × $/core-hr + Δmem GiB × $/GiB-hr) × 168 h × replicas.

export const HOURS_PER_WEEK = 168;
const GIB = 1024 * 1024 * 1024;

export interface Prices {
  cpuCoreHr: number;
  gbHr: number;
}

/** Weekly $ a single applied decision saves (positive) or costs (negative), per replica × replicas. */
export function decisionUsdWeek(e: Decision, p: Prices, replicas = 1): number {
  const cpu = (e.fromCpu - e.toCpu) * p.cpuCoreHr;
  const mem = ((e.fromMem - e.toMem) / GIB) * p.gbHr;
  return (cpu + mem) * HOURS_PER_WEEK * Math.max(1, replicas);
}

export const decisionKey = (e: {
  namespace: string;
  kind: string;
  workload: string;
  container: string;
}) => `${e.namespace}/${e.kind}/${e.workload}/${e.container}`;

export type SavingsPoint = {
  t: number;
  /** Still reclaimable if every recommendation were applied ($/week, engine series). */
  projected?: number;
  /** Cumulative $/week already captured by decisions applied since the window start (estimate). */
  applied?: number;
};

/**
 * Merge the engine's reclaimable-$ series with a running total of applied decisions.
 * Decisions are matched to the current recommendation for their replica count (the
 * decision log doesn't carry it); unmatched ones count one replica.
 */
export function savingsTimeline(
  series: { T: number; V: number }[],
  events: Decision[],
  prices: Prices,
  recs: Rec[],
): SavingsPoint[] {
  if (!series.length) return [];
  const replicas = new Map(recs.map((r) => [decisionKey(r), r.replicas]));
  const from = series[0].T;
  const evs = events
    .filter((e) => e.time >= from)
    .sort((a, b) => a.time - b.time)
    .map((e) => ({ t: e.time, usd: decisionUsdWeek(e, prices, replicas.get(decisionKey(e))) }));
  let i = 0;
  let acc = 0;
  return series.map((p) => {
    while (i < evs.length && evs[i].t <= p.T) acc += evs[i++].usd;
    return { t: p.T, projected: round2(p.V), applied: round2(acc) };
  });
}

/** Sum of applied decisions in a window, $/week. */
export function appliedUsdWeek(events: Decision[], prices: Prices, recs: Rec[], since = 0) {
  const replicas = new Map(recs.map((r) => [decisionKey(r), r.replicas]));
  return round2(
    events
      .filter((e) => e.time >= since)
      .reduce((s, e) => s + decisionUsdWeek(e, prices, replicas.get(decisionKey(e))), 0),
  );
}

/** Biggest reclaimable spend first; only over-provisioned (positive waste) rows. */
export function topWaste(recs: Rec[], n = 10): Rec[] {
  return recs
    .filter((r) => r.wasteUsdWeek > 0)
    .sort((a, b) => b.wasteUsdWeek - a.wasteUsdWeek)
    .slice(0, n);
}

/** Weekly $ the under-provisioned rows need on top (positive number). */
export const neededUsdWeek = (recs: Rec[]) =>
  round2(recs.reduce((s, r) => s + (r.wasteUsdWeek < 0 ? -r.wasteUsdWeek : 0), 0));

const round2 = (n: number) => Math.round(n * 100) / 100;
