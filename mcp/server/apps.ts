import { z } from "zod";
import type { McpTool } from "./tools.ts";

/**
 * MCP Apps (extension `io.modelcontextprotocol/ui`, SEP-1865) glue.
 *
 * A tool is linked to its UI resource through `_meta.ui.resourceUri` (the current
 * form). The flat `_meta["ui/resourceUri"]` key is deprecated in the spec but still
 * read by older hosts, so both are emitted with the same URI — exactly what
 * `registerAppTool` of @modelcontextprotocol/ext-apps does (RESOURCE_URI_META_KEY).
 * The server does not declare the extension in `initialize`: the spec has the CLIENT
 * advertise `capabilities.extensions["io.modelcontextprotocol/ui"]`; hosts that
 * do not support it ignore `_meta` and get a text-only result (see toolResult).
 */

/** Deprecated flat key (ext-apps RESOURCE_URI_META_KEY), kept for older hosts. */
export const LEGACY_RESOURCE_URI_META_KEY = "ui/resourceUri";

export function uiToolMeta(resourceUri: string): Record<string, unknown> {
  return { ui: { resourceUri }, [LEGACY_RESOURCE_URI_META_KEY]: resourceUri };
}

/** tools/list entries; the tool named `uiToolName` advertises the UI resource. */
export function buildToolList(tools: McpTool[], uiToolName: string, resourceUri: string) {
  return tools.map((t) => {
    const entry: Record<string, unknown> = {
      name: t.name,
      description: t.description,
      inputSchema: z.toJSONSchema(t.inputSchema),
      annotations: { readOnlyHint: t.readOnly ?? false },
    };
    if (t.name === uiToolName) entry._meta = uiToolMeta(resourceUri);
    return entry;
  });
}

const num = (v: unknown): number | undefined =>
  typeof v === "number" && Number.isFinite(v) ? v : undefined;
const usd = (v: number) => `$${v.toFixed(2)}`;
const cores = (v: number) => `${v.toFixed(2)} cores`;
const gib = (v: number) => `${(v / 1024 ** 3).toFixed(2)} GiB`;

/**
 * One-paragraph, human-readable summary of TAILOR_OVERVIEW, for hosts that render
 * no UI (and for the model). Returns "" when the payload has no overview block.
 */
export function overviewSummary(out: unknown): string {
  const o = (out as { overview?: Record<string, unknown> } | null)?.overview;
  if (!o || typeof o !== "object") return "";
  const top = out as Record<string, unknown>;
  const parts: string[] = [];
  const w = num(o.workloads);
  if (w !== undefined) {
    const over = num(o.over) ?? 0;
    const under = num(o.under) ?? 0;
    parts.push(`${w} workloads (${over} over-provisioned, ${under} under-provisioned)`);
  }
  const cost = num(o.costUsdWeek);
  const save = num(o.savingsUsdWeek);
  if (cost !== undefined) parts.push(`cost ${usd(cost)}/week`);
  if (save !== undefined) parts.push(`reclaimable ${usd(save)}/week`);
  const wasted = num(o.wastedPct);
  if (wasted !== undefined) parts.push(`${wasted.toFixed(1)}% wasted`);
  const cr = num(o.cpuReserved);
  const crec = num(o.cpuRecommended);
  if (cr !== undefined && crec !== undefined)
    parts.push(`CPU ${cores(cr)} reserved vs ${cores(crec)} recommended`);
  const mr = num(o.memReserved);
  const mrec = num(o.memRecommended);
  if (mr !== undefined && mrec !== undefined)
    parts.push(`memory ${gib(mr)} reserved vs ${gib(mrec)} recommended`);
  const auto = num(top.automatedPct);
  if (auto !== undefined) parts.push(`${auto}% automated`);
  if (typeof top.actuation === "string") parts.push(`actuation ${top.actuation}`);
  if (top.paused === true) parts.push("PAUSED (global kill switch on)");
  return parts.length ? `K8s Tailor overview: ${parts.join("; ")}.` : "";
}

/**
 * tools/call result. `structuredContent` feeds the dashboard; `content` must be
 * meaningful on its own (spec: hosts without MCP Apps support show only it), so the
 * UI tool leads with a readable summary before the raw JSON.
 */
export function toolResult(name: string, uiToolName: string, out: unknown) {
  const content: { type: "text"; text: string }[] = [];
  if (name === uiToolName) {
    const s = overviewSummary(out);
    if (s) content.push({ type: "text", text: s });
  }
  content.push({ type: "text", text: JSON.stringify(out) });
  return { content, structuredContent: out };
}
