import { CalendarDays, X } from "lucide-react";
import { type ReactNode, cloneElement, isValidElement, useId, useState } from "react";
import { Button } from "../ui/button";
import { Calendar } from "../ui/calendar";
import { Label } from "../ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "../ui/select";

/** Label above, control, hint below (linked with aria-describedby). */
export function Field({
  label,
  hint,
  children,
  className,
}: {
  label: ReactNode;
  hint?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  const id = useId();
  const hintId = `${id}-hint`;
  const control = isValidElement<{ id?: string; "aria-describedby"?: string }>(children)
    ? cloneElement(children, { id, "aria-describedby": hint ? hintId : undefined })
    : children;
  return (
    <div className={`grid content-start gap-2 ${className ?? ""}`}>
      <Label htmlFor={id}>{label}</Label>
      {control}
      {hint && (
        <p id={hintId} className="text-xs text-muted-foreground">
          {hint}
        </p>
      )}
    </div>
  );
}

/** Select over a fixed list of string options. */
export function OptionSelect({
  value,
  options,
  onChange,
  id,
  "aria-describedby": describedBy,
}: {
  value: string;
  options: (string | { value: string; label: string })[];
  onChange: (v: string) => void;
  id?: string;
  "aria-describedby"?: string;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger id={id} aria-describedby={describedBy}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {options.map((o) => {
          const v = typeof o === "string" ? o : o.value;
          return (
            <SelectItem key={v} value={v}>
              {typeof o === "string" ? o : o.label}
            </SelectItem>
          );
        })}
      </SelectContent>
    </Select>
  );
}

const iso = (d: Date) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
const fromIso = (s?: string) => {
  const m = s ? /^(\d{4})-(\d{2})-(\d{2})$/.exec(s) : null;
  return m ? new Date(+m[1], +m[2] - 1, +m[3]) : undefined;
};
const long = new Intl.DateTimeFormat("en-US", { dateStyle: "medium" });

/** Date as "YYYY-MM-DD" (or "" when cleared), picked from the calendar. */
export function DateField({
  value,
  onChange,
  id,
  "aria-describedby": describedBy,
}: {
  value: string;
  onChange: (v: string) => void;
  id?: string;
  "aria-describedby"?: string;
}) {
  const [open, setOpen] = useState(false);
  const date = fromIso(value);
  return (
    <div className="flex items-center gap-1">
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <Button
            id={id}
            aria-describedby={describedBy}
            variant="outline"
            className="flex-1 justify-start rounded-(--radius-field) border-input bg-background font-normal tabular-nums"
          >
            <CalendarDays aria-hidden className="text-muted-foreground" />
            {date ? long.format(date) : <span className="text-muted-foreground">Pick a date</span>}
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" aria-label="Choose a date" className="w-auto p-0">
          <Calendar
            mode="single"
            selected={date}
            defaultMonth={date}
            onSelect={(d) => {
              onChange(d ? iso(d) : "");
              setOpen(false);
            }}
          />
        </PopoverContent>
      </Popover>
      {date && (
        <Button variant="ghost" size="icon" aria-label="Clear date" onClick={() => onChange("")}>
          <X aria-hidden />
        </Button>
      )}
    </div>
  );
}
