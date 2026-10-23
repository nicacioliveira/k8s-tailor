import { AlertCircle, RefreshCw } from "lucide-react";
import * as React from "react";

import { cn } from "./lib/cn";

type SystemStatus = "online" | "offline" | "syncing" | "warning" | "error";

const DEFAULT_LABEL: Record<SystemStatus, string> = {
  online: "Online",
  offline: "Offline",
  syncing: "Syncing",
  warning: "Partial",
  error: "Error",
};

interface StatusIndicatorProps extends React.ComponentProps<"span"> {
  status: SystemStatus;
  label?: React.ReactNode;
  description?: React.ReactNode;
  size?: "sm" | "default";
}

function StatusIndicator({
  status,
  label,
  description,
  size = "default",
  className,
  ...props
}: StatusIndicatorProps) {
  const sm = size === "sm";
  const icon = sm ? "size-3" : "size-3.5";
  return (
    <span
      data-slot="status-indicator"
      data-status={status}
      className={cn("inline-flex items-center gap-2", sm ? "text-xs" : "text-sm", className)}
      {...props}
    >
      <span className={cn("flex shrink-0 items-center justify-center", icon)} aria-hidden>
        {status === "online" && (
          <span className="size-2 rounded-full bg-success ring-3 ring-success-surface" />
        )}
        {status === "offline" && (
          <span className="size-2 rounded-full border-2 border-muted-foreground" />
        )}
        {status === "warning" && (
          <span
            className={cn(
              "relative overflow-hidden rounded-full border-2 border-warning-text",
              sm ? "size-2.5" : "size-3",
            )}
          >
            <span className="absolute inset-y-0 left-0 w-1/2 bg-warning-text" />
          </span>
        )}
        {status === "syncing" && (
          <RefreshCw
            className={cn(
              icon,
              "animate-spin text-info-text [animation-duration:var(--duration-shimmer)]",
            )}
          />
        )}
        {status === "error" && <AlertCircle className={cn(icon, "text-destructive-text")} />}
      </span>
      <span className="font-medium text-foreground">
        {label ?? DEFAULT_LABEL[status]}
        {description && <span className="font-normal text-muted-foreground"> · {description}</span>}
      </span>
    </span>
  );
}

export { StatusIndicator };
export type { StatusIndicatorProps, SystemStatus };
