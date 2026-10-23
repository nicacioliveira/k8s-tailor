import * as React from "react";

import { cn } from "./lib/cn";

function useOverflow(ref: React.RefObject<HTMLElement | null>) {
  const [overflow, setOverflow] = React.useState(false);
  React.useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const check = () => setOverflow(el.scrollWidth > el.clientWidth);
    check();
    void document.fonts?.ready.then(check);
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(check);
    ro.observe(el);
    if (el.firstElementChild) ro.observe(el.firstElementChild);
    return () => ro.disconnect();
  }, [ref]);
  return overflow;
}

function Table({
  className,
  scrollLabel,
  ...props
}: React.ComponentProps<"table"> & {
  scrollLabel?: string;
}) {
  const ref = React.useRef<HTMLDivElement>(null);
  const overflow = useOverflow(ref);
  const regionProps = overflow
    ? {
        role: "region" as const,
        tabIndex: 0,
        // Distinct from the surrounding section's name (axe landmark-unique): the
        // region is the scroll container, not the section itself.
        "aria-label":
          scrollLabel ?? (props["aria-label"] ? `${props["aria-label"]} (scrollable)` : "Table"),
      }
    : {};
  return (
    <div
      ref={ref}
      data-slot="table-container"
      data-overflow={overflow || undefined}
      className="relative w-full overflow-x-auto overflow-y-hidden outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
      {...regionProps}
    >
      <table
        data-slot="table"
        className={cn("w-full caption-bottom text-sm tabular-nums", className)}
        {...props}
      />
    </div>
  );
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return (
    <thead
      data-slot="table-header"
      className={cn("[&_tr]:border-b [&_tr]:border-border", className)}
      {...props}
    />
  );
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return (
    <tbody
      data-slot="table-body"
      className={cn("[&_tr:last-child]:border-0", className)}
      {...props}
    />
  );
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return (
    <tfoot
      data-slot="table-footer"
      className={cn("border-t border-border font-medium [&_tr:last-child]:border-0", className)}
      {...props}
    />
  );
}

function TableRow({
  className,
  interactive = false,
  ...props
}: React.ComponentProps<"tr"> & {
  interactive?: boolean;
}) {
  return (
    <tr
      data-slot="table-row"
      className={cn(
        "border-b border-border transition-colors duration-(--duration-fast) data-[state=selected]:bg-selected",
        interactive &&
          "relative cursor-pointer hover:bg-accent-subtle has-focus-visible:bg-accent-subtle",
        className,
      )}
      {...props}
    />
  );
}

function TableHead({
  className,
  numeric = false,
  ...props
}: React.ComponentProps<"th"> & {
  numeric?: boolean;
}) {
  return (
    <th
      data-slot="table-head"
      className={cn(
        "h-10 px-3 text-left align-middle text-xs font-medium whitespace-nowrap text-muted-foreground",
        "[&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[2px]",
        numeric && "text-right",
        className,
      )}
      {...props}
    />
  );
}

function TableCell({
  className,
  numeric = false,
  ...props
}: React.ComponentProps<"td"> & {
  numeric?: boolean;
}) {
  return (
    <td
      data-slot="table-cell"
      data-money={numeric || undefined}
      className={cn(
        "h-12 px-3 align-middle whitespace-nowrap",
        "[&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[2px]",
        numeric && "text-right",
        className,
      )}
      {...props}
    />
  );
}

function TableCaption({ className, ...props }: React.ComponentProps<"caption">) {
  return (
    <caption
      data-slot="table-caption"
      className={cn("mt-4 text-sm text-muted-foreground", className)}
      {...props}
    />
  );
}

export { Table, TableHeader, TableBody, TableFooter, TableHead, TableRow, TableCell, TableCaption };
