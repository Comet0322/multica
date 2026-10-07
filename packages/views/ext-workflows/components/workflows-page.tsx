"use client";

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, ExternalLink, Loader2, MoreHorizontal, Plus, Trash2, Workflow as WorkflowIcon } from "lucide-react";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import {
  extWorkflowListOptions,
  useArchiveExtWorkflow,
  type ExtWorkflow,
} from "@multica/core/ext-workflows";
import { useModalStore } from "@multica/core/modals";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { resolvePublicFileUrl } from "@multica/core/workspace/avatar-url";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import type { Agent, MemberWithUser } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  ListGrid,
  ListGridCell,
  ListGridHeader,
  ListGridHeaderCell,
  ListGridRow,
  LIST_GRID_BOTTOM_CLEARANCE,
} from "@multica/ui/components/ui/list-grid";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import { ActorAvatar } from "../../common/actor-avatar";
import { useT, useTimeAgo } from "../../i18n";
import {
  CollectionPageHeader,
  CollectionPageHeaderAction,
  CollectionPageState,
} from "../../layout/collection-page";
import { useIntentNavigate, useRowLink } from "../../navigation";

// Name + supervisor are the core set (< @2xl); nodes / active runs / last run
// appear from @2xl. The kebab track is constant: it is empty for rows the
// viewer cannot manage.
const GRID_COLS =
  "grid-cols-[0.75rem_minmax(120px,1fr)_160px_1.75rem_0.75rem] " +
  "@2xl:grid-cols-[0.75rem_minmax(200px,1fr)_160px_88px_104px_128px_1.75rem_0.75rem]";

function initialsOf(name: string): string {
  return name
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);
}

function WorkflowAvatar({ workflow }: { workflow: ExtWorkflow }) {
  return (
    <ActorAvatarBase
      name={workflow.name}
      initials={initialsOf(workflow.name)}
      avatarUrl={workflow.avatar_url ? resolvePublicFileUrl(workflow.avatar_url) : undefined}
      isWorkflow
      size="lg"
      className="shrink-0"
    />
  );
}

