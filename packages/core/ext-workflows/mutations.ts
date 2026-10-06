import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { extWorkflowKeys } from "./queries";
import type {
  CreateExtWorkflowRequest,
  DecideExtWorkflowStepRequest,
  ExtWorkflow,
  UpdateExtWorkflowRequest,
} from "./types";

export function useCreateExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateExtWorkflowRequest) => api.createExtWorkflow(data),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.list(wsId) });
    },
  });
}

// Not optimistic: a save can be rejected with a 422 that the editor maps onto
// rows, so the cache only moves once the server has accepted the definition.
export function useUpdateExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & UpdateExtWorkflowRequest) =>
      api.updateExtWorkflow(id, data),
    onSuccess: (workflow) => {
      qc.setQueryData<ExtWorkflow>(extWorkflowKeys.detail(wsId, workflow.id), workflow);
    },
    onSettled: (_data, _err, vars) => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.detail(wsId, vars.id) });
      qc.invalidateQueries({ queryKey: extWorkflowKeys.list(wsId) });
    },
  });
}

export function useArchiveExtWorkflow(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.archiveExtWorkflow(id),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
    },
  });
}

// Decisions and cancels are not optimistic: the engine is the source of truth
// for the resulting step and run state, and a stale expected_status answers 409.
export function useDecideExtWorkflowStep(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      runId,
      stepId,
      ...body
    }: { runId: string; stepId: string } & DecideExtWorkflowStepRequest) =>
      api.decideExtWorkflowStep(runId, stepId, body),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
    },
  });
}

export function useCancelExtWorkflowRun(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (runId: string) => api.cancelExtWorkflowRun(runId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: extWorkflowKeys.runsAll(wsId) });
      qc.invalidateQueries({ queryKey: extWorkflowKeys.all(wsId) });
    },
  });
}
