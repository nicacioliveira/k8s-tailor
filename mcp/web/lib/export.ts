import { MI } from "./fmt";
import { statusId, statusLabel } from "./status";
import type { Rec } from "./types";
import { modeOf } from "./workload-filter";

// Client-side export of the (filtered, sorted) workloads table. Units are explicit in
// the column names and values are raw numbers, so spreadsheets can sum them.

const COLUMNS: [string, (r: Rec) => string | number | boolean | undefined][] = [
  ["namespace", (r) => r.namespace],
  ["kind", (r) => r.kind],
  ["workload", (r) => r.workload],
  ["container", (r) => r.container],
  ["replicas", (r) => r.replicas],
  ["status", (r) => statusLabel(statusId(r))],
  ["mode", (r) => modeOf(r)],
  ["automated", (r) => r.automated],
  ["pinned", (r) => r.overridden],
  ["policy", (r) => r.policy],
  ["profile", (r) => r.profile],
  ["cpu_request_millicores", (r) => Math.round(r.applied.cpuRequest * 1000)],
  ["cpu_recommended_millicores", (r) => Math.round(r.recommended.cpuRequest * 1000)],
  [
    "cpu_limit_millicores",
    (r) => (r.current.cpuLimit ? Math.round(r.current.cpuLimit * 1000) : ""),
  ],
  [
    "cpu_limit_recommended_millicores",
    (r) => (r.recommended.cpuLimit ? Math.round(r.recommended.cpuLimit * 1000) : ""),
  ],
  ["mem_request_mib", (r) => Math.round(r.applied.memRequest / MI)],
  ["mem_recommended_mib", (r) => Math.round(r.recommended.memRequest / MI)],
  ["mem_limit_mib", (r) => (r.current.memLimit ? Math.round(r.current.memLimit / MI) : "")],
  [
    "mem_limit_recommended_mib",
    (r) => (r.recommended.memLimit ? Math.round(r.recommended.memLimit / MI) : ""),
  ],
  ["waste_usd_week", (r) => r.wasteUsdWeek],
  ["oom_risk", (r) => r.oomRisk],
  ["confident", (r) => r.confident],
  ["samples", (r) => r.signals?.samples],
];

/**
 * RFC 4180 cell. Text that a spreadsheet would run as a formula (=, +, -, @, tab, CR)
 * is prefixed with a quote — workload names come from the cluster, not from us.
 */
export function csvCell(v: string | number | boolean | undefined | null): string {
  if (v == null) return "";
  if (typeof v === "number") return Number.isFinite(v) ? String(v) : "";
  if (typeof v === "boolean") return v ? "true" : "false";
  let s = v;
  if (/^[=+\-@\t\r]/.test(s)) s = `'${s}`;
  return /[",\n\r]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function recsToCSV(recs: Rec[]): string {
  const head = COLUMNS.map(([h]) => h).join(",");
  const rows = recs.map((r) => COLUMNS.map(([, get]) => csvCell(get(r))).join(","));
  return `${[head, ...rows].join("\r\n")}\r\n`;
}

export function recsToJSON(recs: Rec[]): string {
  return `${JSON.stringify(recs, null, 2)}\n`;
}

/** `k8s-tailor-workloads-2026-10-21T1830.csv` */
export function exportName(ext: "csv" | "json", now = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  const stamp = `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}T${p(now.getHours())}${p(now.getMinutes())}`;
  return `k8s-tailor-workloads-${stamp}.${ext}`;
}

/** Browser download. Returns false when the environment refuses (sandboxed iframe). */
export function download(name: string, mime: string, text: string): boolean {
  try {
    const url = URL.createObjectURL(new Blob([text], { type: mime }));
    const a = document.createElement("a");
    a.href = url;
    a.download = name;
    a.rel = "noopener";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1_000);
    return true;
  } catch {
    return false;
  }
}
