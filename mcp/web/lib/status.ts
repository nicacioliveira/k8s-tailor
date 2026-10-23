import type { Rec } from "./types";

// At-a-glance verdict for one workload container. Always rendered as a word; the
// tint only reinforces it (see components/workload-badges.tsx).
export const STATUSES = [
  { id: "oom", label: "OOM risk" },
  { id: "limit", label: "Needs higher limit" },
  { id: "gathering", label: "Gathering data" },
  { id: "over", label: "Over-provisioned" },
  { id: "under", label: "Under-provisioned" },
  { id: "ok", label: "Right-sized" },
] as const;
export type StatusId = (typeof STATUSES)[number]["id"];

export function statusId(r: Rec): StatusId {
  if (r.oomRisk) return "oom";
  if (r.limitCapped) return "limit";
  if (!r.confident) return "gathering";
  if (r.wasteUsdWeek > 0.02) return "over";
  if (r.wasteUsdWeek < -0.02) return "under";
  return "ok";
}

export const statusLabel = (id: StatusId) => STATUSES.find((s) => s.id === id)?.label ?? id;
