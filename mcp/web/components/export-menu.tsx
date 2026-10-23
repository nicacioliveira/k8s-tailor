import { Check, ClipboardCopy, Download, FileJson, FileSpreadsheet } from "lucide-react";
import { useState } from "react";
import { copyText } from "../lib/clipboard";
import { download, exportName, recsToCSV, recsToJSON } from "../lib/export";
import type { Rec } from "../lib/types";
import { Button } from "../ui/button";
import { formatNumber } from "../ui/lib/format";
import { Popover, PopoverContent, PopoverTrigger } from "../ui/popover";

/** Export the rows currently in the table (filters and sort applied) as CSV or JSON. */
export function ExportMenu({ recs, filtered }: { recs: Rec[]; filtered: boolean }) {
  const [open, setOpen] = useState(false);
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  const n = recs.length;
  const done = (ok: boolean, what: string) =>
    setStatus({
      ok,
      text: ok ? what : "Blocked in this embedded view. Use “Copy CSV” and paste it into a file.",
    });
  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) setStatus(null);
      }}
    >
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm" disabled={n === 0}>
          <Download aria-hidden />
          Export
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" aria-label="Export rows" className="flex w-64 flex-col gap-1 p-2">
        <p className="px-2 pt-1 pb-2 text-xs text-muted-foreground">
          {formatNumber(n)} row{n === 1 ? "" : "s"}
          {filtered ? " matching the filters" : ""}, in table order.
        </p>
        <Button
          variant="ghost"
          size="sm"
          className="justify-start"
          onClick={() =>
            done(
              download(exportName("csv"), "text/csv;charset=utf-8", recsToCSV(recs)),
              "CSV downloaded.",
            )
          }
        >
          <FileSpreadsheet aria-hidden />
          Download CSV
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="justify-start"
          onClick={() =>
            done(
              download(exportName("json"), "application/json", recsToJSON(recs)),
              "JSON downloaded.",
            )
          }
        >
          <FileJson aria-hidden />
          Download JSON
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="justify-start"
          onClick={async () =>
            done(await copyText(recsToCSV(recs)), "CSV copied to the clipboard.")
          }
        >
          <ClipboardCopy aria-hidden />
          Copy CSV
        </Button>
        <p role="status" className="min-h-5 px-2 pt-1 text-xs text-muted-foreground">
          {status && (
            <span className="inline-flex items-start gap-1">
              {status.ok && <Check aria-hidden className="mt-0.5 size-3.5 shrink-0" />}
              {status.text}
            </span>
          )}
        </p>
      </PopoverContent>
    </Popover>
  );
}
