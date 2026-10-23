import { AlertCircle, AlertTriangle, CheckCircle2, Info, type LucideIcon, X } from "lucide-react";
import * as React from "react";

import { cn } from "./lib/cn";

type BannerVariant = "success" | "warning" | "info" | "danger";

const VARIANTS: Record<BannerVariant, { surface: string; badge: string; icon: LucideIcon }> = {
  success: {
    surface: "bg-success-surface text-success-text",
    badge: "bg-success text-success-foreground",
    icon: CheckCircle2,
  },
  warning: {
    surface: "bg-warning-surface text-warning-text",
    badge: "bg-warning text-warning-foreground",
    icon: AlertTriangle,
  },
  info: {
    surface: "bg-info-surface text-info-text",
    badge: "bg-info text-info-foreground",
    icon: Info,
  },
  danger: {
    surface: "bg-destructive-surface text-destructive-text",
    badge: "bg-destructive text-destructive-foreground",
    icon: AlertCircle,
  },
};

interface BannerProps extends Omit<React.ComponentProps<"div">, "title"> {
  variant?: BannerVariant;
  title: React.ReactNode;
  description?: React.ReactNode;
  icon?: LucideIcon;
  action?: React.ReactNode;
  onDismiss?: () => void;
  dismissLabel?: string;
}

function Banner({
  variant = "info",
  title,
  description,
  icon: IconOverride,
  action,
  onDismiss,
  dismissLabel = "Dismiss",
  className,
  ...props
}: BannerProps) {
  const v = VARIANTS[variant];
  const Icon = IconOverride ?? v.icon;
  return (
    <div
      data-slot="banner"
      data-variant={variant}
      className={cn(
        "flex flex-wrap items-center gap-x-4 gap-y-3 rounded-xl px-5 py-4",
        v.surface,
        className,
      )}
      {...props}
    >
      <span
        className={cn("flex size-9 shrink-0 items-center justify-center rounded-full", v.badge)}
      >
        <Icon className="size-5" aria-hidden />
      </span>
      <div className="min-w-0 flex-1 basis-48">
        <p className="leading-snug font-semibold">{title}</p>
        {description && <p className="mt-0.5 text-sm leading-snug">{description}</p>}
      </div>
      {action && <div className="shrink-0">{action}</div>}
      {onDismiss && (
        <button
          type="button"
          data-slot="banner-dismiss"
          aria-label={dismissLabel}
          onClick={onDismiss}
          className="-my-1 -mr-2 flex size-10 shrink-0 items-center justify-center rounded-full outline-none transition-colors duration-(--duration-fast) hover:bg-accent focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring pointer-coarse:size-11"
        >
          <X className="size-4" aria-hidden />
        </button>
      )}
    </div>
  );
}

export { Banner };
export type { BannerProps, BannerVariant };
