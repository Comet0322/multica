import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

// ext-workflow: every key is workspace-scoped. Run keys live under a separate
// root so a template edit does not refetch run history and vice versa.
export const extWorkflowKeys = {
  all: (wsId: string) => ["ext-workflows", wsId] as const,
  list: (wsId: string) => [...extWorkflowKeys.all(wsId), "list"] as const,
  detail: (wsId: string, id: string) => [...extWorkflowKeys.all(wsId), "detail", id] as const,
  runs: (wsId: string, id: string) => [...extWorkflowKeys.all(wsId), "runs", id] as const,
  runsAll: (wsId: string) => ["ext-workflow-runs", wsId] as const,
  issueRuns: (wsId: string, issueId: string) => ["ext-workflow-runs", wsId, "issue", issueId] as const,
  run: (wsId: string, runId: string) => ["ext-workflow-runs", wsId, "run", runId] as const,
};

export function extWorkflowListOptions(wsId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.list(wsId),
    queryFn: () => api.listExtWorkflows(),
    enabled: !!wsId,
  });
}

export function extWorkflowDetailOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.detail(wsId, id),
    queryFn: () => api.getExtWorkflow(id),
    enabled: !!wsId && !!id,
  });
}

export function extWorkflowRunsOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.runs(wsId, id),
    queryFn: () => api.listExtWorkflowRuns(id),
    enabled: !!wsId && !!id,
  });
}

export function extWorkflowIssueRunsOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.issueRuns(wsId, issueId),
    queryFn: () => api.getIssueExtWorkflowRuns(issueId),
    enabled: !!wsId && !!issueId,
  });
}

export function extWorkflowRunOptions(wsId: string, runId: string) {
  return queryOptions({
    queryKey: extWorkflowKeys.run(wsId, runId),
    queryFn: () => api.getExtWorkflowRun(runId),
    enabled: !!wsId && !!runId,
  });
}
