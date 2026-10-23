import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { AuthGate } from "./components/auth";
import { McpProvider } from "./context.tsx";
import { consumeUrlToken } from "./lib/auth";
import { TooltipProvider } from "./ui/tooltip";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { staleTime: 5_000, retry: 1, refetchOnWindowFocus: false } },
});

// `#token=` (URL fragment, never sent to the server) is accepted once, kept for the
// session and removed from the address bar; a `?token=` query is ignored and stripped.
consumeUrlToken();

/** Providers every MCP-app entry mounts under: StrictMode → Query → MCP transport → auth. */
export function ToolShell({ children }: { children: React.ReactNode }) {
  return (
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <McpProvider>
            <AuthGate>{children}</AuthGate>
          </McpProvider>
        </TooltipProvider>
      </QueryClientProvider>
    </StrictMode>
  );
}
