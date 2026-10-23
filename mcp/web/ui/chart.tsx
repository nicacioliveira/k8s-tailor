import { ChartColumn, Table2 } from "lucide-react";
import * as React from "react";
import * as Recharts from "recharts";
import type { TooltipContentProps } from "recharts";

import { Button } from "./button";
import { EmptyState } from "./empty-state";
import { cn } from "./lib/cn";
import { formatNumber, formatUSD } from "./lib/format";
import { Skeleton } from "./skeleton";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "./table";

type ChartColor = 1 | 2 | 3 | 4 | 5;
type ChartValueFormat = "usd" | "percent" | "number" | ((value: number) => string);

interface ChartSeriesConfig {
  label: string;
  color?: ChartColor;
  format?: ChartValueFormat;
}

type ChartConfig = Record<string, ChartSeriesConfig>;

const COLOR_VAR: Record<ChartColor, string> = {
  1: "var(--chart-1)",
  2: "var(--chart-2)",
  3: "var(--chart-3)",
  4: "var(--chart-4)",
  5: "var(--chart-5)",
};

const percent = new Intl.NumberFormat("en-US", { style: "percent", maximumFractionDigits: 1 });
const compactNumber = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});
const compactUSD = new Intl.NumberFormat("en-US", {
  notation: "compact",
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 1,
});
const minus = (s: string) => s.replace(/^-/, "\u2212");

function formatChartValue(value: number, format: ChartValueFormat = "number"): string {
  if (typeof format === "function") return format(value);
  switch (format) {
    case "usd":
      return formatUSD(value);
    case "percent":
      return minus(percent.format(value / 100));
    default:
      return formatNumber(value);
  }
}

function formatAxisValue(value: number, format: ChartValueFormat = "number"): string {
  if (typeof format === "function") return format(value);
  switch (format) {
    case "usd":
      return minus(compactUSD.format(value));
    case "percent":
      return minus(percent.format(value / 100));
    default:
      return minus(compactNumber.format(value));
  }
}

type Row = Record<string, unknown>;

interface ChartContextValue {
  config: ChartConfig;
  data: readonly Row[];
  categoryKey: string;
  series: string[];
  categorical: boolean;
  formatValue: (key: string, value: number) => string;
  formatCategory: (value: unknown) => string;
  valueFormat: ChartValueFormat;
  total: number;
}

const ChartContext = React.createContext<ChartContextValue | null>(null);

function useChart() {
  const ctx = React.useContext(ChartContext);
  if (!ctx) throw new Error("Chart parts must be used inside <ChartContainer>.");
  return ctx;
}

interface ChartContainerProps extends Omit<React.ComponentProps<"figure">, "children"> {
  config: ChartConfig;
  data: readonly Row[];
  categoryKey: string;
  categoryLabel: string;
  summary: string;
  valueFormat?: ChartValueFormat;
  formatCategory?: (value: unknown) => string;
  series?: string[];
  legend?: boolean;
  tableTotal?: boolean;
  center?: React.ReactNode;
  loading?: boolean;
  empty?: React.ReactNode;
  chartClassName?: string;
  children: React.ReactElement<{ accessibilityLayer?: boolean }>;
}

