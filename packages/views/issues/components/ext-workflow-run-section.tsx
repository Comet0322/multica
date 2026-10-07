"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, ExternalLink } from "lucide-react";
import { toast } from "sonner";
import {
  extWorkflowDetailOptions,
  extWorkflowIssueRunsOptions,
  extWorkflowRunOptions,
  useCancelExtWorkflowRun,
  useDecideExtWorkflowStep,
  type ExtWorkflowDecisionAction,
  type ExtWorkflowRun,
  type ExtWorkflowRunSummary,
  type ExtWorkflowStep,
} from "@multica/core/ext-workflows";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueDetailOptions } from "@multica/core/issues/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { memberListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
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
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { RunStatusBadge, StepStatusIcon, useStepStatusLabel } from "../../ext-workflows/components/status";
import { errorStatus, eventKey, rewindTargets, timelineEntries } from "../../ext-workflows/run-utils";
import { isActiveRunStatus } from "../../ext-workflows/status-keys";
import { useT, useTimeAgo } from "../../i18n";
import { AppLink } from "../../navigation";

// ---------------------------------------------------------------- run section

/** Parent-issue sidebar section: the newest workflow run on this issue. */
export function ExtWorkflowRunSection({ issueId }: { issueId: string }) {
  const wsId = useWorkspaceId();
  const { data } = useQuery({
    ...extWorkflowIssueRunsOptions(wsId, issueId),
    enabled: !!wsId && !!issueId,
  });
  const latest = data?.runs?.[0];
  if (!latest) return null;
  return <RunPanel wsId={wsId} summary={latest} />;
}

