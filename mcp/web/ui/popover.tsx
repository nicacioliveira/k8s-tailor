import * as PopoverPrimitive from "@radix-ui/react-popover";
import * as React from "react";

import { cn } from "./lib/cn";
import { useInsideModal } from "./lib/layer";

function Popover(props: React.ComponentProps<typeof PopoverPrimitive.Root>) {
  return <PopoverPrimitive.Root data-slot="popover" {...props} />;
}

function PopoverTrigger(props: React.ComponentProps<typeof PopoverPrimitive.Trigger>) {
  return <PopoverPrimitive.Trigger data-slot="popover-trigger" {...props} />;
}

function PopoverAnchor(props: React.ComponentProps<typeof PopoverPrimitive.Anchor>) {
  return <PopoverPrimitive.Anchor data-slot="popover-anchor" {...props} />;
}

function PopoverContent({
  className,
  align = "center",
  sideOffset = 6,
  ...props
}: React.ComponentProps<typeof PopoverPrimitive.Content>) {
  const insideModal = useInsideModal();
  return (
    <PopoverPrimitive.Portal>
      <PopoverPrimitive.Content
        data-slot="popover-content"
        align={align}
        sideOffset={sideOffset}
        className={cn(
          "w-72 rounded-xl border border-border bg-popover p-4 text-sm text-popover-foreground shadow-menu outline-none",
          "origin-(--radix-popover-content-transform-origin) data-[state=open]:animate-enter data-[state=closed]:animate-exit",
          insideModal ? "z-(--z-modal)" : "z-(--z-dropdown)",
          className,
        )}
        {...props}
      />
    </PopoverPrimitive.Portal>
  );
}

export { Popover, PopoverTrigger, PopoverAnchor, PopoverContent };