function ChartContainer({
  config,
  data,
  categoryKey,
  categoryLabel,
  summary,
  valueFormat = "number",
  formatCategory: formatCategoryProp,
  series: seriesProp,
  legend,
  tableTotal,
  center,
  loading = false,
  empty,
  chartClassName,
  className,
  style,
  children,
  ...props
}: ChartContainerProps) {
  const [view, setView] = React.useState<"chart" | "table">("chart");
  const tableId = React.useId();

  const first = data[0] ?? {};
  const series = seriesProp ?? Object.keys(config).filter((k) => k in first && k !== categoryKey);
  const categorical = data.some((row) => {
    const c = row[categoryKey];
    return typeof c === "string" && config[c]?.color != null;
  });

  const formatCategory = React.useCallback(
    (v: unknown) => {
      if (formatCategoryProp) return formatCategoryProp(v);
      if (typeof v === "string" && config[v]) return config[v].label;
      return String(v ?? "");
    },
    [formatCategoryProp, config],
  );
  const formatValue = React.useCallback(
    (key: string, v: number) => formatChartValue(v, config[key]?.format ?? valueFormat),
    [config, valueFormat],
  );
  const firstSeries = series[0];
  const total =
    categorical && firstSeries ? data.reduce((s, r) => s + (Number(r[firstSeries]) || 0), 0) : 0;

  const colorVars = Object.fromEntries(
    Object.entries(config).flatMap(([k, c]) =>
      c.color ? [[`--color-${k}`, COLOR_VAR[c.color]]] : [],
    ),
  ) as React.CSSProperties;

  const legendKeys = categorical
    ? data.map((r) => String(r[categoryKey])).filter((k) => config[k]?.color != null)
    : series.filter((k) => config[k]?.color != null);
  const showLegend = legend ?? legendKeys.length > 1;
  const ctx: ChartContextValue = {
    config,
    data,
    categoryKey,
    series,
    categorical,
    formatValue,
    formatCategory,
    valueFormat,
    total,
  };
  const isEmpty = !loading && data.length === 0;

  return (
    <ChartContext.Provider value={ctx}>
      <figure
        data-slot="chart"
        className={cn("flex min-w-0 flex-col gap-3", className)}
        style={{ ...colorVars, ...style }}
        {...props}
      >
        {!loading && !isEmpty && (
          <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
            {showLegend && view === "chart" ? <ChartLegend keys={legendKeys} /> : <span />}
            <Button
              variant="ghost"
              size="sm"
              className="-mr-2 ml-auto text-muted-foreground"
              aria-expanded={view === "table"}
              aria-controls={view === "table" ? tableId : undefined}
              onClick={() => setView((v) => (v === "chart" ? "table" : "chart"))}
            >
              {view === "chart" ? <Table2 aria-hidden /> : <ChartColumn aria-hidden />}
              {view === "chart" ? "View as table" : "View chart"}
            </Button>
          </div>
        )}

        {loading ? (
          <div
            role="status"
            aria-label="Loading chart"
            className={cn("relative flex items-end gap-2 px-2", chartClassName ?? "h-72")}
          >
            {[48, 64, 40, 72, 56, 80, 60, 88, 68, 52, 76, 64].map((h, i) => (
              <Skeleton
                key={i}
                className="flex-1 rounded-t-md rounded-b-none"
                style={{ height: `${h}%` }}
              />
            ))}
          </div>
        ) : isEmpty ? (
          (empty ?? (
            <EmptyState
              size="sm"
              illustration="chart"
              title="No samples in this range"
              description="Pick a wider time range, or wait for metrics to accumulate."
            />
          ))
        ) : view === "table" ? (
          <ChartDataTable
            id={tableId}
            summary={summary}
            categoryLabel={categoryLabel}
            total={tableTotal ?? categorical}
          />
        ) : (
          <div
            role="img"
            aria-label={summary}
            className={cn("relative w-full min-w-0", chartClassName ?? "h-72")}
          >
            <Recharts.ResponsiveContainer
              width="100%"
              height="100%"
              initialDimension={{ width: 1, height: 1 }}
            >
              {React.cloneElement(children, { accessibilityLayer: false })}
            </Recharts.ResponsiveContainer>
            {center && (
              <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center text-center">
                {center}
              </div>
            )}
          </div>
        )}
      </figure>
    </ChartContext.Provider>
  );
}

interface ChartTableProps extends Omit<React.ComponentProps<"div">, "children"> {
  config: ChartConfig;
  data: readonly Row[];
  categoryKey: string;
  categoryLabel: string;
  summary: string;
  valueFormat?: ChartValueFormat;
  formatCategory?: (value: unknown) => string;
  series?: string[];
  total?: boolean;
}

function ChartTable({
  config,
  data,
  categoryKey,
  categoryLabel,
  summary,
  valueFormat = "number",
  formatCategory: fc,
  series: seriesProp,
  total = false,
  className,
  ...props
}: ChartTableProps) {
  const first = data[0] ?? {};
  const series = seriesProp ?? Object.keys(config).filter((k) => k in first && k !== categoryKey);
  const ctx: ChartContextValue = {
    config,
    data,
    categoryKey,
    series,
    categorical: false,
    formatValue: (key, v) => formatChartValue(v, config[key]?.format ?? valueFormat),
    formatCategory: (v) =>
      fc ? fc(v) : typeof v === "string" && config[v] ? config[v].label : String(v ?? ""),
    valueFormat,
    total: 0,
  };
  const id = React.useId();
  return (
    <ChartContext.Provider value={ctx}>
      <div data-slot="chart-table" className={className} {...props}>
        <ChartDataTable id={id} summary={summary} categoryLabel={categoryLabel} total={total} />
      </div>
    </ChartContext.Provider>
  );
}

