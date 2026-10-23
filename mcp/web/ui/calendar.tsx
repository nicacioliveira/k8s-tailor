import { ChevronDown, ChevronLeft, ChevronRight } from "lucide-react";
import * as React from "react";
import { type DayButtonProps, DayPicker, type DayPickerProps, type Labels } from "react-day-picker";

import { cn } from "./lib/cn";

const weekdayShort = new Intl.DateTimeFormat("en-US", { weekday: "short" });
const weekdayLong = new Intl.DateTimeFormat("en-US", { weekday: "long" });
const dayLong = new Intl.DateTimeFormat("en-US", {
  day: "numeric",
  month: "long",
  year: "numeric",
});
const monthYear = new Intl.DateTimeFormat("en-US", { month: "long", year: "numeric" });
const monthShort = new Intl.DateTimeFormat("en-US", { month: "short" });

const LABELS: Partial<Labels> = {
  labelNav: () => "Calendar navigation",
  labelPrevious: () => "Previous month",
  labelNext: () => "Next month",
  labelGrid: (date) => monthYear.format(date),
  labelMonthDropdown: () => "Month",
  labelYearDropdown: () => "Year",
  labelWeekday: (date) => weekdayLong.format(date),
  labelDayButton: (date, m) =>
    [dayLong.format(date), weekdayLong.format(date), m.today && "today", m.selected && "selected"]
      .filter(Boolean)
      .join(", "),
  labelGridcell: (date, m) =>
    [dayLong.format(date), m?.today && "today"].filter(Boolean).join(", "),
};

function CalendarDayButton({ className, day, modifiers, ...props }: DayButtonProps) {
  const ref = React.useRef<HTMLButtonElement>(null);
  React.useEffect(() => {
    if (modifiers.focused) ref.current?.focus();
  }, [modifiers.focused]);
  const pontaDoIntervalo = modifiers.range_start || modifiers.range_end;
  return (
    <button
      ref={ref}
      data-slot="calendar-day"
      data-day={day.isoDate}
      data-today={modifiers.today || undefined}
      aria-current={modifiers.today ? "date" : undefined}
      data-outside={modifiers.outside || undefined}
      data-selected-single={
        (modifiers.selected && !modifiers.range_middle && !pontaDoIntervalo) || undefined
      }
      data-range-edge={pontaDoIntervalo || undefined}
      data-range-middle={modifiers.range_middle || undefined}
      className={cn(
        "relative flex size-9 items-center justify-center rounded-(--radius-field) text-sm tabular-nums pointer-coarse:size-11",
        "transition-colors duration-(--duration-fast)",
        "hover:bg-accent hover:text-accent-foreground",
        "outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring",
        "data-outside:text-muted-foreground",
        "data-today:font-semibold data-today:after:absolute data-today:after:bottom-1 data-today:after:left-1/2 data-today:after:size-1 data-today:after:-translate-x-1/2 data-today:after:rounded-full data-today:after:bg-primary",
        "data-selected-single:bg-primary data-selected-single:text-primary-foreground data-selected-single:hover:bg-primary data-selected-single:hover:text-primary-foreground data-selected-single:after:bg-primary-foreground",
        "data-range-edge:bg-primary data-range-edge:text-primary-foreground data-range-edge:hover:bg-primary data-range-edge:hover:text-primary-foreground data-range-edge:after:bg-primary-foreground",
        "data-range-middle:rounded-none data-range-middle:text-foreground data-range-middle:hover:rounded-(--radius-field)",
        "disabled:pointer-events-none disabled:opacity-50 aria-disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

function CalendarChevron({
  orientation,
  className,
}: { orientation?: "up" | "down" | "left" | "right"; className?: string }) {
  const Icone =
    orientation === "left" ? ChevronLeft : orientation === "right" ? ChevronRight : ChevronDown;
  return <Icone aria-hidden className={cn("size-4", className)} />;
}

const BOTAO_NAV = cn(
  "inline-flex size-8 items-center justify-center rounded-full text-muted-foreground pointer-coarse:size-control",
  "transition-colors duration-(--duration-fast) hover:bg-accent hover:text-accent-foreground",
  "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
  "aria-disabled:pointer-events-none aria-disabled:opacity-50",
);

type CalendarProps = DayPickerProps;

function Calendar({
  className,
  classNames,
  formatters,
  labels,
  components,
  showOutsideDays,
  ...props
}: CalendarProps) {
  const mostrarFora = showOutsideDays ?? (props.numberOfMonths ?? 1) === 1;
  return (
    <DayPicker
      data-slot="calendar"
      weekStartsOn={0}
      showOutsideDays={mostrarFora}
      className={cn("w-fit p-3 text-sm", className)}
      formatters={{
        formatCaption: (d) => monthYear.format(d),
        formatWeekdayName: (d) => weekdayShort.format(d),
        formatMonthDropdown: (d) => monthShort.format(d),
        ...formatters,
      }}
      labels={{ ...LABELS, ...labels }}
      classNames={{
        root: "relative",
        months: "relative flex flex-col gap-6 sm:flex-row",
        month: "flex flex-col gap-3",
        nav: "absolute inset-x-0 top-0 flex items-center justify-between",
        button_previous: BOTAO_NAV,
        button_next: BOTAO_NAV,
        month_caption: "flex h-8 items-center justify-center px-10 pointer-coarse:h-control",
        caption_label: "flex items-center gap-1 text-sm font-medium",
        dropdowns: "flex items-center gap-2",
        dropdown_root:
          "relative flex h-8 items-center rounded-(--radius-field) border border-input bg-background px-2 shadow-card has-focus-visible:outline-2 has-focus-visible:outline-offset-2 has-focus-visible:outline-ring",
        dropdown: "absolute inset-0 w-full cursor-pointer opacity-0",
        month_grid: "border-separate border-spacing-x-0 border-spacing-y-0.5",
        weekdays: "",
        weekday: "h-8 w-9 p-0 text-xs font-normal text-muted-foreground pointer-coarse:w-11",
        weeks: "",
        week: "",
        week_number: "text-xs text-muted-foreground",
        week_number_header: "w-9",
        day: "p-0 text-center",
        range_start: "rounded-l-(--radius-field) bg-selected",
        range_middle: "bg-selected",
        range_end: "rounded-r-(--radius-field) bg-selected",
        selected: "",
        today: "",
        outside: "",
        disabled: "",
        hidden: "invisible",
        focused: "",
        footer: "pt-3 text-sm text-muted-foreground",
        chevron: "",
        ...classNames,
      }}
      components={{
        Chevron: CalendarChevron,
        DayButton: CalendarDayButton,
        ...components,
      }}
      {...props}
    />
  );
}

export { Calendar, CalendarDayButton };
export type { CalendarProps };
