"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, GitBranch, History, Save, SearchX, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { ApiError } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import {
  extWorkflowDetailOptions,
  useArchiveExtWorkflow,
  useUpdateExtWorkflow,
  type ExtWorkflow,
} from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT, useTimeAgo } from "../../i18n";
import { BreadcrumbHeader } from "../../layout/breadcrumb-header";
import { CollectionPageState } from "../../layout/collection-page";
import { useNavigation } from "../../navigation";
import {
  extractValidationErrors,
  mapServerErrors,
  nodesToRows,
  rowsToInput,
  serializeRows,
  validateRows,
  type MappedServerErrors,
  type NodeRow,
} from "../node-editor-helpers";
import { AgentSelect } from "./agent-select";
import { NodeEditor } from "./node-editor";
import { WorkflowRunsTab } from "./workflow-runs-tab";

type DetailTab = "nodes" | "runs";
const DETAIL_TABS: { id: DetailTab; icon: typeof GitBranch }[] = [
  { id: "nodes", icon: GitBranch },
  { id: "runs", icon: History },
];

interface Draft {
  name: string;
  description: string;
  supervisorId: string;
  maxRewinds: number;
  rows: NodeRow[];
}

function draftFrom(w: ExtWorkflow): Draft {
  return {
    name: w.name,
    description: w.description ?? "",
    supervisorId: w.supervisor_agent_id,
    maxRewinds: w.max_rewinds,
    rows: nodesToRows(w.nodes ?? []),
  };
}

function signature(d: Draft): string {
  return JSON.stringify([d.name, d.description, d.supervisorId, d.maxRewinds, serializeRows(d.rows)]);
}

