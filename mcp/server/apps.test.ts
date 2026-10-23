import { describe, expect, test } from "bun:test";
import { getToolUiResourceUri } from "@modelcontextprotocol/ext-apps/app-bridge";
import {
  EXTENSION_ID,
  RESOURCE_MIME_TYPE,
  RESOURCE_URI_META_KEY,
} from "@modelcontextprotocol/ext-apps/server";
import {
  LEGACY_RESOURCE_URI_META_KEY,
  buildToolList,
  overviewSummary,
  toolResult,
} from "./apps.ts";
import { DASHBOARD_RESOURCE_URI, dashboardUiResource } from "./resources.ts";
import { DASHBOARD_TOOL_ID, tools } from "./tools.ts";

describe("MCP Apps tool metadata", () => {
  const list = buildToolList(tools, DASHBOARD_TOOL_ID, DASHBOARD_RESOURCE_URI);
  const dash = list.find((t) => t.name === DASHBOARD_TOOL_ID) as Record<string, any>;

  test("the dashboard tool carries the nested AND the deprecated flat key", () => {
    expect(dash._meta.ui).toEqual({ resourceUri: "ui://k8s-tailor/dashboard" });
    expect(LEGACY_RESOURCE_URI_META_KEY).toBe(RESOURCE_URI_META_KEY);
    expect(dash._meta[RESOURCE_URI_META_KEY]).toBe("ui://k8s-tailor/dashboard");
  });

  test("the SDK's host-side resolver finds the URI with either form alone", () => {
    expect(getToolUiResourceUri(dash)).toBe(DASHBOARD_RESOURCE_URI);
    expect(getToolUiResourceUri({ _meta: { ui: dash._meta.ui } })).toBe(DASHBOARD_RESOURCE_URI);
    expect(
      getToolUiResourceUri({
        _meta: { [RESOURCE_URI_META_KEY]: dash._meta[RESOURCE_URI_META_KEY] },
      }),
    ).toBe(DASHBOARD_RESOURCE_URI);
  });

  test("only the dashboard tool advertises a UI; every tool keeps its schema", () => {
    for (const t of list) {
      if (t.name !== DASHBOARD_TOOL_ID) expect(t._meta).toBeUndefined();
      expect((t.inputSchema as { type?: string }).type).toBe("object");
    }
  });

  test("resource keeps the ui:// URI and the MCP Apps mime type", () => {
    expect(dashboardUiResource.uri).toBe("ui://k8s-tailor/dashboard");
    expect(dashboardUiResource.mimeType).toBe(RESOURCE_MIME_TYPE);
    expect(EXTENSION_ID).toBe("io.modelcontextprotocol/ui");
  });
});

describe("text fallback for hosts without MCP Apps", () => {
  const ov = {
    overview: {
      workloads: 12,
      over: 7,
      under: 2,
      costUsdWeek: 120.5,
      savingsUsdWeek: 30.25,
      wastedPct: 25.04,
      cpuReserved: 8,
      cpuRecommended: 5.5,
      memReserved: 16 * 1024 ** 3,
      memRecommended: 10 * 1024 ** 3,
      usage: [],
    },
    actuation: "active",
    automatedPct: 41.67,
    paused: false,
  };

  test("the UI tool leads with a readable summary, then the raw JSON", () => {
    const r = toolResult(DASHBOARD_TOOL_ID, DASHBOARD_TOOL_ID, ov);
    expect(r.structuredContent).toBe(ov);
    expect(r.content).toHaveLength(2);
    expect(r.content[0].text).toBe(
      "K8s Tailor overview: 12 workloads (7 over-provisioned, 2 under-provisioned); cost $120.50/week; reclaimable $30.25/week; 25.0% wasted; CPU 8.00 cores reserved vs 5.50 cores recommended; memory 16.00 GiB reserved vs 10.00 GiB recommended; 41.67% automated; actuation active.",
    );
    expect(JSON.parse(r.content[1].text)).toEqual(ov);
  });

  test("paused is called out; a payload without overview falls back to JSON only", () => {
    expect(overviewSummary({ ...ov, paused: true })).toContain("PAUSED");
    expect(overviewSummary({})).toBe("");
    expect(overviewSummary(null)).toBe("");
    expect(toolResult(DASHBOARD_TOOL_ID, DASHBOARD_TOOL_ID, { x: 1 }).content).toEqual([
      { type: "text", text: '{"x":1}' },
    ]);
  });

  test("other tools are unchanged: JSON text + structuredContent", () => {
    expect(toolResult("TAILOR_ECONOMY", DASHBOARD_TOOL_ID, { a: 1 })).toEqual({
      content: [{ type: "text", text: '{"a":1}' }],
      structuredContent: { a: 1 },
    });
  });
});