function ArchiveWorkflowDialog({
  workflow,
  open,
  onOpenChange,
}: {
  workflow: ExtWorkflow;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("ext-workflows");
  const wsId = useCurrentWorkspace()?.id ?? "";
  const archive = useArchiveExtWorkflow(wsId);
  const confirm = async () => {
    try {
      await archive.mutateAsync(workflow.id);
      onOpenChange(false);
      toast.success(t(($) => $.archive_dialog.success));
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.archive_dialog.failed));
    }
  };
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t(($) => $.archive_dialog.title)}</DialogTitle>
          <DialogDescription>
            {t(($) => $.archive_dialog.description, { name: workflow.name })}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={archive.isPending}
            onClick={() => onOpenChange(false)}
          >
            {t(($) => $.archive_dialog.cancel)}
          </Button>
          <Button
            type="button"
            variant="destructive"
            size="sm"
            disabled={archive.isPending}
            aria-busy={archive.isPending}
            onClick={() => void confirm()}
          >
            {archive.isPending ? (
              <>
                <Loader2 className="mr-1 size-3.5 animate-spin" />
                {t(($) => $.archive_dialog.archiving)}
              </>
            ) : (
              t(($) => $.archive_dialog.confirm)
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function WorkflowRowActions({ workflow }: { workflow: ExtWorkflow }) {
  const { t } = useT("ext-workflows");
  const { t: tCommon } = useT("common");
  const p = useWorkspacePaths();
  const intentNavigate = useIntentNavigate();
  const [archiveOpen, setArchiveOpen] = useState(false);
  return (
    <span onClick={(e) => e.stopPropagation()} className="flex items-center">
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <button
              type="button"
              aria-label={t(($) => $.page.row_menu)}
              className="flex size-7 items-center justify-center rounded-md text-muted-foreground opacity-0 transition-opacity hover:bg-accent hover:text-accent-foreground group-hover/row:opacity-100 focus-visible:opacity-100 data-popup-open:bg-accent data-popup-open:opacity-100 data-popup-open:text-accent-foreground"
            >
              <MoreHorizontal className="size-4" />
            </button>
          }
        />
        <DropdownMenuContent align="end" className="w-40">
          <DropdownMenuItem
            onClick={() => intentNavigate(p.workflowDetail(workflow.id), "foreground-tab", workflow.name)}
          >
            <ExternalLink className="size-3.5" />
            {tCommon(($) => $.navigation.open_in_new_tab)}
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem variant="destructive" onClick={() => setArchiveOpen(true)}>
            <Trash2 className="size-3.5" />
            {t(($) => $.page.archive_action)}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <ArchiveWorkflowDialog workflow={workflow} open={archiveOpen} onOpenChange={setArchiveOpen} />
    </span>
  );
}

export function WorkflowsPage() {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const wsId = useCurrentWorkspace()?.id ?? "";
  const p = useWorkspacePaths();
  const rowLink = useRowLink();
  const currentUser = useAuthStore((s) => s.user);

  const {
    data: workflows = [],
    isLoading,
    isError,
    error,
    refetch,
  } = useQuery({
    ...extWorkflowListOptions(wsId),
    enabled: !!wsId,
  });
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: members = [] } = useQuery(memberListOptions(wsId));

  const agentsById = useMemo(() => new Map(agents.map((a: Agent) => [a.id, a])), [agents]);
  const isWorkspaceAdmin = useMemo(() => {
    const me = members.find((m: MemberWithUser) => m.user_id === currentUser?.id);
    return me?.role === "owner" || me?.role === "admin";
  }, [members, currentUser]);
  // Mirrors canManageSquad: workspace admins manage all, creators manage their own.
  const canManage = (w: ExtWorkflow) =>
    isWorkspaceAdmin || (!!currentUser && w.creator_id === currentUser.id);

  const openCreate = () => useModalStore.getState().open("create-ext-workflow");

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <CollectionPageHeader
        icon={WorkflowIcon}
        title={t(($) => $.page.title)}
        count={workflows.length}
        actions={
          <CollectionPageHeaderAction icon={Plus} label={t(($) => $.page.new_button)} onClick={openCreate} />
        }
      />

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <CollectionPageState
          role="alert"
          tone="destructive"
          icon={AlertCircle}
          title={t(($) => $.page.load_failed)}
          description={error instanceof Error ? error.message : undefined}
          actions={
            <Button type="button" variant="outline" size="sm" onClick={() => void refetch()}>
              {t(($) => $.page.retry)}
            </Button>
          }
        />
      ) : workflows.length === 0 ? (
        <CollectionPageState
          icon={WorkflowIcon}
          title={t(($) => $.page.empty)}
          actions={
            <Button size="sm" onClick={openCreate}>
              <Plus aria-hidden="true" className="size-3.5" />
              {t(($) => $.page.new_button)}
            </Button>
          }
        />
      ) : (
        <div className="@container min-h-0 flex-1 overflow-auto">
          <ListGrid className={GRID_COLS} style={{ paddingBottom: LIST_GRID_BOTTOM_CLEARANCE }}>
            <ListGridHeader>
              <ListGridHeaderCell>{t(($) => $.page.table.name)}</ListGridHeaderCell>
              <ListGridHeaderCell>{t(($) => $.page.table.supervisor)}</ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">{t(($) => $.page.table.nodes)}</ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">
                {t(($) => $.page.table.active_runs)}
              </ListGridHeaderCell>
              <ListGridHeaderCell className="hidden @2xl:flex">{t(($) => $.page.table.last_run)}</ListGridHeaderCell>
              <span aria-hidden="true" />
            </ListGridHeader>
            {workflows.map((w: ExtWorkflow) => {
              const supervisor = agentsById.get(w.supervisor_agent_id);
              return (
                <ListGridRow key={w.id} className="cursor-pointer" {...rowLink(p.workflowDetail(w.id), w.name)}>
                  <ListGridCell className="gap-3">
                    <WorkflowAvatar workflow={w} />
                    <div className="min-w-0 flex-1">
                      <span className="block min-w-0 truncate text-body font-medium">{w.name}</span>
                      {w.description ? (
                        <span className="block min-w-0 truncate text-caption text-muted-foreground">
                          {w.description}
                        </span>
                      ) : null}
                    </div>
                  </ListGridCell>
                  <ListGridCell className="gap-1.5">
                    <ActorAvatar actorType="agent" actorId={w.supervisor_agent_id} size="sm" />
                    <span className="min-w-0 truncate text-caption text-muted-foreground">
                      {supervisor?.name ?? w.supervisor_agent_id.slice(0, 8)}
                    </span>
                  </ListGridCell>
                  <ListGridCell className="hidden text-caption tabular-nums text-muted-foreground @2xl:flex">
                    {w.node_count}
                  </ListGridCell>
                  <ListGridCell className="hidden text-caption tabular-nums text-muted-foreground @2xl:flex">
                    {w.active_run_count}
                  </ListGridCell>
                  <ListGridCell className="hidden whitespace-nowrap text-caption text-muted-foreground @2xl:flex">
                    {w.last_run_at ? timeAgo(w.last_run_at) : t(($) => $.page.never_run)}
                  </ListGridCell>
                  <ListGridCell className="justify-end px-0">
                    {canManage(w) ? <WorkflowRowActions workflow={w} /> : null}
                  </ListGridCell>
                </ListGridRow>
              );
            })}
          </ListGrid>
        </div>
      )}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className="@container min-h-0 flex-1 overflow-auto">
      <ListGrid className={GRID_COLS}>
        <ListGridHeader>
          <ListGridHeaderCell>
            <Skeleton className="h-3 w-12" />
          </ListGridHeaderCell>
          <ListGridHeaderCell>
            <Skeleton className="h-3 w-12" />
          </ListGridHeaderCell>
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <ListGridHeaderCell className="hidden @2xl:flex" />
          <span aria-hidden="true" />
        </ListGridHeader>
        {Array.from({ length: 3 }).map((_, i) => (
          <ListGridRow key={i} className="h-16 hover:bg-transparent">
            <ListGridCell className="gap-3">
              <Skeleton className="size-8 rounded-full" />
              <Skeleton className="h-3.5 w-32 max-w-full" />
            </ListGridCell>
            <ListGridCell className="gap-1.5">
              <Skeleton className="size-5 rounded-full" />
              <Skeleton className="h-3 w-16" />
            </ListGridCell>
            <ListGridCell className="hidden @2xl:flex" />
            <ListGridCell className="hidden @2xl:flex" />
            <ListGridCell className="hidden @2xl:flex" />
            <span aria-hidden="true" />
          </ListGridRow>
        ))}
      </ListGrid>
    </div>
  );
}
