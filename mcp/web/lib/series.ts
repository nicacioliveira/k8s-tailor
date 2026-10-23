import type { Sample } from "./types";

// Chart data shaping shared by the cluster, compare and fit charts.

export type FitPoint = {
  t: number;
  used?: number;
  recommended?: number;
  current?: number;
  fit?: number;
  waste?: number;
  margin?: number;
  [series: string]: number | undefined;
};

/**
 * Honest 3-layer band stacked up to the reserved request:
 *   used -> safety margin (used..fit) -> RECLAIMABLE waste (fit..reserved).
 * A workload pinned at its floor (fit == reserved, e.g. memory at MIN) shows margin,
 * not a huge "waste": only the fit..reserved gap is removable.
 * The request (current) is forward-filled between samples.
 */
export function addWasteBand(data: FitPoint[]): FitPoint[] {
  // Seed with the first known request so the leading edge isn't drawn as "no request".
  let cur = data.find((p) => p.current != null)?.current ?? 0;
  for (const p of data) {
    if (p.current != null) cur = p.current;
    const fit = p.fit ?? p.used ?? 0;
    if (p.used != null) p.margin = +Math.max(fit - p.used, 0).toFixed(1);
    if (cur > 0) p.waste = +Math.max(cur - fit, 0).toFixed(1);
  }
  return data;
}

/**
 * Recommendation curve that hugs the usage shape: at each point, the trailing-window
 * q-quantile of usage × (1 + margin) — "what the request would be right now".
 * The server recommendation is a floor (min request, anti-OOM peak): the rolling
 * quantile alone would dip below what the tailor will ever set and misread as waste.
 */
export function addRollingFit(data: FitPoint[], winPts = 24, q = 0.9, margin = 0.12): FitPoint[] {
  const used = data.map((p) => p.used);
  let lastRec = data.find((p) => p.recommended != null)?.recommended;
  for (let i = 0; i < data.length; i++) {
    if (data[i].recommended != null) lastRec = data[i].recommended;
    const from = Math.max(0, i - winPts + 1);
    const w = used
      .slice(from, i + 1)
      .filter((v): v is number => v != null)
      .sort((a, b) => a - b);
    if (!w.length) continue;
    const idx = Math.min(w.length - 1, Math.floor(q * (w.length - 1)));
    let f = w[idx] * (1 + margin);
    if (lastRec != null) f = Math.max(f, lastRec);
    data[i].fit = +f.toFixed(1);
  }
  return data;
}

/** Merge usage / recommended / current series by timestamp, scaled to display units. */
export function mergeSeries(
  used: Sample[] | null | undefined,
  rec: Sample[] | null | undefined,
  cur: Sample[] | null | undefined,
  scale: (v: number) => number,
): FitPoint[] {
  const m = new Map<number, FitPoint>();
  const put = (arr: Sample[] | null | undefined, key: "used" | "recommended" | "current") => {
    for (const p of arr ?? []) {
      const e = m.get(p.T) ?? { t: p.T };
      e[key] = +scale(p.V).toFixed(1);
      m.set(p.T, e);
    }
  };
  put(used, "used");
  put(rec, "recommended");
  put(cur, "current");
  return [...m.values()].sort((a, b) => a.t - b.t);
}

/** Keep only points that carry a usage sample (request/rec. points are forward-filled first). */
export const withUsage = (data: FitPoint[]) =>
  data.some((p) => p.used != null) ? data.filter((p) => p.used != null) : data;
