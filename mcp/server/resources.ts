import { readFile } from "node:fs/promises";
import { join } from "node:path";

export const DASHBOARD_RESOURCE_URI = "ui://k8s-tailor/dashboard";

/**
 * The embedded dashboard, delivered THROUGH MCP as an app resource:
 * a self-contained HTML bundle with mimeType `text/html;profile=mcp-app`.
 * The host reads this resource, iframes the HTML, and proxies tool calls over
 * postMessage. A tool links to it via `_meta.ui.resourceUri` (MCP Apps, SEP-1865)
 * plus the deprecated flat `_meta["ui/resourceUri"]` for older hosts (apps.ts).
 */
export const dashboardUiResource = {
  uri: DASHBOARD_RESOURCE_URI,
  name: "K8s Tailor",
  description: "Pod rightsizing dashboard: recommendations, fit charts, decisions.",
  mimeType: "text/html;profile=mcp-app",
  read: async () => {
    const distPath = join(process.cwd(), "dist/client/web/entries/dashboard-ui.html");
    const html = await readFile(distPath, "utf-8");
    return { uri: DASHBOARD_RESOURCE_URI, mimeType: "text/html;profile=mcp-app", text: html };
  },
};

export const resources = [dashboardUiResource];