function RunPanel({ wsId, summary }: { wsId: string; summary: ExtWorkflowRunSummary }) {
  const { t } = useT("ext-workflows");
  const p = useWorkspacePaths();
  const [open, setOpen] = useState(true);
  const [timelineOpen, setTimelineOpen] = useState(false);
  const [cancelOpen, setCancelOpen] = useState(false);
  const { data: detail } = useQuery({ ...extWorkflowRunOptions(wsId, summary.id), enabled: !!wsId });
  const decide = useDecideExtWorkflowStep(wsId);
  const cancel = useCancelExtWorkflowRun(wsId);

  // Mirrors the server's MemberCanDecide: the triggering member, the workflow
  // creator, or a workspace owner/admin. The server stays authoritative (403).
  const userId = useAuthStore((s) => s.user?.id);
  const { data: members } = useQuery({ ...memberListOptions(wsId), enabled: !!wsId && !!userId });
  const role = members?.find((m) => m.user_id === userId)?.role;
  const isTrigger = summary.triggered_by_type === "member" && summary.triggered_by_id === userId;
  const isAdmin = role === "owner" || role === "admin";
  const { data: workflow } = useQuery({
    ...extWorkflowDetailOptions(wsId, summary.workflow_id),
    enabled: !!wsId && !!userId && !!role && !isTrigger && !isAdmin,
  });
  const canDecide = !!userId && !!role && (isTrigger || isAdmin || workflow?.creator_id === userId);

  const run: ExtWorkflowRunSummary | ExtWorkflowRun = detail ?? summary;
  const steps = detail?.steps ?? [];
  const events = detail?.events ?? [];

  // Returns true when the decision was accepted, so dialogs close only on success.
  const send = async (
    step: ExtWorkflowStep,
    action: ExtWorkflowDecisionAction,
    extra: { to?: string; reason?: string; feedback?: string } = {},
  ): Promise<boolean> => {
    try {
      await decide.mutateAsync({
        runId: summary.id,
        stepId: step.id,
        action,
        ...extra,
        expected_status: step.status,
      });
      toast.success(t(($) => $.run_section.toast.decided));
      return true;
    } catch (err) {
      const status = errorStatus(err);
      if (status === 409) toast.error(t(($) => $.run_section.toast.mismatch));
      else if (status === 403) toast.error(t(($) => $.run_section.toast.forbidden));
      else toast.error(err instanceof Error && err.message ? err.message : t(($) => $.run_section.toast.failed));
      return false;
    }
  };

  const cancelRun = async () => {
    try {
      await cancel.mutateAsync(summary.id);
      toast.success(t(($) => $.run_section.toast.cancelled));
      setCancelOpen(false);
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.run_section.toast.cancel_failed));
    }
  };

  return (
    <div data-ext-workflow-run={run.id}>
      <button
        type="button"
        className={cn(
          "mb-2 flex w-full items-center gap-1 rounded-md px-2 py-1 text-caption font-medium transition-colors hover:bg-accent/70",
          !open && "text-muted-foreground hover:text-foreground",
        )}
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {t(($) => $.run_section.title)}
        <ChevronRight
          className={cn("!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform", open && "rotate-90")}
        />
        <span className="ml-auto">
          <RunStatusBadge status={run.status} />
        </span>
      </button>

      {open && (
        <div className="space-y-2 pl-2">
          <div className="flex items-center gap-2 px-2 text-micro text-muted-foreground">
            <AppLink href={p.workflowDetail(summary.workflow_id)} className="min-w-0 truncate hover:text-foreground">
              {summary.workflow_name}
            </AppLink>
            <span className="shrink-0 tabular-nums">
              {t(($) => $.run_section.rewinds, { used: run.rewinds_used, max: run.max_rewinds })}
            </span>
          </div>

          <ul className="space-y-1">
            {steps.map((step) => (
              <StepRow
                key={step.id}
                step={step}
                run={run}
                steps={steps}
                busy={decide.isPending}
                canDecide={canDecide}
                onSend={send}
              />
            ))}
          </ul>

          <div className="px-2">
            <button
              type="button"
              className="flex items-center gap-1 text-micro font-medium text-muted-foreground hover:text-foreground"
              aria-expanded={timelineOpen}
              onClick={() => setTimelineOpen(!timelineOpen)}
            >
              <ChevronRight className={cn("!size-3 transition-transform", timelineOpen && "rotate-90")} />
              {t(($) => $.run_section.timeline)}
            </button>
            {timelineOpen && <Timeline events={events} />}
          </div>

          {canDecide && isActiveRunStatus(run.status) && (
            <div className="px-2">
              <Button
                type="button"
                variant="ghost"
                size="xs"
                className="text-destructive hover:text-destructive"
                onClick={() => setCancelOpen(true)}
              >
                {t(($) => $.run_section.cancel_run)}
              </Button>
            </div>
          )}
        </div>
      )}

      {cancelOpen && (
        <AlertDialog open onOpenChange={(v) => { if (!v && !cancel.isPending) setCancelOpen(false); }}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>{t(($) => $.run_section.cancel_dialog.title)}</AlertDialogTitle>
              <AlertDialogDescription>{t(($) => $.run_section.cancel_dialog.description)}</AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel disabled={cancel.isPending}>
                {t(($) => $.run_section.cancel_dialog.keep)}
              </AlertDialogCancel>
              <AlertDialogAction
                onClick={() => void cancelRun()}
                disabled={cancel.isPending}
                className="bg-destructive text-white hover:bg-destructive/90"
              >
                {t(($) => $.run_section.cancel_dialog.confirm)}
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- steps

type DialogKind = "redo" | "rewind" | "abort";

function StepRow({
  step,
  run,
  steps,
  busy,
  canDecide,
  onSend,
}: {
  step: ExtWorkflowStep;
  run: Pick<ExtWorkflowRunSummary, "rewinds_used" | "max_rewinds">;
  steps: ExtWorkflowStep[];
  busy: boolean;
  canDecide: boolean;
  onSend: (
    step: ExtWorkflowStep,
    action: ExtWorkflowDecisionAction,
    extra?: { to?: string; reason?: string; feedback?: string },
  ) => Promise<boolean>;
}) {
  const { t } = useT("ext-workflows");
  const p = useWorkspacePaths();
  const { getActorName } = useActorName();
  const statusLabel = useStepStatusLabel();
  const [dialog, setDialog] = useState<DialogKind | null>(null);

  const awaitingHuman = step.status === "awaiting_human" && canDecide;
  const canRetry = step.attempts < step.max_attempts;
  const canRewind = run.rewinds_used < run.max_rewinds;

  return (
    <li className="rounded-md px-2 py-1.5 hover:bg-accent/40">
      <div className="flex items-center gap-2">
        <StepStatusIcon status={step.status} />
        <span className="min-w-0 flex-1 truncate text-caption font-medium">{step.title}</span>
        <span className="shrink-0 text-micro tabular-nums text-muted-foreground">
          {t(($) => $.run_section.attempts, { attempts: step.attempts, max: step.max_attempts })}
        </span>
        <AppLink
          href={p.issueDetail(step.issue_id)}
          aria-label={t(($) => $.run_section.open_child)}
          className="shrink-0 rounded-xs p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <ExternalLink className="size-3" />
        </AppLink>
      </div>
      <div className="mt-0.5 flex min-w-0 items-center gap-1.5 pl-5 text-micro text-muted-foreground">
        <ActorAvatar actorType="agent" actorId={step.agent_id} size="xs" />
        <span className="truncate">{getActorName("agent", step.agent_id)}</span>
        <span aria-hidden="true">·</span>
        <span className="shrink-0">{statusLabel(step.status)}</span>
      </div>

      {awaitingHuman && (
        <div className="mt-2 space-y-2 rounded-md border border-warning/40 bg-warning/5 p-2">
          <p className="text-caption font-medium">{t(($) => $.run_section.needs_decision)}</p>
          {step.escalation_reason && (
            <p className="whitespace-pre-wrap text-caption text-muted-foreground">{step.escalation_reason}</p>
          )}
          <div className="flex flex-wrap gap-1.5">
            <Button type="button" size="xs" disabled={busy} onClick={() => void onSend(step, "approve")}>
              {t(($) => $.run_section.action_approve)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRetry}
              onClick={() => setDialog("redo")}
            >
              {t(($) => $.run_section.action_redo)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRetry}
              onClick={() => void onSend(step, "retry")}
            >
              {t(($) => $.run_section.action_retry)}
            </Button>
            <Button type="button" size="xs" variant="outline" disabled={busy} onClick={() => void onSend(step, "skip")}>
              {t(($) => $.run_section.action_skip)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="outline"
              disabled={busy || !canRewind}
              onClick={() => setDialog("rewind")}
            >
              {t(($) => $.run_section.action_rewind)}
            </Button>
            <Button
              type="button"
              size="xs"
              variant="destructive"
              disabled={busy}
              onClick={() => setDialog("abort")}
            >
              {t(($) => $.run_section.action_abort)}
            </Button>
          </div>
          {!canRetry && <p className="text-micro text-muted-foreground">{t(($) => $.run_section.attempt_limit)}</p>}
          {!canRewind && <p className="text-micro text-muted-foreground">{t(($) => $.run_section.rewind_limit)}</p>}
        </div>
      )}

      {dialog === "redo" && (
        <DecisionDialog
          title={t(($) => $.run_section.redo_dialog.title)}
          fieldLabel={t(($) => $.run_section.redo_dialog.feedback_label)}
          placeholder={t(($) => $.run_section.redo_dialog.feedback_placeholder)}
          confirmLabel={t(($) => $.run_section.redo_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.redo_dialog.cancel)}
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text) => onSend(step, "redo", { feedback: text })}
        />
      )}
      {dialog === "rewind" && (
        <DecisionDialog
          title={t(($) => $.run_section.rewind_dialog.title)}
          fieldLabel={t(($) => $.run_section.rewind_dialog.feedback_label)}
          placeholder={t(($) => $.run_section.rewind_dialog.feedback_placeholder)}
          confirmLabel={t(($) => $.run_section.rewind_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.rewind_dialog.cancel)}
          targetLabel={t(($) => $.run_section.rewind_dialog.target_label)}
          targets={rewindTargets(steps, step.node_key).map((target) => ({
            value: target.key,
            label: target.isSelf
              ? t(($) => $.run_section.rewind_dialog.target_self, { title: target.title })
              : target.title,
          }))}
          defaultTarget={step.node_key}
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text, to) => onSend(step, "rewind", { to, feedback: text })}
        />
      )}
      {dialog === "abort" && (
        <DecisionDialog
          title={t(($) => $.run_section.abort_dialog.title)}
          fieldLabel={t(($) => $.run_section.abort_dialog.reason_label)}
          placeholder={t(($) => $.run_section.abort_dialog.reason_placeholder)}
          confirmLabel={t(($) => $.run_section.abort_dialog.confirm)}
          cancelLabel={t(($) => $.run_section.abort_dialog.cancel)}
          destructive
          pending={busy}
          onClose={() => setDialog(null)}
          onConfirm={(text) => onSend(step, "abort", { reason: text })}
        />
      )}
    </li>
  );
}

