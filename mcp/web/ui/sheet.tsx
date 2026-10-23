import * as SheetPrimitive from "@radix-ui/react-dialog";
import { type VariantProps, cva } from "class-variance-authority";
import * as React from "react";

import { DialogOverlay, OverlayCloseButton } from "./dialog";
import { cn } from "./lib/cn";
import { ModalLayerProvider } from "./lib/layer";

const sheetContentVariants = cva(
  [
    "fixed z-(--z-modal) flex flex-col gap-4 overflow-y-auto bg-popover text-popover-foreground shadow-modal outline-none",
    "transition-[translate] duration-(--duration-page) ease-(--ease-out-expo) data-[state=closed]:animate-exit",
  ],
  {
    variants: {
      side: {
        right:
          "inset-y-0 right-0 h-full w-3/4 border-l border-border sm:max-w-sm starting:translate-x-full",
        left: "inset-y-0 left-0 h-full w-3/4 border-r border-border sm:max-w-sm starting:-translate-x-full",
        top: "inset-x-0 top-0 h-auto max-h-[85dvh] border-b border-border starting:-translate-y-full",
        bottom:
          "inset-x-0 bottom-0 h-auto max-h-[85dvh] rounded-t-2xl border-t border-border starting:translate-y-full",
      },
    },
    defaultVariants: { side: "right" },
  },
);

function Sheet(props: React.ComponentProps<typeof SheetPrimitive.Root>) {
  return <SheetPrimitive.Root data-slot="sheet" {...props} />;
}

function SheetTrigger(props: React.ComponentProps<typeof SheetPrimitive.Trigger>) {
  return <SheetPrimitive.Trigger data-slot="sheet-trigger" {...props} />;
}

function SheetClose(props: React.ComponentProps<typeof SheetPrimitive.Close>) {
  return <SheetPrimitive.Close data-slot="sheet-close" {...props} />;
}

function SheetPortal(props: React.ComponentProps<typeof SheetPrimitive.Portal>) {
  return <SheetPrimitive.Portal data-slot="sheet-portal" {...props} />;
}

function SheetContent({
  className,
  children,
  side = "right",
  showCloseButton = true,
  ...props
}: React.ComponentProps<typeof SheetPrimitive.Content> &
  VariantProps<typeof sheetContentVariants> & {
    showCloseButton?: boolean;
  }) {
  return (
    <SheetPortal>
      <DialogOverlay data-slot="sheet-overlay" />
      <SheetPrimitive.Content
        data-slot="sheet-content"
        data-side={side}
        className={cn(sheetContentVariants({ side }), className)}
        {...props}
      >
        <ModalLayerProvider>{children}</ModalLayerProvider>
        {showCloseButton && <OverlayCloseButton className="top-4 right-4" />}
      </SheetPrimitive.Content>
    </SheetPortal>
  );
}

function SheetHeader({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="sheet-header"
      className={cn("flex flex-col gap-1.5 p-6 pr-14", className)}
      {...props}
    />
  );
}

function SheetFooter({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="sheet-footer"
      className={cn("mt-auto flex flex-col gap-2 p-6", className)}
      {...props}
    />
  );
}

function SheetTitle({ className, ...props }: React.ComponentProps<typeof SheetPrimitive.Title>) {
  return (
    <SheetPrimitive.Title
      data-slot="sheet-title"
      className={cn("text-lg leading-tight font-semibold", className)}
      {...props}
    />
  );
}

function SheetDescription({
  className,
  ...props
}: React.ComponentProps<typeof SheetPrimitive.Description>) {
  return (
    <SheetPrimitive.Description
      data-slot="sheet-description"
      className={cn("text-sm text-pretty text-muted-foreground", className)}
      {...props}
    />
  );
}

export {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
  SheetTrigger,
  sheetContentVariants,
};