function ChartDataTable({
  id,
  summary,
  categoryLabel,
  total,
}: { id: string; summary: string; categoryLabel: string; total: boolean }) {
  const { data, categoryKey, series, config, formatValue, formatCategory } = useChart();
  const sums = series.map((k) => data.reduce((s, r) => s + (Number(r[k]) || 0), 0));
  return (
    <div id={id} className="max-h-96 overflow-y-auto rounded-lg border border-border">
      <Table>
        <TableCaption className="sr-only">{summary}</TableCaption>
        <TableHeader className="sticky top-0 bg-card">
          <TableRow>
            <TableHead scope="col">{categoryLabel}</TableHead>
            {series.map((k) => (
              <TableHead key={k} scope="col" numeric>
                {config[k]?.label ?? k}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.map((row, i) => (
            <TableRow key={i}>
              <TableHead scope="row" className="h-10 text-sm font-normal text-foreground">
                {formatCategory(row[categoryKey])}
              </TableHead>
              {series.map((k) => (
                <TableCell key={k} numeric className="h-10">
                  {row[k] == null ? "—" : formatValue(k, Number(row[k]))}
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
        {total && (
          <TableFooter>
            <TableRow>
              <TableHead scope="row" className="h-10 text-sm font-medium text-foreground">
                Total
              </TableHead>
              {series.map((k, i) => (
                <TableCell key={k} numeric className="h-10">
                  {formatValue(k, sums[i]!)}
                </TableCell>
              ))}
            </TableRow>
          </TableFooter>
        )}
      </Table>
    </div>
  );
}

function ChartLegend({ keys, className }: { keys?: string[]; className?: string }) {
  const { config, series } = useChart();
  const list = keys ?? series;
  return (
    <ul
      data-slot="chart-legend"
      className={cn(
        "flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground",
        className,
      )}
    >
      {list.map((k) => (
        <li key={k} className="flex items-center gap-1.5">
          <span
            aria-hidden
            className="size-2.5 shrink-0 rounded-[3px] bg-(--swatch)"
            style={{ "--swatch": `var(--color-${k})` } as React.CSSProperties}
          />
          {config[k]?.label ?? k}
        </li>
      ))}
    </ul>
  );
}

const AXIS_TICK = { fill: "var(--muted-foreground)", fontSize: 12 };

function ChartXAxis(props: React.ComponentProps<typeof Recharts.XAxis>) {
  const { categoryKey, formatCategory } = useChart();
  return (
    <Recharts.XAxis
      dataKey={categoryKey}
      tickLine={false}
      axisLine={false}
      tickMargin={10}
      minTickGap={20}
      tick={AXIS_TICK}
      tickFormatter={(v) => formatCategory(v)}
      {...props}
    />
  );
}

function ChartYAxis({
  format,
  ...props
}: React.ComponentProps<typeof Recharts.YAxis> & { format?: ChartValueFormat }) {
  const { valueFormat } = useChart();
  return (
    <Recharts.YAxis
      tickLine={false}
      axisLine={false}
      tickMargin={8}
      tickCount={5}
      niceTicks="snap125"
      width="auto"
      tick={AXIS_TICK}
      tickFormatter={(v: number) => formatAxisValue(v, format ?? valueFormat)}
      {...props}
    />
  );
}

function ChartValueXAxis({
  format,
  normalized = false,
  ...props
}: React.ComponentProps<typeof Recharts.XAxis> & {
  format?: ChartValueFormat;
  normalized?: boolean;
}) {
  const { valueFormat } = useChart();
  return (
    <Recharts.XAxis
      type="number"
      tickLine={false}
      axisLine={false}
      tickMargin={8}
      tick={AXIS_TICK}
      {...(normalized
        ? {
            domain: [0, 1] as [number, number],
            ticks: [0, 0.25, 0.5, 0.75, 1],
            tickFormatter: (v: number) => percent.format(v),
          }
        : {
            tickCount: 5,
            niceTicks: "snap125" as const,
            tickFormatter: (v: number) => formatAxisValue(v, format ?? valueFormat),
          })}
      {...props}
    />
  );
}

function ChartCategoryYAxis(props: React.ComponentProps<typeof Recharts.YAxis>) {
  const { categoryKey, formatCategory } = useChart();
  return (
    <Recharts.YAxis
      type="category"
      dataKey={categoryKey}
      tickLine={false}
      axisLine={false}
      tickMargin={8}
      width="auto"
      tick={AXIS_TICK}
      tickFormatter={(v) => formatCategory(v)}
      {...props}
    />
  );
}

interface ChartReferenceLineProps
  extends Omit<
    React.ComponentProps<typeof Recharts.ReferenceLine>,
    "x" | "y" | "label" | "stroke"
  > {
  value: number;
  label: string;
  axis?: "y" | "x";
  labelPosition?: "start" | "end";
}

function ChartReferenceLine({
  value,
  label,
  axis = "y",
  labelPosition = "end",
  ...props
}: ChartReferenceLineProps) {
  const position =
    axis === "y"
      ? labelPosition === "end"
        ? "insideTopRight"
        : "insideTopLeft"
      : labelPosition === "end"
        ? "insideTopRight"
        : "insideBottomRight";
  return (
    <Recharts.ReferenceLine
      data-slot="chart-reference-line"
      {...(axis === "y" ? { y: value } : { x: value })}
      stroke="var(--foreground)"
      strokeWidth={1.5}
      strokeDasharray="6 4"
      ifOverflow="extendDomain"
      label={{
        value: label,
        position,
        fill: "var(--foreground)",
        fontSize: 12,
        fontWeight: 500,
        stroke: "var(--card)",
        strokeWidth: 3,
        paintOrder: "stroke",
      }}
      {...props}
    />
  );
}

function ChartGrid(props: React.ComponentProps<typeof Recharts.CartesianGrid>) {
  return <Recharts.CartesianGrid vertical={false} stroke="var(--border)" {...props} />;
}

interface ChartTooltipProps extends Omit<React.ComponentProps<typeof Recharts.Tooltip>, "cursor"> {
  cursor?: "line" | "band" | false;
}

function ChartTooltip({ cursor = "line", ...props }: ChartTooltipProps) {
  return (
    <Recharts.Tooltip
      isAnimationActive={false}
      cursor={
        cursor === "band"
          ? { fill: "var(--accent-subtle)" }
          : cursor === "line"
            ? { stroke: "var(--neutral-7)", strokeWidth: 1 }
            : false
      }
      content={<ChartTooltipContent />}
      {...props}
    />
  );
}

type ChartTooltipContentProps = Partial<TooltipContentProps<number, string>> & {
  showTotal?: boolean;
  hideLabel?: boolean;
};

function ChartTooltipContent({
  active,
  payload,
  label,
  showTotal = false,
  hideLabel,
}: ChartTooltipContentProps) {
  const { config, categoryKey, categorical, formatValue, formatCategory, series, total } =
    useChart();
  if (!active || !payload?.length) return null;

  const rows = payload
    .filter((p) => p.type !== "none" && p.value != null)
    .map((p) => {
      const row = p.payload as Row | undefined;
      const key = categorical ? String(row?.[categoryKey]) : String(p.dataKey ?? p.name);
      const valueKey = categorical ? (series[0] ?? key) : key;
      const value = Number(p.value);
      return { key, valueKey, value, label: config[key]?.label ?? String(p.name ?? key) };
    });
  const heading =
    (hideLabel ?? categorical)
      ? null
      : formatCategory(label ?? (payload[0]?.payload as Row | undefined)?.[categoryKey]);
  const sum = rows.reduce((s, r) => s + r.value, 0);

  return (
    <div className="min-w-40 rounded-lg border border-border bg-popover px-3 py-2 text-xs text-popover-foreground shadow-menu">
      {heading && <p className="mb-1.5 font-medium text-foreground">{heading}</p>}
      <ul className="grid gap-1">
        {rows.map((r) => (
          <li key={r.key} className="flex items-center gap-2">
            <span
              aria-hidden
              className="size-2.5 shrink-0 rounded-[3px] bg-(--swatch)"
              style={{ "--swatch": `var(--color-${r.key})` } as React.CSSProperties}
            />
            <span className="text-muted-foreground">{r.label}</span>
            <span className="ml-auto pl-4 font-medium text-foreground tabular-nums">
              {formatValue(r.valueKey, r.value)}
              {categorical && total > 0 && (
                <span className="ml-1.5 font-normal text-muted-foreground">
                  {percent.format(r.value / total)}
                </span>
              )}
            </span>
          </li>
        ))}
      </ul>
      {showTotal && rows.length > 1 && (
        <p className="mt-1.5 flex items-center justify-between gap-4 border-t border-border pt-1.5">
          <span className="text-muted-foreground">Total</span>
          <span className="font-medium text-foreground tabular-nums">
            {formatValue(rows[0]!.valueKey, sum)}
          </span>
        </p>
      )}
    </div>
  );
}

export {
  ChartContainer,
  ChartTable,
  ChartTooltip,
  ChartTooltipContent,
  ChartLegend,
  ChartXAxis,
  ChartYAxis,
  ChartGrid,
  ChartValueXAxis,
  ChartCategoryYAxis,
  ChartReferenceLine,
  formatChartValue,
  formatAxisValue,
  useChart,
};
export type {
  ChartConfig,
  ChartSeriesConfig,
  ChartColor,
  ChartValueFormat,
  ChartContainerProps,
  ChartTableProps,
  ChartReferenceLineProps,
};
