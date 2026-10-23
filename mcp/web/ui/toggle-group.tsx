import * as ToggleGroupPrimitive from "@radix-ui/react-toggle-group";
import * as React from "react";

import { Check } from "lucide-react";

import { cn } from "./lib/cn";
import { Tooltip, TooltipContent, TooltipTrigger } from "./tooltip";

type ToggleGroupProps = React.ComponentProps<typeof ToggleGroupPrimitive.Root> & {
  allowEmpty?: boolean;
};

function ToggleGroup({ className, allowEmpty = false, ...props }: ToggleGroupProps) {
  const [inner, setInner] = React.useState(
    props.type === "single" ? (props.defaultValue ?? "") : "",
  );
  const cls = cn(
    "inline-flex w-fit max-w-full items-center gap-0.5 overflow-x-auto rounded-full bg-muted p-1 pointer-coarse:p-0.5",
    className,
  );

  if (props.type === "single" && !allowEmpty) {
    const { value, defaultValue: _d, onValueChange, ...rest } = props;
    return (
      <ToggleGroupPrimitive.Root
        data-slot="toggle-group"
        className={cls}
        {...rest}
        type="single"
        value={value ?? inner}
        onValueChange={(v) => {
          if (!v) return;
          setInner(v);
          onValueChange?.(v);
        }}
      />
    );
  }
  return <ToggleGroupPrimitive.Root data-slot="toggle-group" className={cls} {...props} />;
}

type ToggleGroupTone = "success" | "warning" | "destructive" | "info" | "neutral";

const TONE: Record<ToggleGroupTone, string> = {
  success:
    "bg-success-surface text-success-text aria-checked:bg-success-surface aria-checked:text-success-text aria-pressed:bg-success-surface aria-pressed:text-success-text dark:aria-checked:bg-success-surface dark:aria-pressed:bg-success-surface hover:text-success-text",
  warning:
    "bg-warning-surface text-warning-text aria-checked:bg-warning-surface aria-checked:text-warning-text aria-pressed:bg-warning-surface aria-pressed:text-warning-text dark:aria-checked:bg-warning-surface dark:aria-pressed:bg-warning-surface hover:text-warning-text",
  destructive:
    "bg-destructive-surface text-destructive-text aria-checked:bg-destructive-surface aria-checked:text-destructive-text aria-pressed:bg-destructive-surface aria-pressed:text-destructive-text dark:aria-checked:bg-destructive-surface dark:aria-pressed:bg-destructive-surface hover:text-destructive-text",
  info: "bg-info-surface text-info-text aria-checked:bg-info-surface aria-checked:text-info-text aria-pressed:bg-info-surface aria-pressed:text-info-text dark:aria-checked:bg-info-surface dark:aria-pressed:bg-info-surface hover:text-info-text",
  neutral:
    "bg-secondary text-secondary-foreground aria-checked:bg-secondary aria-checked:text-secondary-foreground aria-pressed:bg-secondary aria-pressed:text-secondary-foreground dark:aria-checked:bg-secondary dark:aria-pressed:bg-secondary hover:text-secondary-foreground",
};
const TONE_STATE =
  "group hover:ring-1 hover:ring-current aria-checked:font-semibold aria-checked:shadow-none aria-checked:ring-2 aria-checked:ring-current aria-pressed:font-semibold aria-pressed:shadow-none aria-pressed:ring-2 aria-pressed:ring-current";

function ToggleGroupItem({
  className,
  tooltip,
  size = "default",
  tone,
  children,
  ...props
}: React.ComponentProps<typeof ToggleGroupPrimitive.Item> & {
  tone?: ToggleGroupTone;
  tooltip?: string;
  size?: "default" | "icon";
}) {
  const item = (
    <ToggleGroupPrimitive.Item
      data-slot="toggle-group-item"
      data-size={size}
      className={cn(
        "inline-flex h-8 min-w-8 shrink-0 items-center justify-center gap-1.5 rounded-full px-3 text-sm whitespace-nowrap text-muted-foreground",
        "transition-[color,background-color,box-shadow] duration-(--duration-fast) hover:text-foreground",
        "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
        "aria-checked:bg-card aria-checked:font-medium aria-checked:text-card-foreground aria-checked:shadow-raised",
        "aria-pressed:bg-card aria-pressed:font-medium aria-pressed:text-card-foreground aria-pressed:shadow-raised",
        "dark:aria-checked:bg-secondary-hover dark:aria-pressed:bg-secondary-hover",
        "disabled:pointer-events-none disabled:opacity-50 pointer-coarse:h-control-touch pointer-coarse:min-w-control-touch",
        "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        size === "icon" && "w-8 px-0 pointer-coarse:w-control-touch",
        tone && [TONE[tone], TONE_STATE],
        className,
      )}
      {...(tone ? { "data-tone": tone } : {})}
      {...props}
    >
      {tone && (
        <Check
          aria-hidden
          data-slot="toggle-group-item-check"
          className="hidden group-aria-checked:block group-aria-pressed:block"
        />
      )}
      {children}
    </ToggleGroupPrimitive.Item>
  );
  if (!tooltip) return item;
  return (
    <Tooltip>
      <TooltipTrigger asChild>{item}</TooltipTrigger>
      <TooltipContent>{tooltip}</TooltipContent>
    </Tooltip>
  );
}

const ITEM =
  "inline-flex h-8 min-w-8 shrink-0 items-center justify-center gap-1.5 rounded-full px-3 text-sm whitespace-nowrap text-muted-foreground transition-[color,background-color,box-shadow] duration-(--duration-fast) hover:text-foreground outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring pointer-coarse:h-control-touch pointer-coarse:min-w-control-touch [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4 aria-[current=page]:bg-card aria-[current=page]:font-medium aria-[current=page]:text-card-foreground aria-[current=page]:shadow-raised dark:aria-[current=page]:bg-secondary-hover";

function ToggleGroupLinks({
  className,
  ...props
}: React.ComponentProps<"nav"> & { "aria-label": string }) {
  return (
    <nav
      data-slot="toggle-group-links"
      className={cn(
        "inline-flex w-fit max-w-full items-center gap-0.5 overflow-x-auto rounded-full bg-muted p-1 pointer-coarse:p-0.5",
        className,
      )}
      {...props}
    />
  );
}

interface ToggleGroupLinkProps extends React.ComponentProps<"a"> {
  href: string;
  active?: boolean;
  asChild?: boolean;
  size?: "default" | "icon";
}

function ToggleGroupLink({
  className,
  active = false,
  asChild = false,
  size = "default",
  children,
  ...props
}: ToggleGroupLinkProps) {
  const cls = cn(ITEM, size === "icon" && "w-8 px-0 pointer-coarse:w-control-touch", className);
  const aria = {
    "data-slot": "toggle-group-link",
    "aria-current": active ? ("page" as const) : undefined,
  };
  if (asChild && React.isValidElement<Record<string, unknown>>(children)) {
    return React.cloneElement(children, {
      ...aria,
      ...props,
      className: cn(cls, children.props.className as string | undefined),
    });
  }
  return (
    <a {...aria} className={cls} {...props}>
      {children}
    </a>
  );
}

export { ToggleGroup, ToggleGroupItem, ToggleGroupLink, ToggleGroupLinks };
export type { ToggleGroupLinkProps, ToggleGroupProps, ToggleGroupTone };
