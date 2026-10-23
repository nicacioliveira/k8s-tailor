import * as React from "react";

import {
  EmptyChartIllustration,
  EmptyInboxIllustration,
  EmptyListIllustration,
  EmptySearchIllustration,
} from "./illustrations";
import { cn } from "./lib/cn";

type EmptyIllustration = "inbox" | "list" | "search" | "chart";

const ILLUSTRATIONS: Record<EmptyIllustration, React.ComponentType<{ size?: number }>> = {
  inbox: EmptyInboxIllustration,
  list: EmptyListIllustration,
  search: EmptySearchIllustration,
  chart: EmptyChartIllustration,
};

interface EmptyStateProps {
  title: string;
  description?: React.ReactNode;
  illustration?: EmptyIllustration;
  icon?: React.ReactNode;
  action?: React.ReactNode;
  secondaryAction?: React.ReactNode;
  as?: "h2" | "h3" | "h4";
  size?: "default" | "sm";
  className?: string;
}

function EmptyState({
  title,
  description,
  illustration,
  icon,
  action,
  secondaryAction,
  as: Heading = "h3",
  size = "default",
  className,
}: EmptyStateProps) {
  const Illustration = illustration ? ILLUSTRATIONS[illustration] : null;
  const small = size === "sm";

  return (
    <div
      data-slot="empty-state"
      className={cn(
        "flex flex-col items-center justify-center px-6 text-center",
        small ? "py-8" : "py-12",
        className,
      )}
    >
      {Illustration ? (
        <div className="mb-4 text-muted-foreground">
          <Illustration size={small ? 72 : 96} />
        </div>
      ) : (
        icon && (
          <div className="mb-4 flex size-12 items-center justify-center rounded-xl bg-muted text-muted-foreground [&_svg:not([class*='size-'])]:size-5">
            {icon}
          </div>
        )
      )}
      <Heading className="text-base font-semibold tracking-tight text-foreground">{title}</Heading>
      {description && (
        <p className="mt-1.5 max-w-sm text-sm leading-relaxed text-pretty text-muted-foreground">
          {description}
        </p>
      )}
      {(action || secondaryAction) && (
        <div
          className={cn(
            "flex flex-wrap items-center justify-center gap-2",
            small ? "mt-4" : "mt-5",
          )}
        >
          {action}
          {secondaryAction}
        </div>
      )}
    </div>
  );
}

export { EmptyState };
export type { EmptyStateProps, EmptyIllustration };
