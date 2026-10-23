import * as SwitchPrimitive from "@radix-ui/react-switch";
import { AlertCircle } from "lucide-react";
import * as React from "react";

import { Label } from "./label";
import { cn } from "./lib/cn";

function Switch({ className, ...props }: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer relative inline-flex h-5 w-9 shrink-0 items-center rounded-full border border-transparent p-0.5",
        "bg-input data-[state=checked]:bg-primary",
        "transition-colors duration-(--duration-fast)",
        "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
        "aria-invalid:border-destructive aria-invalid:focus-visible:outline-destructive",
        "disabled:cursor-not-allowed disabled:opacity-50",
        "pointer-coarse:after:absolute pointer-coarse:after:-inset-x-1 pointer-coarse:after:-inset-y-3 pointer-coarse:after:content-['']",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className={cn(
          "pointer-events-none block size-4 rounded-full bg-background shadow-card",
          "transition-transform duration-(--duration-fast) ease-(--ease-standard)",
          "data-[state=checked]:translate-x-4 data-[state=unchecked]:translate-x-0",
        )}
      />
    </SwitchPrimitive.Root>
  );
}

type SwitchFieldProps = React.ComponentProps<typeof SwitchPrimitive.Root> & {
  label: React.ReactNode;
  description?: React.ReactNode;
  error?: React.ReactNode;
  containerClassName?: string;
};

function SwitchField({
  label,
  description,
  error,
  id,
  className,
  containerClassName,
  disabled,
  "aria-describedby": describedBy,
  ...props
}: SwitchFieldProps) {
  const autoId = React.useId();
  const switchId = id ?? autoId;
  const descriptionId = `${switchId}-description`;
  const errorId = `${switchId}-error`;
  return (
    <div
      data-slot="switch-field"
      data-disabled={disabled || undefined}
      className={cn("flex items-start justify-between gap-4 py-1", containerClassName)}
    >
      <div className="grid min-w-0 gap-1">
        <Label
          htmlFor={switchId}
          className={cn("leading-5", disabled && "cursor-not-allowed opacity-50")}
        >
          {label}
        </Label>
        {description ? (
          <p id={descriptionId} className="text-sm text-muted-foreground">
            {description}
          </p>
        ) : null}
        {error ? (
          <p id={errorId} className="flex items-start gap-1.5 text-sm text-destructive-text">
            <AlertCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
            <span>{error}</span>
          </p>
        ) : null}
      </div>
      <Switch
        id={switchId}
        disabled={disabled}
        aria-describedby={
          [description ? descriptionId : null, error ? errorId : null, describedBy]
            .filter(Boolean)
            .join(" ") || undefined
        }
        aria-invalid={error ? true : props["aria-invalid"]}
        className={cn("mt-px", className)}
        {...props}
      />
    </div>
  );
}

export { Switch, SwitchField };
export type { SwitchFieldProps };
