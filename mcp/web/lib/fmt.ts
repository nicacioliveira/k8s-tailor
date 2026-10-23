import { formatNumber, formatUSD } from "../ui/lib/format.ts";

// Display helpers. "—" always means "no value" (never rendered as 0).
export const MI = 1024 * 1024;

/** cores -> "250m"; undefined -> "—". */
export const milli = (c?: number | null) =>
  c == null ? "—" : `${formatNumber(Math.round(c * 1000))}m`;

/** bytes -> "512Mi"; undefined -> "—". */
export const mib = (b?: number | null) =>
  b == null ? "—" : `${formatNumber(Math.round(b / MI))}Mi`;

export const usd = (n: number) => formatUSD(n);

export const pct1 = (ratio: number) => `${formatNumber(ratio * 100, 1)}%`;

export const nowSec = () => Math.floor(Date.now() / 1000);

const time = new Intl.DateTimeFormat("en-US", { hour: "2-digit", minute: "2-digit" });
const dayTime = new Intl.DateTimeFormat("en-US", {
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
});
const full = new Intl.DateTimeFormat("en-US", { dateStyle: "medium", timeStyle: "medium" });

export const hhmm = (t: number) => time.format(new Date(t * 1000));
export const dayHhmm = (t: number) => dayTime.format(new Date(t * 1000));
export const fullDate = (t: number) => full.format(new Date(t * 1000));

/** Relative age: "just now", "12s ago", "4m ago", "3h ago", "2d ago". */
export const ago = (t: number) => {
  const s = Math.max(0, nowSec() - t);
  if (s < 5) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
};

/** Relative change, e.g. "−42%", "+8%", "new". */
export const change = (from: number, to: number) => {
  if (!from) return to > 0 ? "new" : "—";
  const d = Math.round(((to - from) / from) * 100);
  return d > 0 ? `+${d}%` : d < 0 ? `−${-d}%` : "0%";
};

export const keyOf = (r: {
  namespace: string;
  kind: string;
  workload: string;
  container: string;
}) => `${r.namespace}/${r.kind}/${r.workload}/${r.container}`;

export const workloadKey = (r: { namespace: string; kind: string; workload: string }) =>
  `${r.namespace}/${r.kind}/${r.workload}`;
