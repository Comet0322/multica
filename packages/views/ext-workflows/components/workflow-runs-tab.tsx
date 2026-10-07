"use client";

import { AlertCircle } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { extWorkflowRunsOptions } from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@multica/ui/components/ui/table";
import { useT, useTimeAgo } from "../../i18n";
import { CollectionPageState } from "../../layout/collection-page";
import { useRowLink } from "../../navigation";
import { RunStatusBadge } from "./status";

export function WorkflowRunsTab({ workflowId }: { workflowId: string }) {
  const { t } = useT("ext-workflows");
  const timeAgo = useTimeAgo();
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const rowLink = useRowLink();
  const { data, isLoading, isError, error, refetch } = useQuery({
    ...extWorkflowRunsOptions(wsId, workflowId),
    enabled: !!wsId && !!workflowId,
  });
  const runs = data?.runs ?? [];

  if (isLoading) return <Skeleton className="h-24 w-full" />;
  if (isError && !data) {
    return (
      <CollectionPageState
        role="alert"
        tone="destructive"
        icon={AlertCircle}
        className="py-8"
        title={t(($) => $.runs.load_failed)}
        description={error instanceof Error ? error.message : undefined}
        actions={
          <Button type="button" variant="outline" size="sm" onClick={() => void refetch()}>
            {t(($) => $.runs.retry)}
          </Button>
        }
      />
    );
  }
  if (runs.length === 0) {
    return <p className="py-8 text-center text-body text-muted-foreground">{t(($) => $.runs.empty)}</p>;
  }

  return (
    <div className="space-y-2">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t(($) => $.runs.col_issue)}</TableHead>
            <TableHead>{t(($) => $.runs.col_status)}</TableHead>
            <TableHead>{t(($) => $.runs.col_started)}</TableHead>
            <TableHead>{t(($) => $.runs.col_finished)}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {runs.map((run) => (
            <TableRow
              key={run.id}
              className="cursor-pointer"
              {...rowLink(p.issueDetail(run.issue_id), run.issue_identifier)}
            >
              <TableCell className="max-w-0">
                <div className="flex min-w-0 items-center gap-2">
                  <span className="shrink-0 text-caption text-muted-foreground">{run.issue_identifier}</span>
                  <span className="truncate">{run.issue_title}</span>
                </div>
              </TableCell>
              <TableCell>
                <RunStatusBadge status={run.status} />
              </TableCell>
              <TableCell className="whitespace-nowrap text-caption text-muted-foreground">
                {timeAgo(run.started_at)}
              </TableCell>
              <TableCell className="whitespace-nowrap text-caption text-muted-foreground">
                {run.finished_at ? timeAgo(run.finished_at) : "—"}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {data && data.total > runs.length && (
        <p className="text-caption text-muted-foreground">
          {t(($) => $.runs.showing, { shown: runs.length, total: data.total })}
        </p>
      )}
    </div>
  );
}
