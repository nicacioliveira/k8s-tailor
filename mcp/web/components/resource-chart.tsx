import { Area, ComposedChart, Line, ReferenceLine } from "recharts";
import { dayHhmm, fullDate, hhmm } from "../lib/fmt";
import type { FitPoint } from "../lib/series";
import {
  type ChartConfig,
  ChartContainer,
  ChartGrid,
  ChartReferenceLine,
  ChartTooltip,
  ChartXAxis,
  ChartYAxis,
} from "../ui/chart";
import { formatNumber, formatUSD } from "../ui/lib/format";

export type Layer = { key: string; kind: "stack" | "area" | "line" };
export type RefLine = { value: number; label: string; dash?: string; position?: "start" | "end" };

// Fixed series vocabulary, same colour everywhere (chart-1..5, max 4 series per chart):
//   usage = 1, safety margin = 5, reclaimable waste = 4, recommended request = 2.
export const FIT_CONFIG: ChartConfig = {
  used: { label: "Usage", color: 1 },
  margin: { label: "Safety margin", color: 5 },
  waste: { label: "Reclaimable waste", color: 4 },
  fit: { label: "Recommended request", color: 2 },
};
export const FIT_LAYERS: Layer[] = [
  { key: "used", kind: "stack" },
  { key: "margin", kind: "stack" },
  { key: "waste", kind: "stack" },
  { key: "fit", kind: "line" },
];

const FILL: Record<string, number> = { used: 0.28, margin: 0.22, waste: 0.4 };

export function ResourceChart({
  data,
  unit,
  summary,
  config,
  layers,
  refs = [],
  events = [],
  loading,
  className,
}: {
  data: FitPoint[];
  unit: "m" | "Mi" | "$";
  summary: string;
  config: ChartConfig;
  layers: Layer[];
  refs?: RefLine[];
  events?: number[];
  loading?: boolean;
  className?: string;
}) {
  const span = data.length > 1 ? data[data.length - 1].t - data[0].t : 0;
  const tick = span > 20 * 3600 ? dayHhmm : hhmm;
  const fmt = (v: number) =>
    unit === "$" ? formatUSD(v) : `${formatNumber(Math.round(v))}${unit}`;
  const present = layers.filter((l) => data.some((p) => p[l.key] != null));
  return (
    <ChartContainer
      config={config}
      data={data}
      categoryKey="t"
      categoryLabel="Time"
      summary={summary}
      valueFormat={fmt}
      formatCategory={(v) => (typeof v === "number" ? fullDate(v) : String(v ?? ""))}
      series={present.map((l) => l.key)}
      loading={loading}
      chartClassName="h-64"
      className={className}
    >
      <ComposedChart data={data} margin={{ top: 16, right: 8, left: 0, bottom: 0 }}>
        <ChartGrid />
        <ChartXAxis
          type="number"
          scale="time"
          domain={["dataMin", "dataMax"]}
          tickFormatter={(v: number) => tick(v)}
          minTickGap={48}
        />
        <ChartYAxis domain={[0, "auto"]} />
        <ChartTooltip />
        {present.map((l) =>
          l.kind === "line" ? (
            <Line
              key={l.key}
              type="monotone"
              dataKey={l.key}
              stroke={`var(--color-${l.key})`}
              strokeWidth={2}
              dot={false}
              isAnimationActive={false}
              connectNulls
            />
          ) : (
            <Area
              key={l.key}
              type="monotone"
              dataKey={l.key}
              stackId={l.kind === "stack" ? "a" : undefined}
              stroke={l.key === "used" || l.kind === "area" ? `var(--color-${l.key})` : "none"}
              strokeWidth={2}
              fill={`var(--color-${l.key})`}
              fillOpacity={FILL[l.key] ?? 0.08}
              isAnimationActive={false}
              dot={false}
              connectNulls
            />
          ),
        )}
        {refs.map((r) => (
          <ChartReferenceLine
            key={r.label}
            value={r.value}
            label={r.label}
            labelPosition={r.position ?? "end"}
            strokeDasharray={r.dash ?? "6 4"}
          />
        ))}
        {events.map((t, i) => (
          <ReferenceLine
            key={`${t}-${i}`}
            x={t}
            stroke="var(--muted-foreground)"
            strokeDasharray="2 3"
            label={{
              value: "resized",
              position: "insideTopLeft",
              fill: "var(--muted-foreground)",
              fontSize: 11,
            }}
          />
        ))}
      </ComposedChart>
    </ChartContainer>
  );
}
