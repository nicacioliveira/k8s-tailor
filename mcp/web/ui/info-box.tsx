import { AlertCircle, AlertTriangle, CheckCircle2, Info, type LucideIcon } from "lucide-react";
import * as React from "react";

import { cn } from "./lib/cn";
import { type IconProp, renderIcon } from "./lib/icon";

type InfoBoxTone = "default" | "muted" | "info" | "success" | "warning" | "danger";

const TONE: Record<InfoBoxTone, { box: string; icon: LucideIcon | null }> = {
  default: { box: "border-border bg-card text-card-foreground", icon: null },
  muted: { box: "border-transparent bg-muted text-foreground", icon: null },
  info: { box: "border-transparent bg-info-surface text-info-text", icon: Info },
  success: { box: "border-transparent bg-success-surface text-success-text", icon: CheckCircle2 },
  warning: { box: "border-transparent bg-warning-surface text-warning-text", icon: AlertTriangle },
  danger: {
    box: "border-transparent bg-destructive-surface text-destructive-text",
    icon: AlertCircle,
  },
};

interface InfoBoxProps extends Omit<React.ComponentProps<"div">, "title"> {
  tone?: InfoBoxTone;
  title?: React.ReactNode;
  icon?: IconProp | false;
  action?: React.ReactNode;
}

function InfoBox({
  className,
  tone = "default",
  title,
  icon,
  action,
  children,
  ...props
}: InfoBoxProps) {
  const t = TONE[tone];
  const Icon = icon === false ? null : (icon ?? t.icon);
  return (
    <div
      data-slot="info-box"
      data-tone={tone}
      className={cn(
        "flex gap-3 rounded-xl border px-4 py-3 text-sm leading-relaxed",
        t.box,
        className,
      )}
      {...props}
    >
      {renderIcon(Icon, "mt-0.5 size-4 shrink-0")}
      <div className="min-w-0 flex-1">
        {title && <p className="font-semibold">{title}</p>}
        {children}
        {action && (
          <div data-slot="info-box-action" className="mt-3 flex flex-wrap gap-2">
            {action}
          </div>
        )}
      </div>
    </div>
  );
}

export { InfoBox };
export type { InfoBoxProps, InfoBoxTone };
