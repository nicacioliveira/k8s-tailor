import * as React from "react";

import { cn } from "./lib/cn";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-control w-full min-w-0 rounded-(--radius-field) border border-input bg-background px-3 py-1 text-sm shadow-card pointer-coarse:h-control-touch",
        "placeholder:text-muted-foreground selection:bg-primary selection:text-primary-foreground",
        "transition-[color,border-color,box-shadow] duration-(--duration-fast) outline-none",
        "file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium file:text-foreground",
        "disabled:pointer-events-none disabled:cursor-not-allowed disabled:bg-muted disabled:opacity-50",
        "focus-visible:border-ring focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
        "aria-invalid:border-destructive aria-invalid:focus-visible:outline-destructive",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