export function WorkflowDetailPage() {
  const { t } = useT("ext-workflows");
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { pathname, push } = useNavigation();
  const workflowId = pathname.split("/").pop() ?? "";
  const currentUser = useAuthStore((s) => s.user);

  const { data: workflow, isError, error, refetch } = useQuery({
    ...extWorkflowDetailOptions(wsId, workflowId),
    enabled: !!wsId && !!workflowId,
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const update = useUpdateExtWorkflow(wsId);
  const archive = useArchiveExtWorkflow(wsId);

  const [baseline, setBaseline] = useState<Draft | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [tab, setTab] = useState<DetailTab>("nodes");
  const [pendingTab, setPendingTab] = useState<DetailTab | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const [serverErrors, setServerErrors] = useState<MappedServerErrors | null>(null);
  const [confirmArchive, setConfirmArchive] = useState(false);

  const dirty = !!draft && !!baseline && signature(draft) !== signature(baseline);
  const serverDraft = workflow ? draftFrom(workflow) : null;
  // Adopt server state while the user has no unsaved edits (first load, a
  // realtime update, or right after Discard). Never overwrite a dirty draft.
  if (serverDraft && !dirty && (!baseline || signature(serverDraft) !== signature(baseline))) {
    setBaseline(serverDraft);
    setDraft(serverDraft);
  }

  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const myRole = useMemo(
    () => members.find((m: MemberWithUser) => m.user_id === currentUser?.id)?.role ?? null,
    [members, currentUser],
  );
  // Mirrors canManageSquad: workspace admins, or the creator.
  const canManage =
    myRole === "owner" || myRole === "admin" || (!!currentUser && workflow?.creator_id === currentUser.id);

  const nodesChanged = !!draft && !!baseline && serializeRows(draft.rows) !== serializeRows(baseline.rows);
  const validation = useMemo(
    () => (draft && showErrors ? validateRows(draft.rows) : null),
    [draft, showErrors],
  );

  // A failed refetch keeps the loaded editor; only an initial failure replaces it.
  if (isError && !workflow) {
    if (error instanceof ApiError && error.status === 404) {
      return (
        <CollectionPageState icon={SearchX} title={t(($) => $.detail.not_found)} />
      );
    }
    return (
      <CollectionPageState
        role="alert"
        tone="destructive"
        icon={AlertCircle}
        title={t(($) => $.detail.load_failed)}
        description={error instanceof Error ? error.message : undefined}
        actions={
          <Button type="button" variant="outline" size="sm" onClick={() => void refetch()}>
            {t(($) => $.detail.retry)}
          </Button>
        }
      />
    );
  }
  if (!workflow || !draft || !baseline) return <DetailSkeleton />;

  const patch = (next: Partial<Draft>) => setDraft({ ...draft, ...next });
  const canSave = canManage && dirty && !!draft.name.trim() && !!draft.supervisorId && !update.isPending;

  const requestTab = (next: DetailTab) => {
    if (next === tab) return;
    if (dirty) {
      setPendingTab(next);
      return;
    }
    setTab(next);
  };

  const discard = () => {
    setDraft(baseline);
    setShowErrors(false);
    setServerErrors(null);
  };

  const save = async () => {
    if (!canSave) return;
    if (nodesChanged) {
      setShowErrors(true);
      if (!validateRows(draft.rows).valid) return;
    }
    try {
      const saved = await update.mutateAsync({
        id: workflowId,
        name: draft.name.trim(),
        description: draft.description.trim(),
        supervisor_agent_id: draft.supervisorId,
        max_rewinds: draft.maxRewinds,
        ...(nodesChanged ? { nodes: rowsToInput(draft.rows) } : {}),
      });
      const next = draftFrom(saved);
      setBaseline(next);
      setDraft(next);
      setShowErrors(false);
      setServerErrors(null);
      toast.success(t(($) => $.detail.saved));
    } catch (err) {
      const errors = extractValidationErrors(err);
      if (errors) {
        setServerErrors(mapServerErrors(errors, draft.rows));
        toast.error(t(($) => $.detail.validation_failed));
      } else {
        toast.error(err instanceof Error && err.message ? err.message : t(($) => $.detail.save_failed));
      }
    }
  };

  const doArchive = async () => {
    try {
      await archive.mutateAsync(workflowId);
      toast.success(t(($) => $.archive_dialog.success));
      push(p.workflows());
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.archive_dialog.failed));
    }
  };

  const creatorName = members.find((m: MemberWithUser) => m.user_id === workflow.creator_id)?.name;
  const initials = workflow.name
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <BreadcrumbHeader
        segments={[{ href: p.workflows(), label: t(($) => $.page.title) }]}
        leaf={
          <>
            <ActorAvatarBase
              name={workflow.name}
              initials={initials}
              avatarUrl={workflow.avatar_url ? resolvePublicFileUrl(workflow.avatar_url) : undefined}
              isWorkflow
              size="sm"
            />
            <h1 className="truncate text-body font-medium text-foreground">{workflow.name}</h1>
          </>
        }
        actions={
          canManage ? (
            <>
              {dirty && <span className="text-caption text-muted-foreground">{t(($) => $.detail.unsaved)}</span>}
              {dirty && (
                <Button size="sm" variant="ghost" onClick={discard}>
                  {t(($) => $.detail.discard)}
                </Button>
              )}
              <Button size="sm" onClick={() => void save()} disabled={!canSave} aria-busy={update.isPending}>
                <Save className="size-3.5" />
                {update.isPending ? t(($) => $.detail.saving) : t(($) => $.detail.save)}
              </Button>
              <Button
                size="sm"
                variant="ghost"
                className="text-destructive hover:text-destructive"
                onClick={() => setConfirmArchive(true)}
              >
                <Trash2 className="size-3.5" />
                {t(($) => $.detail.archive_button)}
              </Button>
            </>
          ) : null
        }
      />

      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3 md:grid md:grid-cols-[minmax(0,1fr)_280px] md:gap-4 md:overflow-hidden md:p-6 lg:grid-cols-[minmax(0,1fr)_320px]">
        <div className="flex min-h-[60vh] flex-col overflow-hidden rounded-lg border bg-background md:h-full md:min-h-0">
          <div className="flex shrink-0 items-center gap-0 overflow-x-auto border-b px-2 md:px-4">
            {DETAIL_TABS.map((entry) => (
              <button
                key={entry.id}
                type="button"
                onClick={() => requestTab(entry.id)}
                className={`flex shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2.5 text-caption font-medium transition-colors ${
                  tab === entry.id
                    ? "border-foreground text-foreground"
                    : "border-transparent text-muted-foreground hover:text-foreground"
                }`}
              >
                <entry.icon className="size-3.5" />
                {t(($) => $.detail.tabs[entry.id])}
              </button>
            ))}
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-4 md:p-6">
            {tab === "nodes" ? (
              <NodeEditor
                rows={draft.rows}
                onChange={(rows) => {
                  setServerErrors(null);
                  patch({ rows });
                }}
                agents={agents as Agent[]}
                validation={validation}
                serverErrors={serverErrors}
                activeRunCount={workflow.active_run_count}
                defaultAgentId={draft.supervisorId}
                readOnly={!canManage}
              />
            ) : (
              <WorkflowRunsTab workflowId={workflowId} />
            )}
          </div>
        </div>

        <aside className="flex w-full flex-col gap-4 rounded-lg border bg-background p-5 md:h-full md:min-h-0 md:overflow-y-auto">
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-name" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.name)}
            </Label>
            <Input
              id="ext-workflow-inspector-name"
              value={draft.name}
              disabled={!canManage}
              onChange={(e) => patch({ name: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-description" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.description)}
            </Label>
            <Textarea
              id="ext-workflow-inspector-description"
              value={draft.description}
              disabled={!canManage}
              placeholder={t(($) => $.inspector.description_placeholder)}
              rows={3}
              onChange={(e) => patch({ description: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label className="text-caption text-muted-foreground">{t(($) => $.inspector.supervisor)}</Label>
            <AgentSelect
              agents={agents as Agent[]}
              value={draft.supervisorId}
              onChange={(supervisorId) => patch({ supervisorId })}
              ariaLabel={t(($) => $.inspector.supervisor)}
              placeholder={t(($) => $.create.supervisor_placeholder)}
              requireRuntime
              disabled={!canManage}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-inspector-rewinds" className="text-caption text-muted-foreground">
              {t(($) => $.inspector.max_rewinds)}
            </Label>
            <Input
              id="ext-workflow-inspector-rewinds"
              type="number"
              min={0}
              max={10}
              value={draft.maxRewinds}
              disabled={!canManage}
              onChange={(e) => {
                const n = e.target.valueAsNumber;
                patch({ maxRewinds: Number.isNaN(n) ? 0 : Math.min(10, Math.max(0, Math.trunc(n))) });
              }}
              className="w-24"
            />
            <p className="text-caption text-muted-foreground">{t(($) => $.inspector.max_rewinds_hint)}</p>
          </div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 border-t pt-4 text-caption">
            <PropRow label={t(($) => $.inspector.created_by)}>
              <span className="flex min-w-0 items-center gap-1.5">
                <ActorAvatar actorType="member" actorId={workflow.creator_id} size="xs" />
                <span className="truncate">{creatorName ?? workflow.creator_id.slice(0, 8)}</span>
              </span>
            </PropRow>
            <PropRow label={t(($) => $.inspector.updated)}>
              <UpdatedAt value={workflow.updated_at} />
            </PropRow>
          </dl>
        </aside>
      </div>

      {pendingTab !== null && (
        <AlertDialog open onOpenChange={(v) => { if (!v) setPendingTab(null); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.detail.discard_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.detail.discard_dialog.description)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>{t(($) => $.detail.discard_dialog.keep_editing)}</AlertDialogCancel>
              <AlertDialogAction
                variant="destructive"
                onClick={() => {
                  discard();
                  setTab(pendingTab);
                  setPendingTab(null);
                }}
              >
                {t(($) => $.detail.discard_dialog.discard)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}

      {confirmArchive && (
        <AlertDialog open onOpenChange={(v) => { if (!v && !archive.isPending) setConfirmArchive(false); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.archive_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>
                {t(($) => $.archive_dialog.description, { name: workflow.name })}
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={archive.isPending}>{t(($) => $.archive_dialog.cancel)}</AlertDialogCancel>
              <AlertDialogAction
                onClick={() => void doArchive()}
                disabled={archive.isPending}
                className="bg-destructive text-white hover:bg-destructive/90"
              >
                {archive.isPending ? t(($) => $.archive_dialog.archiving) : t(($) => $.archive_dialog.confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}

function PropRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

function UpdatedAt({ value }: { value: string }) {
  const timeAgo = useTimeAgo();
  return <span className="text-muted-foreground">{timeAgo(value)}</span>;
}

function DetailSkeleton() {
  return (
    <div className="flex flex-1 flex-col gap-3 p-6 md:grid md:grid-cols-[minmax(0,1fr)_320px]">
      <Skeleton className="h-64 w-full" />
      <Skeleton className="h-64 w-full" />
    </div>
  );
}
