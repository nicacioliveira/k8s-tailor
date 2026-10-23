import { useMutation } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useState } from "react";
import { Field, OptionSelect } from "../components/field";
import { Hint } from "../components/workload-badges";
import { useMcpApp } from "../context.tsx";
import type { GroupRuleView, MemberRef } from "../lib/types";
import { Banner } from "../ui/banner";
import { Button } from "../ui/button";
import { Card } from "../ui/card";
import { ConfirmDialog } from "../ui/confirm-dialog";
import { EmptyState } from "../ui/empty-state";
import { InfoBox } from "../ui/info-box";
import { Input } from "../ui/input";
import { formatNumber } from "../ui/lib/format";
import { SectionHeader } from "../ui/section-header";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "../ui/sheet";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { callTool, useTool } from "../use-tool.ts";

const blankRule = (): GroupRuleView => ({
  name: "",
  groupBy: "namespace",
  namespaces: [],
  actuation: "direct",
  cohortBy: "",
  cohortSelect: "",
});

const METHODS = [
  { value: "direct", label: "direct (in-place)" },
  { value: "ksvc", label: "ksvc (Knative)" },
  { value: "webhook", label: "webhook" },
];

/**
 * TailorGroup COLLAPSE rules: how workloads fold into one unit sharing an aggregate
 * profile. Distinct from Policies (actuation). A collapse rule never actuates.
 */
