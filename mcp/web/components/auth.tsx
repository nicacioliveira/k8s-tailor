import { useQueryClient } from "@tanstack/react-query";
import { KeyRound } from "lucide-react";
import { useEffect, useId, useState, useSyncExternalStore } from "react";
import {
  type AuthRequest,
  clearToken,
  getToken,
  isRemembered,
  onAuthRequired,
  onTokenChange,
  requestAuth,
  setToken,
} from "../lib/auth";
import { Button } from "../ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "../ui/card";
import { Checkbox } from "../ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { InfoBox } from "../ui/info-box";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { TailorMark } from "./brand";

/** Live "is a token stored?" for the header indicator. */
export function useHasToken(): boolean {
  return useSyncExternalStore(
    onTokenChange,
    () => !!getToken(),
    () => false,
  );
}

function TokenForm({
  message,
  onSaved,
  onCancel,
  submitLabel,
}: {
  message?: string;
  onSaved: () => void;
  onCancel?: () => void;
  submitLabel: string;
}) {
  const id = useId();
  const [value, setValue] = useState("");
  const [remember, setRemember] = useState(isRemembered);
  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!value.trim()) return;
    setToken(value, remember);
    onSaved();
  };
  return (
    <form className="grid gap-4" onSubmit={submit}>
      {message && (
        <InfoBox tone="warning" role="alert">
          {message}
        </InfoBox>
      )}
      <div className="grid gap-2">
        <Label htmlFor={`${id}-token`}>Bearer token</Label>
        <Input
          id={`${id}-token`}
          type="password"
          autoComplete="off"
          spellCheck={false}
          // biome-ignore lint/a11y/noAutofocus: the token is the only thing this form asks for
          autoFocus
          value={value}
          onChange={(e) => setValue(e.target.value)}
        />
      </div>
      <div className="flex items-center gap-2">
        <Checkbox
          id={`${id}-remember`}
          checked={remember}
          onCheckedChange={(v) => setRemember(v === true)}
        />
        <Label htmlFor={`${id}-remember`} className="font-normal">
          Remember on this device
        </Label>
      </div>
      <p className="text-xs text-muted-foreground">
        Kept in this browser only — for this tab unless you tick “remember”. Sent as{" "}
        <code>Authorization: Bearer</code> to this server.
      </p>
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        {onCancel && (
          <Button type="button" variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        )}
        <Button type="submit" disabled={!value.trim()}>
          {submitLabel}
        </Button>
      </div>
    </form>
  );
}

/**
 * Standalone auth. Listens for "auth required" (a 401 from the transport, or the header
 * button) and asks for the token:
 *  - MCP transport (everything is gated): a full-screen sign-in replaces the dashboard;
 *  - engine REST transport (writes gated; reads too with TAILOR_API_PROTECT_READS): a
 *    dialog over the dashboard, so the user keeps their place; saving the token refetches
 *    every query, so protected reads load right away and a refused change can be retried.
 */
export function AuthGate({ children }: { children: React.ReactNode }) {
  const qc = useQueryClient();
  const [req, setReq] = useState<AuthRequest | null>(null);
  const hasToken = useHasToken();

  useEffect(() => onAuthRequired(setReq), []);

  const saved = () => {
    setReq(null);
    qc.invalidateQueries();
  };

  if (req?.target === "mcp") {
    return (
      <main className="grid min-h-dvh place-items-center p-6">
        <Card className="w-full max-w-md">
          <CardHeader>
            <TailorMark className="mb-2 size-9 text-primary" />
            <h1 className="text-lg leading-none font-semibold">Sign in to K8s Tailor</h1>
            <CardDescription>Paste the access token your operator gave you.</CardDescription>
          </CardHeader>
          <CardContent>
            <TokenForm
              message={req.reason === "rejected" ? req.message : undefined}
              onSaved={saved}
              submitLabel="Unlock dashboard"
            />
          </CardContent>
        </Card>
      </main>
    );
  }

  return (
    <>
      {children}
      <Dialog open={req?.target === "engine"} onOpenChange={(o) => !o && setReq(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {req?.reason !== "rejected"
                ? "API token"
                : req.read
                  ? "Token needed to view this engine"
                  : "Token needed to make changes"}
            </DialogTitle>
            <DialogDescription>
              {req?.reason !== "rejected"
                ? "Pausing, policies, groups and per-workload changes need the engine's API token (TAILOR_API_TOKEN). An engine started with TAILOR_API_PROTECT_READS needs it for reading too."
                : req.read
                  ? "This engine protects its data (TAILOR_API_PROTECT_READS). Save the token and the dashboard loads."
                  : "The change was not applied. Save the token, then repeat the change."}
            </DialogDescription>
          </DialogHeader>
          {hasToken && req?.reason === "manual" && (
            <InfoBox tone="success" title="A token is saved in this browser">
              Paste a new one to replace it, or forget it.
              <DialogFooter className="mt-3 sm:justify-start">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    clearToken();
                    setReq(null);
                  }}
                >
                  Forget token
                </Button>
              </DialogFooter>
            </InfoBox>
          )}
          <TokenForm
            message={req?.reason === "rejected" ? req.message : undefined}
            onSaved={saved}
            onCancel={() => setReq(null)}
            submitLabel="Save token"
          />
        </DialogContent>
      </Dialog>
    </>
  );
}

/** Header control (engine REST mode): shows whether writes are signed and opens the dialog. */
export function AccessTokenButton() {
  const has = useHasToken();
  return (
    <Button
      variant="ghost"
      size="sm"
      aria-label={
        has ? "API token saved — manage token" : "Sign in with the API token to make changes"
      }
      onClick={() => requestAuth({ reason: "manual", target: "engine" })}
    >
      <KeyRound aria-hidden />
      <span className="max-sm:sr-only">{has ? "Token saved" : "Sign in"}</span>
    </Button>
  );
}
