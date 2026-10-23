import * as React from "react";

import { cn } from "./lib/cn";

interface SectionHeaderProps {
  title: React.ReactNode;
  description?: React.ReactNode;
  action?: React.ReactNode;
  actions?: React.ReactNode;
  id?: string;
  as?: "h2" | "h3" | "h4";
  className?: string;
}

function SectionHeader({
  title,
  description,
  action,
  actions,
  id,
  as: Heading = "h2",
  className,
}: SectionHeaderProps) {
  const autoId = React.useId();
  const headingId = id ?? autoId;
  const lista = [
    ...React.Children.toArray(actions),
    ...(action ? [<React.Fragment key="action">{action}</React.Fragment>] : []),
  ];
  const varias = lista.length > 1;
  return (
    <div
      data-slot="section-header"
      className={cn("flex flex-wrap items-end justify-between gap-x-4 gap-y-2", className)}
    >
      <div className="min-w-0">
        <Heading id={headingId} className="text-base font-semibold tracking-tight text-foreground">
          {title}
        </Heading>
        {description && <p className="mt-0.5 text-sm text-muted-foreground">{description}</p>}
      </div>
      {lista.length > 0 && (
        <div
          data-slot="section-header-actions"
          {...(varias ? { role: "group", "aria-labelledby": headingId } : null)}
          className="flex max-w-full flex-wrap items-center gap-2 max-sm:w-full"
        >
          {lista}
        </div>
      )}
    </div>
  );
}

export { SectionHeader };
export type { SectionHeaderProps };
