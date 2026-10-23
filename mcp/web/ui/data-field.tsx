import * as React from "react";

import { cn } from "./lib/cn";

const InDataGrid = React.createContext(false);

function isEmpty(value: React.ReactNode) {
  return value === null || value === undefined || value === "";
}

interface DataFieldProps {
  label: React.ReactNode;
  value: React.ReactNode;
  numeric?: boolean;
  className?: string;
}

function DataField({ label, value, numeric = false, className }: DataFieldProps) {
  const inGrid = React.useContext(InDataGrid);
  const Term = inGrid ? "dt" : "p";
  const Desc = inGrid ? "dd" : "p";

  return (
    <div data-slot="data-field" className={cn("flex min-w-0 flex-col gap-1", className)}>
      <Term className="text-xs text-muted-foreground">{label}</Term>
      <Desc
        data-money={numeric || undefined}
        className="text-sm leading-snug font-medium break-words"
      >
        {isEmpty(value) ? (
          <>
            <span aria-hidden className="text-muted-foreground">
              —
            </span>
            <span className="sr-only">Not set</span>
          </>
        ) : (
          value
        )}
      </Desc>
    </div>
  );
}

function DataGrid({ children, className }: { children: React.ReactNode; className?: string }) {
  return (
    <InDataGrid.Provider value={true}>
      <dl data-slot="data-grid" className={cn("grid gap-x-6 gap-y-5 sm:grid-cols-2", className)}>
        {children}
      </dl>
    </InDataGrid.Provider>
  );
}

export { DataField, DataGrid };
export type { DataFieldProps };
