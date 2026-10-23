import { Minus, TrendingDown, TrendingUp } from "lucide-react";
import * as React from "react";

import { cn } from "./lib/cn";
import { formatDelta } from "./lib/format";
import { Skeleton } from "./skeleton";
import { Sparkline } from "./sparkline";

type KpiTint = "neutral" | "emerald" | "blue" | "amber" | "red" | "violet";

interface KpiCardProps {
  label: string;
  value: string;
  unit?: string;
  unavailable?: boolean;
  trend?: number[];
  delta?: number;
  deltaLabel?: string;
  icon?: React.ReactNode;
  badge?: React.ReactNode;
  tint?: KpiTint;
  polarity?: "up-is-good" | "down-is-good" | "neutral";
  loading?: boolean;
  className?: string;
}

const TINT: Record<KpiTint, string> = {
  neutral: "text-muted-foreground",
  emerald: "text-success",
  blue: "text-primary",
  amber: "text-warning",
  red: "text-destructive",
  violet: "text-chart-4",
};

function KpiCard({
  label,
  value,
  unit,
  unavailable = false,
  trend,
  delta,
  deltaLabel,
  icon,
  badge,
  tint = "neutral",
  polarity = "up-is-good",
  loading,
  className,
}: KpiCardProps) {
  const direction =
    unavailable || delta == null ? null : delta > 0 ? "up" : delta < 0 ? "down" : "flat";
  const good =
    polarity === "neutral" || direction === "flat"
      ? null
      : (direction === "up") === (polarity === "up-is-good");
  const DeltaIcon = direction === "up" ? TrendingUp : direction === "down" ? TrendingDown : Minus;

  return (
    <div
      data-slot="kpi-card"
      className={cn(
        "group @container relative flex flex-col rounded-xl border border-border bg-card p-5 text-card-foreground",
        "transition-[border-color,box-shadow] duration-(--duration-base) hover:border-border hover:shadow-raised",
        className,
      )}
    >
      <div className="mb-3 flex min-w-0 flex-1 flex-col items-start gap-2 @min-[13rem]:flex-row">
        {icon && (
          <div className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground [&_svg]:size-4">
            {icon}
          </div>
        )}
        <span
          className={cn(
            "min-w-0 text-sm leading-5 font-medium text-pretty wrap-break-word text-muted-foreground",
            icon && "@min-[13rem]:pt-1",
          )}
        >
          {label}
        </span>
      </div>

      <div className="mt-auto flex items-end justify-between gap-3">
        <div className="min-w-0 flex-1">
          {loading ? (
            <div role="status" aria-busy aria-label={`Loading ${label}`}>
              <Skeleton className="h-7 w-32" />
              <Skeleton className="mt-2 h-3 w-20" />
            </div>
          ) : (
            <>
              <p
                data-money
                className={cn(
                  "text-[clamp(1.05rem,6.5cqw,1.5rem)] leading-tight font-semibold tracking-tight whitespace-nowrap tabular-nums",
                  unavailable && "text-muted-foreground",
                )}
              >
                {unavailable ? (
                  <>
                    <span aria-hidden>—</span>
                    <span className="sr-only">unavailable</span>
                  </>
                ) : (
                  <>
                    {value}
                    {unit ? (
                      <span className="ml-1 text-sm font-medium text-muted-foreground">{unit}</span>
                    ) : null}
                  </>
                )}
              </p>
              {!unavailable && (direction || deltaLabel || badge) && (
                <div className="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs">
                  {direction && delta != null && (
                    <span
                      className={cn(
                        "inline-flex items-center gap-0.5 font-medium tabular-nums",
                        good === true && "text-success",
                        good === false && "text-destructive",
                        good === null && "text-muted-foreground",
                      )}
                    >
                      <DeltaIcon className="size-3" aria-hidden />
                      {formatDelta(delta)}
                    </span>
                  )}
                  {deltaLabel && <span className="text-muted-foreground">{deltaLabel}</span>}
                  {badge}
                </div>
              )}
            </>
          )}
        </div>
        {!loading && !unavailable && trend && trend.length >= 2 && (
          <div
            className={cn(
              // Too narrow for both (2-up grid at 360px): the sparkline would sit on the label.
              "shrink-0 opacity-80 transition-opacity group-hover:opacity-100 @max-[8rem]:hidden",
              TINT[tint],
            )}
          >
            <Sparkline data={trend} width={72} height={28} fill={false} />
          </div>
        )}
      </div>
    </div>
  );
}

export { KpiCard };
export type { KpiCardProps, KpiTint };