export function GroupingView() {
  const app = useMcpApp();
  const list = useTool<{ groups: GroupRuleView[] | null }>(
    "TAILOR_LIST_GROUPS",
    {},
    { refetchInterval: 15_000 },
  );
  const [editing, setEditing] = useState<GroupRuleView | null>(null);
  const [isNew, setIsNew] = useState(false);
  const [confirmDel, setConfirmDel] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: (g: GroupRuleView) =>
      callTool(app, "TAILOR_SET_GROUP", { ...g, namespaces: g.namespaces ?? [] }),
    onSuccess: () => {
      setEditing(null);
      list.refetch();
    },
  });
  const del = useMutation({
    mutationFn: (name: string) => callTool(app, "TAILOR_DELETE_GROUP", { name }),
    onSuccess: () => list.refetch(),
  });
  const groups = list.data?.groups ?? [];
  const scope = (g: GroupRuleView) =>
    (g.namespaces ?? []).length ? (g.namespaces ?? []).join(", ") : "All namespaces";
  const open = (g: GroupRuleView, fresh: boolean) => {
    save.reset();
    setIsNew(fresh);
    setEditing({ ...g, namespaces: g.namespaces ?? [] });
  };

  return (
    <>
      <h1 className="sr-only">Grouping</h1>
      <section aria-labelledby="grouping-title" className="flex flex-col gap-4">
        <SectionHeader
          id="grouping-title"
          title="Grouping"
          description="Collapse rules fold many workloads into one unit that shares a single aggregate profile. They only group — they never actuate."
          action={
            <Button size="sm" onClick={() => open(blankRule(), true)}>
              <Plus aria-hidden />
              New rule
            </Button>
          }
        />
        <InfoBox tone="muted" icon={false}>
          Use <code>groupBy: namespace</code> to fold a whole namespace — for example every
          per-environment StatefulSet in a site namespace, which share no common label. A label key
          folds pods sharing that label. Knative services are grouped by default.
        </InfoBox>
        {list.isError && (
          <Banner
            variant="danger"
            role="alert"
            title="Could not load collapse rules"
            description={list.error instanceof Error ? list.error.message : String(list.error)}
          />
        )}
        <Card className="gap-0 py-0">
          {list.isLoading ? (
            <div role="status" aria-label="Loading rules" className="flex flex-col gap-3 p-4">
              {[0, 1].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : groups.length === 0 ? (
            <EmptyState
              illustration="inbox"
              title="No collapse rules"
              description="Add one to collapse a namespace or a labelled fleet into a single sizing unit."
              action={
                <Button size="sm" onClick={() => open(blankRule(), true)}>
                  <Plus aria-hidden />
                  Create the first rule
                </Button>
              }
            />
          ) : (
            <Table aria-label="Collapse rules">
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-4">Rule</TableHead>
                  <TableHead>Group by</TableHead>
                  <TableHead>Scope</TableHead>
                  <TableHead>Method</TableHead>
                  <TableHead>Cohort</TableHead>
                  <TableHead className="pr-4">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {groups.map((g) => (
                  <TableRow key={g.name} interactive onClick={() => open(g, false)}>
                    <TableCell className="pl-4">
                      <button
                        type="button"
                        className="font-medium outline-none hover:underline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
                      >
                        {g.name}
                      </button>
                    </TableCell>
                    <TableCell>
                      <code>{g.groupBy}</code>
                    </TableCell>
                    <TableCell className="text-muted-foreground">{scope(g)}</TableCell>
                    <TableCell className="text-muted-foreground">
                      {g.actuation || "direct"}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {g.cohortBy ? `${g.cohortBy}: ${g.cohortSelect || "latest"}` : "—"}
                    </TableCell>
                    <TableCell className="pr-4 text-right" onClick={(e) => e.stopPropagation()}>
                      <Hint text="Delete rule">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Delete rule ${g.name}`}
                          onClick={() => setConfirmDel(g.name)}
                        >
                          <Trash2 aria-hidden />
                        </Button>
                      </Hint>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </Card>
      </section>

      <Sheet open={editing != null} onOpenChange={(o) => !o && setEditing(null)}>
        <SheetContent side="right" className="w-full gap-0 sm:max-w-2xl">
          <SheetHeader className="border-b border-border">
            <SheetTitle>{isNew ? "New collapse rule" : `Edit “${editing?.name}”`}</SheetTitle>
            <SheetDescription>
              <code>groupBy: namespace</code> collapses an entire namespace; a label key collapses
              pods sharing that label.
            </SheetDescription>
          </SheetHeader>
          {editing && (
            <div className="flex flex-col gap-8 p-6">
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="Name">
                  <Input
                    placeholder="e.g. site-envs"
                    value={editing.name}
                    onChange={(e) => setEditing({ ...editing, name: e.target.value })}
                  />
                </Field>
                <Field label="Group by" hint='A label key, or "namespace"'>
                  <Input
                    placeholder="namespace"
                    value={editing.groupBy}
                    onChange={(e) => setEditing({ ...editing, groupBy: e.target.value })}
                  />
                </Field>
                <Field label="Namespaces" hint="Globs, comma-separated">
                  <Input
                    placeholder="apps-*"
                    value={(editing.namespaces ?? []).join(", ")}
                    onChange={(e) =>
                      setEditing({
                        ...editing,
                        namespaces: e.target.value
                          .split(",")
                          .map((s) => s.trim())
                          .filter(Boolean),
                      })
                    }
                  />
                </Field>
                <Field label="Method">
                  <OptionSelect
                    value={editing.actuation || "direct"}
                    options={METHODS}
                    onChange={(v) => setEditing({ ...editing, actuation: v })}
                  />
                </Field>
              </div>
              {!isNew && editing.name && <GroupMembers name={editing.name} />}
            </div>
          )}
          <SheetFooter className="sticky bottom-0 mt-auto flex-row justify-end border-t border-border bg-popover">
            {save.isError && (
              <InfoBox tone="danger" role="alert" className="mr-auto" title="Not saved">
                {save.error instanceof Error ? save.error.message : String(save.error)}
              </InfoBox>
            )}
            <Button variant="outline" onClick={() => setEditing(null)}>
              Cancel
            </Button>
            <Button
              onClick={() => editing && save.mutate(editing)}
              disabled={!editing?.name || !editing?.groupBy}
              loading={save.isPending}
            >
              {isNew ? "Create rule" : "Save rule"}
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <ConfirmDialog
        open={confirmDel != null}
        onOpenChange={(o) => !o && setConfirmDel(null)}
        tone="destructive"
        title={`Delete rule “${confirmDel}”?`}
        description="Workloads stop being collapsed by this rule and appear individually again. The TailorGroup resource is removed from the cluster."
        confirmLabel="Delete rule"
        onConfirm={async () => {
          if (confirmDel) await del.mutateAsync(confirmDel);
        }}
      />
    </>
  );
}

/** Drill-down: the raw workloads a collapse rule folds. */
function GroupMembers({ name }: { name: string }) {
  const q = useTool<{ members: MemberRef[] | null }>(
    "TAILOR_GROUP_MEMBERS",
    { name },
    { refetchInterval: 30_000 },
  );
  const members = q.data?.members ?? [];
  const units = new Set(members.map((m) => m.unit)).size;
  return (
    <section aria-labelledby="members-title" className="flex flex-col gap-3">
      <div className="flex items-baseline gap-2">
        <h3 id="members-title" className="text-base font-semibold tracking-tight">
          Members
        </h3>
        {!q.isLoading && (
          <span className="text-sm text-muted-foreground tabular-nums">
            {formatNumber(members.length)} workloads
            {units > 1 ? ` across ${formatNumber(units)} units` : ""}
          </span>
        )}
      </div>
      {q.isError ? (
        <InfoBox tone="danger" title="Could not load members">
          {q.error instanceof Error ? q.error.message : String(q.error)}
        </InfoBox>
      ) : q.isLoading ? (
        <Skeleton className="h-32 w-full" />
      ) : members.length === 0 ? (
        <EmptyState
          size="sm"
          illustration="search"
          title="No workloads match this rule yet"
          description="Check the namespaces globs and the group-by key."
        />
      ) : (
        <div className="max-h-80 overflow-auto rounded-xl border border-border">
          <Table aria-label="Rule members">
            <TableHeader className="sticky top-0 bg-popover">
              <TableRow>
                <TableHead className="pl-4">Unit</TableHead>
                <TableHead>Namespace</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Workload</TableHead>
                <TableHead numeric className="pr-4">
                  Replicas
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {members.slice(0, 300).map((m) => (
                <TableRow key={`${m.unit}/${m.namespace}/${m.kind}/${m.name}`}>
                  <TableCell className="pl-4 text-muted-foreground">{m.unit}</TableCell>
                  <TableCell className="text-muted-foreground">{m.namespace}</TableCell>
                  <TableCell>{m.kind}</TableCell>
                  <TableCell className="font-medium">{m.name}</TableCell>
                  <TableCell numeric className="pr-4">
                    {formatNumber(m.replicas)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      {members.length > 300 && (
        <p className="text-sm text-muted-foreground tabular-nums">
          Showing 300 of {formatNumber(members.length)}
        </p>
      )}
    </section>
  );
}
