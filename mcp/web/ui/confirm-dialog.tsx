import * as React from "react";

import { Button } from "./button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "./dialog";
import { InfoBox } from "./info-box";
import { Input } from "./input";
import { Label } from "./label";

type ConfirmTone = "default" | "destructive";

type ConfirmOptions = {
  title: React.ReactNode;
  description?: React.ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  tone?: ConfirmTone;
  requireText?: string;
  requireTextLabel?: React.ReactNode;
  requireTextHint?: React.ReactNode;
};

type ConfirmDialogProps = ConfirmOptions & {
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  onConfirm?: () => void | Promise<void>;
  onCancel?: () => void;
  trigger?: React.ReactNode;
  children?: React.ReactNode;
  confirmDisabled?: boolean;
  errorMessage?: (error: unknown) => React.ReactNode;
};

const defaultErrorMessage = (error: unknown) =>
  error instanceof Error && error.message ? error.message : "Try again.";

function ConfirmDialog({
  title,
  description,
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  tone = "default",
  requireText,
  requireTextLabel,
  requireTextHint = "Case-sensitive.",
  confirmDisabled = false,
  errorMessage = defaultErrorMessage,
  open: openProp,
  defaultOpen = false,
  onOpenChange,
  onConfirm,
  onCancel,
  trigger,
  children,
}: ConfirmDialogProps) {
  const [uncontrolledOpen, setUncontrolledOpen] = React.useState(defaultOpen);
  const open = openProp ?? uncontrolledOpen;
  const [loading, setLoading] = React.useState(false);
  const cancelRef = React.useRef<HTMLButtonElement>(null);
  const confirmRef = React.useRef<HTMLButtonElement>(null);
  const typedRef = React.useRef<HTMLInputElement>(null);
  const typedId = React.useId();
  const [typed, setTyped] = React.useState("");
  const [error, setError] = React.useState<{ value: unknown } | null>(null);
  const typedOk = !requireText || typed.trim() === requireText.trim();
  const blocked = confirmDisabled || !typedOk;

  React.useEffect(() => {
    if (open) {
      setTyped("");
      setError(null);
    }
  }, [open]);

  const setOpen = React.useCallback(
    (next: boolean) => {
      if (openProp === undefined) setUncontrolledOpen(next);
      onOpenChange?.(next);
    },
    [openProp, onOpenChange],
  );

  const handleOpenChange = (next: boolean) => {
    if (!next && loading) return;
    if (!next) onCancel?.();
    setOpen(next);
  };

  const handleConfirm = async () => {
    if (blocked || loading) return;
    setError(null);
    try {
      const result = onConfirm?.();
      if (result instanceof Promise) {
        setLoading(true);
        await result;
      }
    } catch (err) {
      setError({ value: err });
      return;
    } finally {
      setLoading(false);
    }
    setOpen(false);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      {trigger ? <DialogTrigger asChild>{trigger}</DialogTrigger> : null}
      <DialogContent
        role="alertdialog"
        data-slot="confirm-dialog"
        data-tone={tone}
        showCloseButton={false}
        onPointerDownOutside={(event) => event.preventDefault()}
        onInteractOutside={(event) => event.preventDefault()}
        onOpenAutoFocus={(event) => {
          event.preventDefault();
          (requireText
            ? typedRef
            : tone === "destructive"
              ? cancelRef
              : confirmRef
          ).current?.focus();
        }}
        {...(description ? {} : { "aria-describedby": undefined })}
      >
        <DialogHeader className="pr-0">
          <DialogTitle>{title}</DialogTitle>
          {description ? <DialogDescription>{description}</DialogDescription> : null}
        </DialogHeader>
        {children}
        {requireText ? (
          <div data-slot="confirm-dialog-require" className="grid gap-2">
            <Label htmlFor={typedId} className="block leading-normal font-normal">
              {requireTextLabel ?? (
                <>
                  To confirm, type <strong className="font-semibold">{requireText}</strong>
                </>
              )}
            </Label>
            <Input
              ref={typedRef}
              id={typedId}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.nativeEvent.isComposing) {
                  e.preventDefault();
                  void handleConfirm();
                }
              }}
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={loading}
              aria-describedby={requireTextHint ? `${typedId}-dica` : undefined}
            />
            {requireTextHint ? (
              <p id={`${typedId}-dica`} className="text-sm text-muted-foreground">
                {requireTextHint}
              </p>
            ) : null}
          </div>
        ) : null}
        {error ? (
          <InfoBox tone="danger" role="alert" title="Could not complete">
            {errorMessage(error.value)}
          </InfoBox>
        ) : null}
        <DialogFooter>
          <DialogClose asChild>
            <Button ref={cancelRef} variant="outline" disabled={loading}>
              {cancelLabel}
            </Button>
          </DialogClose>
          <Button
            ref={confirmRef}
            variant={tone === "destructive" ? "destructive" : "default"}
            loading={loading}
            disabled={blocked}
            onClick={() => void handleConfirm()}
          >
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

type UseConfirmOptions = ConfirmOptions & {
  onConfirm?: () => void | Promise<void>;
};

type PendingConfirm = {
  options: UseConfirmOptions;
  resolve: (value: boolean) => void;
};

function useConfirm() {
  const [pending, setPending] = React.useState<PendingConfirm | null>(null);
  const [open, setOpen] = React.useState(false);
  const pendingRef = React.useRef<PendingConfirm | null>(null);

  const settle = React.useCallback((value: boolean) => {
    const current = pendingRef.current;
    pendingRef.current = null;
    current?.resolve(value);
  }, []);

  const confirm = React.useCallback(
    (options: UseConfirmOptions) =>
      new Promise<boolean>((resolve) => {
        settle(false);
        const next = { options, resolve };
        pendingRef.current = next;
        setPending(next);
        setOpen(true);
      }),
    [settle],
  );

  React.useEffect(() => () => settle(false), [settle]);

  const confirmDialog = pending ? (
    <ConfirmDialog
      {...pending.options}
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) settle(false);
      }}
      onConfirm={() => {
        const result = pending.options.onConfirm?.();
        if (result instanceof Promise) return result.then(() => settle(true));
        settle(true);
      }}
    />
  ) : null;

  return { confirm, confirmDialog };
}

export { ConfirmDialog, useConfirm };
export type { ConfirmDialogProps, ConfirmOptions, ConfirmTone };