function DecisionDialog({
  title,
  fieldLabel,
  placeholder,
  confirmLabel,
  cancelLabel,
  targetLabel,
  targets,
  defaultTarget,
  destructive = false,
  pending,
  onClose,
  onConfirm,
}: {
  title: string;
  fieldLabel: string;
  placeholder: string;
  confirmLabel: string;
  cancelLabel: string;
  targetLabel?: string;
  targets?: { value: string; label: string }[];
  defaultTarget?: string;
  destructive?: boolean;
  pending: boolean;
  onClose: () => void;
  /** Resolves true when the decision was accepted; the dialog then closes. */
  onConfirm: (text: string, target: string) => Promise<boolean>;
}) {
  const [text, setText] = useState("");
  const [target, setTarget] = useState(defaultTarget ?? targets?.[0]?.value ?? "");
  const canConfirm = text.trim() !== "" && !pending;

  const confirm = async () => {
    if (!canConfirm) return;
    if (await onConfirm(text.trim(), target)) onClose();
  };

  return (
    <Dialog open onOpenChange={(v) => { if (!v) onClose(); }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          {targets && targetLabel && (
            <div className="space-y-1.5">
              <Label htmlFor="ext-workflow-rewind-target" className="text-caption text-muted-foreground">
                {targetLabel}
              </Label>
              <select
                id="ext-workflow-rewind-target"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                className="h-8 w-full rounded-lg border border-input bg-transparent px-2.5 text-body outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
              >
                {targets.map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
              </select>
            </div>
          )}
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-decision-text" className="text-caption text-muted-foreground">
              {fieldLabel}
            </Label>
            <Textarea
              id="ext-workflow-decision-text"
              autoFocus
              rows={4}
              value={text}
              placeholder={placeholder}
              onChange={(e) => setText(e.target.value)}
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" size="sm" onClick={onClose}>
            {cancelLabel}
          </Button>
          <Button
            type="button"
            size="sm"
            variant={destructive ? "destructive" : "default"}
            disabled={!canConfirm}
            aria-busy={pending}
            onClick={() => void confirm()}
          >
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------- timeline

function Timeline({ events }: { events: ExtWorkflowRun["events"] }) {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const { getActorName } = useActorName();

  if (events.length === 0) {
    return <p className="mt-1 text-micro text-muted-foreground">{t(($) => $.run_section.timeline_empty)}</p>;
  }
  return (
    <ol className="mt-1 max-h-56 space-y-1.5 overflow-y-auto border-l pl-3">
      {timelineEntries(events).map(({ event, detail }) => {
        const kind = eventKey(event.kind);
        const by =
          event.actor_type === "agent" || event.actor_type === "member"
            ? event.actor_id
              ? getActorName(event.actor_type, event.actor_id)
              : null
            : t(($) => $.run_section.actor_engine);
        const actor =
          by && event.on_behalf_of
            ? t(($) => $.run_section.actor_on_behalf, { actor: by, member: getActorName("member", event.on_behalf_of) })
            : by;
        return (
          <li key={event.id} className="text-micro">
            <div className="flex items-baseline gap-1.5">
              <span className="font-medium">{t(($) => $.events[kind])}</span>
              {actor && <span className="truncate text-muted-foreground">{actor}</span>}
              <span className="ml-auto shrink-0 text-muted-foreground">{timeAgo(event.created_at)}</span>
            </div>
            {detail && <p className="whitespace-pre-wrap text-muted-foreground">{detail}</p>}
          </li>
        );
      })}
    </ol>
  );
}

// ---------------------------------------------------------------- child-issue line

/** One line on a child issue: "Workflow step k of n · MUL-123", linking to the parent. */
export function ExtWorkflowStepLine({ issueId }: { issueId: string }) {
  const { t } = useT("ext-workflows");
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data } = useQuery({
    ...extWorkflowIssueRunsOptions(wsId, issueId),
    enabled: !!wsId && !!issueId,
  });
  const stepOf = data?.step_of ?? null;
  const { data: parent } = useQuery({
    ...issueDetailOptions(wsId, stepOf?.parent_issue_id ?? ""),
    enabled: !!stepOf,
  });
  if (!stepOf) return null;

  const params = { index: stepOf.index, total: stepOf.total };
  return (
    <AppLink
      href={p.issueDetail(stepOf.parent_issue_id)}
      className="flex items-center gap-1.5 rounded-md px-2 py-1 text-caption text-muted-foreground hover:bg-accent/70 hover:text-foreground"
    >
      {parent?.identifier
        ? t(($) => $.run_section.step_line_with_parent, { ...params, parent: parent.identifier })
        : t(($) => $.run_section.step_line, params)}
    </AppLink>
  );
}
