import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "../ui/dialog";

const ROWS: { keys: string[]; label: string }[] = [
  { keys: ["/"], label: "Focus the filter on this page" },
  { keys: ["g", "o"], label: "Go to Overview" },
  { keys: ["g", "w"], label: "Go to Workloads" },
  { keys: ["g", "s"], label: "Go to Savings" },
  { keys: ["g", "p"], label: "Go to Policies" },
  { keys: ["g", "r"], label: "Go to Grouping" },
  { keys: ["g", "d"], label: "Go to Decisions" },
  { keys: ["g", "h"], label: "Go to Health" },
  { keys: ["?"], label: "Show this help" },
  { keys: ["Esc"], label: "Close a panel or dialog" },
];

export function ShortcutsHelp({
  open,
  onOpenChange,
}: { open: boolean; onOpenChange: (o: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Keyboard shortcuts</DialogTitle>
          <DialogDescription>
            Work anywhere except while typing in a field. Press the keys in sequence for “g then o”.
          </DialogDescription>
        </DialogHeader>
        <dl className="grid grid-cols-[auto_1fr] items-center gap-x-6 gap-y-3 text-sm">
          {ROWS.map((r) => (
            <div key={r.label} className="contents">
              <dt className="flex items-center gap-1">
                {r.keys.map((k, i) => (
                  <span key={k} className="flex items-center gap-1">
                    {i > 0 && <span className="text-xs text-muted-foreground">then</span>}
                    <kbd className="inline-flex h-6 min-w-6 items-center justify-center rounded-md border border-border bg-muted px-1.5 text-foreground">
                      {k}
                    </kbd>
                  </span>
                ))}
              </dt>
              <dd className="text-muted-foreground">{r.label}</dd>
            </div>
          ))}
        </dl>
      </DialogContent>
    </Dialog>
  );
}
