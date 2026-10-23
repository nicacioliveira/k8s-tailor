import { buildToolList, toolResult } from "./apps.ts";
import { type GateConfig, configFromEnv, serve } from "./http.ts";
import { engine } from "./engine.ts";
import { DASHBOARD_RESOURCE_URI, dashboardUiResource, resources } from "./resources.ts";
import { DASHBOARD_TOOL_ID, tools } from "./tools.ts";

const PORT = Number(process.env.PORT ?? 8020);
// Bind address. The chart sets 127.0.0.1 when the nginx auth sidecar fronts this
// process, so nothing else in the cluster can reach this port directly.
// Default: all interfaces in a pod, loopback for a local `bun dev`.
const HOST =
  process.env.MCP_HOST ?? (process.env.KUBERNETES_SERVICE_HOST ? "0.0.0.0" : "127.0.0.1");
const PROTOCOL_VERSION = "2025-06-18";

// Fail closed: /mcp forwards TAILOR_API_TOKEN on every engine call, so it ALWAYS
// requires MCP_API_TOKEN — also behind the nginx sidecar (defense in depth: the
// chart gives both the same token). Only MCP_ALLOW_UNAUTHENTICATED=true (dev)
// opens it.
let gate: GateConfig;
try {
  gate = configFromEnv(process.env);
} catch (e) {
  console.error(String((e as Error).message ?? e));
  process.exit(1);
}

const toolByName = new Map(tools.map((t) => [t.name, t]));

// tools/list payload, computed once. The dashboard tool advertises its UI
// (`_meta.ui.resourceUri` + the deprecated flat key, see apps.ts) so an MCP Apps
// host reads the ui:// resource and iframes it.
const toolList = buildToolList(tools, DASHBOARD_TOOL_ID, DASHBOARD_RESOURCE_URI);

function rpc(id: unknown, result: unknown) {
  return { jsonrpc: "2.0", id, result };
}
function rpcError(id: unknown, code: number, message: string) {
  return { jsonrpc: "2.0", id, error: { code, message } };
}

async function handleRpc(msg: any): Promise<unknown | null> {
  const { id, method, params } = msg ?? {};
  switch (method) {
    case "initialize":
      return rpc(id, {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: { tools: {}, resources: {} },
        serverInfo: { name: "k8s-tailor", version: "0.1.0" },
      });
    case "notifications/initialized":
      return null; // notification — no response
    case "ping":
      return rpc(id, {});
    case "tools/list":
      return rpc(id, { tools: toolList });
    case "tools/call": {
      const tool = toolByName.get(params?.name);
      if (!tool) return rpcError(id, -32602, `unknown tool: ${params?.name}`);
      try {
        const args = tool.inputSchema.parse(params?.arguments ?? {});
        const out = await tool.execute(args);
        return rpc(id, toolResult(tool.name, DASHBOARD_TOOL_ID, out));
      } catch (e) {
        return rpc(id, { content: [{ type: "text", text: String(e) }], isError: true });
      }
    }
    case "resources/list":
      return rpc(id, {
        resources: resources.map((r) => ({
          uri: r.uri,
          name: r.name,
          description: r.description,
          mimeType: r.mimeType,
        })),
      });
    case "resources/read": {
      if (params?.uri === DASHBOARD_RESOURCE_URI) {
        return rpc(id, { contents: [await dashboardUiResource.read()] });
      }
      return rpcError(id, -32602, `unknown resource: ${params?.uri}`);
    }
    default:
      return rpcError(id, -32601, `method not found: ${method}`);
  }
}

serve(
  gate,
  {
    rpc: handleRpc,
    ready: () => engine.healthy(),
    // Local standalone page (an MCP host uses the MCP resource instead).
    ui: async () => (await dashboardUiResource.read()).text,
  },
  HOST,
  PORT,
);

console.log(
  `k8s-tailor-mcp on ${HOST}:${PORT} (engine=${engine.base}, engine-token=${engine.hasToken ? "set" : "unset"}, auth=${gate.token ? "token" : "OPEN (dev)"})`,
);
