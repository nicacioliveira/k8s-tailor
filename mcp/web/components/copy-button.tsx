import { Check, Copy } from "lucide-react";
import { useEffect, useState } from "react";
import { copyText } from "../lib/clipboard";
import { Button } from "../ui/button";

/** One-click copy with a spoken confirmation ("Copied" / "Copy failed"). */
export function CopyButton({
  text,
  label,
  iconOnly,
}: { text: string; label: string; iconOnly?: boolean }) {
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");
  useEffect(() => {
    if (state === "idle") return;
    const t = setTimeout(() => setState("idle"), 2_000);
    return () => clearTimeout(t);
  }, [state]);
  return (
    <span className="inline-flex items-center gap-2">
      <Button
        variant={iconOnly ? "ghost" : "outline"}
        size={iconOnly ? "icon-sm" : "sm"}
        aria-label={iconOnly ? label : undefined}
        onClick={async () => setState((await copyText(text)) ? "ok" : "fail")}
      >
        {state === "ok" ? <Check aria-hidden /> : <Copy aria-hidden />}
        {iconOnly ? null : label}
      </Button>
      <span role="status" className="text-xs text-muted-foreground">
        {state === "ok"
          ? "Copied"
          : state === "fail"
            ? "Copy blocked — select the text instead"
            : ""}
      </span>
    </span>
  );
}
