import { useQuery } from "@tanstack/react-query";
import { useMcpApp } from "./context.tsx";
import type { ToolCaller } from "./context.tsx";

function extractToolError(result: any): string | null {
  if (result?.isError && Array.isArray(result.content)) {
    return result.content.find((c: any) => c.type === "text")?.text ?? "tool error";
  }
  return null;
}

/** Read a tool as a query. Returns its structuredContent. */
export function useTool<T>(
  name: string,
  args: Record<string, unknown> = {},
  opts: { refetchInterval?: number; enabled?: boolean } = {},
) {
  const app = useMcpApp();
  return useQuery<T | null>({
    queryKey: [name, args],
    enabled: !!app && (opts.enabled ?? true),
    refetchInterval: opts.refetchInterval,
    queryFn: async () => {
      const result: any = await app!.callServerTool({ name, arguments: args });
      const err = extractToolError(result);
      if (err) throw new Error(err);
      return (result.structuredContent ?? null) as T | null;
    },
  });
}

/** Imperative tool call for actions/mutations. */
export async function callTool<T = any>(
  app: ToolCaller | null,
  name: string,
  args: Record<string, unknown> = {},
): Promise<T> {
  if (!app) throw new Error("MCP app not connected");
  const result: any = await app.callServerTool({ name, arguments: args });
  const err = extractToolError(result);
  if (err) throw new Error(err);
  return (result.structuredContent ?? null) as T;
}
