import { Slot } from "@radix-ui/react-slot";
import { type VariantProps, cva } from "class-variance-authority";
import { Loader2 } from "lucide-react";
import * as React from "react";

import { cn } from "./lib/cn";

const FILLED_DISABLED =
  "disabled:bg-muted disabled:text-muted-foreground disabled:opacity-100 disabled:shadow-none";

const buttonVariants = cva(
  [
    "inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-(--radius-button) text-sm font-medium",
    "transition-[color,background-color,border-color,box-shadow,transform] duration-(--duration-fast)",
    "outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring",
    "disabled:pointer-events-none disabled:opacity-50 aria-busy:cursor-progress",
    "aria-invalid:border-destructive aria-invalid:outline-destructive active:scale-[0.98]",
    "[&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  ],
  {
    variants: {
      variant: {
        default: `bg-primary text-primary-foreground shadow-button hover:bg-primary/90 ${FILLED_DISABLED}`,
        brand: `bg-brand text-brand-foreground shadow-button hover:bg-brand/90 ${FILLED_DISABLED}`,
        destructive: `bg-destructive text-destructive-foreground shadow-card hover:bg-destructive/90 focus-visible:outline-destructive ${FILLED_DISABLED}`,
        outline:
          "border border-border bg-card shadow-card hover:bg-accent hover:text-accent-foreground",
        secondary: "bg-secondary text-secondary-foreground hover:bg-secondary-hover",
        ghost: "hover:bg-accent hover:text-accent-foreground",
        link: "text-primary underline-offset-4 hover:underline active:scale-100",
      },
      size: {
        default: "h-control px-4 has-[>svg]:px-3 pointer-coarse:h-control-touch",
        sm: "h-control-sm gap-1.5 px-3 has-[>svg]:px-2.5 pointer-coarse:h-control",
        lg: "h-control-lg px-6 text-base has-[>svg]:px-5",
        icon: "size-control pointer-coarse:size-control-touch",
        "icon-sm": "size-control-sm pointer-coarse:size-control",
        "icon-lg": "size-control-lg",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

type ButtonProps = React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean;
    loading?: boolean;
  };

function Button({
  className,
  variant,
  size,
  asChild = false,
  loading = false,
  disabled,
  children,
  ...props
}: ButtonProps) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size, className }))}
      disabled={asChild ? undefined : disabled || loading}
      aria-busy={loading || undefined}
      {...props}
    >
      {loading && !asChild ? (
        <>
          <Loader2 className="animate-spin" aria-hidden />
          {children}
        </>
      ) : (
        children
      )}
    </Comp>
  );
}

export { Button, buttonVariants };
export type { ButtonProps };
