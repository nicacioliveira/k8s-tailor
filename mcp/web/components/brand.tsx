import type * as React from "react";

/**
 * K8s Tailor mark: a hexagon (the cluster) with two chevrons closing in (right-sizing).
 * Monochrome — strokes in currentColor, so the caller picks the token (`text-primary`).
 * The favicon in entries/dashboard-ui.html is the same drawing on a filled tile.
 */
export function TailorMark({ className, ...props }: React.SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      className={className}
      {...props}
    >
      <path d="M12 2.5 20.5 7.25v9.5L12 21.5 3.5 16.75v-9.5Z" />
      <path d="m8 9.25 2.75 2.75L8 14.75" />
      <path d="m16 9.25-2.75 2.75L16 14.75" />
    </svg>
  );
}

export function Wordmark() {
  return (
    <span className="flex items-center gap-2.5">
      <TailorMark className="size-7 text-primary" />
      <span className="text-base font-semibold tracking-tight text-foreground">K8s Tailor</span>
    </span>
  );
}
