import * as SliderPrimitive from "@radix-ui/react-slider";
import * as React from "react";

import { cn } from "./lib/cn";
import { formatNumber } from "./lib/format";

type SliderProps = Omit<React.ComponentProps<typeof SliderPrimitive.Root>, "children"> & {
  label?: React.ReactNode;
  formatValue?: (valor: number) => string;
  hideValue?: boolean;
  thumbLabels?: string[];
  marks?: Array<{ value: number; label?: string }>;
};

function Slider({
  className,
  label,
  formatValue = (v) => formatNumber(v),
  hideValue = false,
  thumbLabels,
  marks,
  value,
  defaultValue,
  onValueChange,
  min = 0,
  max = 100,
  id,
  "aria-label": ariaLabel,
  "aria-describedby": describedBy,
  "aria-invalid": ariaInvalid,
  ...props
}: SliderProps) {
  const [internal, setInternal] = React.useState<number[]>(defaultValue ?? [min]);
  const current = value ?? internal;
  const autoId = React.useId();
  const base = id ?? autoId;
  const labelId = `${base}-label`;
  const isRange = current.length > 1;
  const valueText = current.map(formatValue).join(" – ");
  const pct = (v: number) => ((v - min) / (max - min)) * 100;

  return (
    <div data-slot="slider-field" className={cn("grid w-full gap-3", className)}>
      {label || !hideValue ? (
        <div className="flex items-baseline justify-between gap-3">
          {label ? (
            <span id={labelId} className="text-sm leading-none font-medium">
              {label}
            </span>
          ) : (
            <span />
          )}
          {!hideValue ? (
            <output htmlFor={base} aria-hidden className="text-sm font-medium tabular-nums">
              {valueText}
            </output>
          ) : null}
        </div>
      ) : null}
      <SliderPrimitive.Root
        data-slot="slider"
        id={base}
        value={value}
        defaultValue={value ? undefined : internal}
        onValueChange={(v) => {
          setInternal(v);
          onValueChange?.(v);
        }}
        min={min}
        max={max}
        className={cn(
          "group/slider relative flex w-full touch-none items-center select-none",
          "h-5 pointer-coarse:h-control-touch",
          "data-[disabled]:cursor-not-allowed data-[disabled]:opacity-50",
        )}
        {...props}
      >
        <SliderPrimitive.Track
          data-slot="slider-track"
          className="relative h-1.5 w-full grow overflow-hidden rounded-full bg-neutral-6"
        >
          <SliderPrimitive.Range data-slot="slider-range" className="absolute h-full bg-primary" />
        </SliderPrimitive.Track>
        {current.map((v, i) => (
          <SliderPrimitive.Thumb
            key={i}
            data-slot="slider-thumb"
            aria-label={thumbLabels?.[i] ?? (label ? undefined : ariaLabel)}
            aria-labelledby={!thumbLabels?.[i] && label ? labelId : undefined}
            aria-valuetext={
              isRange ? `${thumbLabels?.[i] ?? ""} ${formatValue(v)}`.trim() : formatValue(v)
            }
            aria-describedby={describedBy}
            aria-invalid={ariaInvalid}
            className={cn(
              "relative block size-5 shrink-0 rounded-full border-2 border-primary bg-background shadow-card",
              "transition-[box-shadow,transform] duration-(--duration-fast) hover:shadow-raised",
              "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
              "aria-invalid:border-destructive",
              "data-[disabled]:pointer-events-none",
              "pointer-coarse:after:absolute pointer-coarse:after:-inset-3 pointer-coarse:after:content-['']",
            )}
          />
        ))}
      </SliderPrimitive.Root>
      {marks?.length ? (
        <div aria-hidden className="relative -mt-1 h-5 text-xs text-muted-foreground tabular-nums">
          {marks.map((m) => {
            const p = pct(m.value);
            return (
              <span
                key={m.value}
                className={cn(
                  "absolute top-0 whitespace-nowrap",
                  p <= 0 ? "" : p >= 100 ? "-translate-x-full" : "-translate-x-1/2",
                )}
                style={{ left: `${p}%` }}
              >
                {m.label ?? formatValue(m.value)}
              </span>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}

export { Slider };
export type { SliderProps };
