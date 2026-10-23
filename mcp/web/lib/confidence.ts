import type { Rec } from "./types";

// Per-resource confidence: the engine actuates a resource only once it has enough samples
// for it. `confident` is the OR of the two; `cpuConfident` / `memConfident` say which one.
// An engine from before the split omits them: that is "unknown", NEVER "confident".

export type ResourceId = "cpu" | "mem";
export type Confidence = "confident" | "gathering" | "unknown";

export const RESOURCE_LABEL: Record<ResourceId, string> = { cpu: "CPU", mem: "Memory" };

export function resourceConfidence(
  r: Pick<Rec, "cpuConfident" | "memConfident">,
  res: ResourceId,
): Confidence {
  const v = res === "cpu" ? r.cpuConfident : r.memConfident;
  if (typeof v !== "boolean") return "unknown";
  return v ? "confident" : "gathering";
}

export const CONFIDENCE_LABEL: Record<Confidence, string> = {
  confident: "Confident",
  gathering: "Gathering data",
  unknown: "Not reported",
};

/** The one resource held for lack of samples while the other is confident, or null. */
export function heldResource(r: Pick<Rec, "cpuConfident" | "memConfident">): ResourceId | null {
  const cpu = resourceConfidence(r, "cpu");
  const mem = resourceConfidence(r, "mem");
  if (cpu === "gathering" && mem === "confident") return "cpu";
  if (mem === "gathering" && cpu === "confident") return "mem";
  return null;
}

/** "CPU held: not enough samples yet" — only when exactly one resource is held. */
export function heldHint(r: Pick<Rec, "cpuConfident" | "memConfident">): string | null {
  const h = heldResource(r);
  return h ? `${RESOURCE_LABEL[h]} held: not enough samples yet` : null;
}
