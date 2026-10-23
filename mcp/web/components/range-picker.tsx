import { CalendarRange } from "lucide-react";
import { useState } from "react";
import type { DateRange } from "react-day-picker";
import { dayHhmm } from "../lib/fmt";
import { Button } from "../ui/button";
import { Calendar } from "../ui/calendar";
import { Label } from "../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";
import { ToggleGroup, ToggleGroupItem } from "../ui/toggle-group";

// Time range for every metric panel. Presets map to engine `window`/`step`; a custom
// range is encoded as "custom:<from>:<to>" (unix seconds).
const RANGES: { id: string; window: string; step: number }[] = [
  { id: "5m", window: "5m", step: 10 },
  { id: "10m", window: "10m", step: 15 },
  { id: "15m", window: "15m", step: 20 },
  { id: "30m", window: "30m", step: 30 },
  { id: "1h", window: "1h", step: 30 },
  { id: "3h", window: "3h", step: 60 },
  { id: "12h", window: "12h", step: 180 },
  { id: "24h", window: "24h", step: 300 },
  { id: "7d", window: "7d", step: 1800 },
];
export const DEFAULT_RANGE = "30m";

export const rangeArgs = (id: string): Record<string, unknown> => {
  if (id.startsWith("custom:")) {
    const [, from, to] = id.split(":");
    const span = Math.max(1, +to - +from);
    return { from: +from, to: +to, step: Math.max(15, Math.round(span / 300)) };
  }
  const r = RANGES.find((x) => x.id === id) ?? RANGES.find((x) => x.id === DEFAULT_RANGE);
  return r ? { window: r.window, step: r.step } : {};
};

const TIMES = Array.from({ length: 48 }, (_, i) => {
  const h = String(Math.floor(i / 2)).padStart(2, "0");
  return `${h}:${i % 2 ? "30" : "00"}`;
});

const atTime = (day: Date, hm: string) => {
  const [h, m] = hm.split(":").map(Number);
  const d = new Date(day);
  d.setHours(h, m, 0, 0);
  return Math.floor(d.getTime() / 1000);
};

export function RangePicker({
  value,
  onChange,
}: { value: string; onChange: (id: string) => void }) {
  const isCustom = value.startsWith("custom:");
  const [open, setOpen] = useState(false);
  const parts = isCustom ? value.split(":").map(Number) : [];
  const [range, setRange] = useState<DateRange | undefined>(() =>
    isCustom ? { from: new Date(parts[1] * 1000), to: new Date(parts[2] * 1000) } : undefined,
  );
  const [fromTime, setFromTime] = useState("00:00");
  const [toTime, setToTime] = useState("23:30");
  const from = range?.from ? atTime(range.from, fromTime) : null;
  const to = range?.from ? atTime(range.to ?? range.from, toTime) : null;
  const valid = from != null && to != null && from < to;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <ToggleGroup
        type="single"
        value={isCustom ? "" : value}
        onValueChange={(v) => v && onChange(v)}
        aria-label="Time range"
      >
        {RANGES.map((r) => (
          <ToggleGroupItem key={r.id} value={r.id} className="px-2.5 tabular-nums">
            {r.id}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <Button
            variant={isCustom ? "secondary" : "ghost"}
            size="sm"
            aria-pressed={isCustom}
            className="tabular-nums"
          >
            <CalendarRange aria-hidden />
            {isCustom ? `${dayHhmm(parts[1])} – ${dayHhmm(parts[2])}` : "Custom"}
          </Button>
        </PopoverTrigger>
        <PopoverContent align="end" aria-label="Custom time range" className="w-auto p-0">
          <Calendar mode="range" selected={range} onSelect={setRange} numberOfMonths={1} />
          <div className="grid grid-cols-2 gap-3 border-t border-border p-3">
            <TimeSelect label="From" value={fromTime} onChange={setFromTime} />
            <TimeSelect label="To" value={toTime} onChange={setToTime} />
          </div>
          <div className="flex items-center justify-between gap-3 border-t border-border p-3">
            <p className="text-xs text-muted-foreground tabular-nums">
              {valid ? `${dayHhmm(from)} – ${dayHhmm(to)}` : "Pick a start and end"}
            </p>
            <Button
              size="sm"
              disabled={!valid}
              onClick={() => {
                if (!valid) return;
                onChange(`custom:${from}:${to}`);
                setOpen(false);
              }}
            >
              Apply range
            </Button>
          </div>
        </PopoverContent>
      </Popover>
    </div>
  );
}

function TimeSelect({
  label,
  value,
  onChange,
}: { label: string; value: string; onChange: (v: string) => void }) {
  return (
    <div className="grid gap-1.5">
      <Label className="text-xs text-muted-foreground">{label}</Label>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger size="sm" aria-label={`${label} time`} className="tabular-nums">
          <SelectValue />
        </SelectTrigger>
        <SelectContent position="popper" className="max-h-60">
          {TIMES.map((t) => (
            <SelectItem key={t} value={t} className="tabular-nums">
              {t}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
