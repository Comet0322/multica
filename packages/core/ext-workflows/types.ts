// ext-workflow: wire types for the first-class workflow feature. Mirrors the
// JSON the Go handlers in server/internal/handler/ext_workflow*.go produce.

export type ExtWorkflowRunStatus =
  | "running"
  | "waiting_human"
  | "done"
  | "failed"
  | "cancelled"
  /** Any status the server adds later; render neutrally. */
  | "unknown";

export type ExtWorkflowStepStatus =
  | "pending"
  | "running"
  | "awaiting_supervisor"
  | "awaiting_human"
  | "done"
  | "skipped"
  | "failed"
  | "cancelled"
  /** Any status the server adds later; render neutrally. */
  | "unknown";

/** Actions a human may take on a step that is awaiting_human. */
export type ExtWorkflowDecisionAction =
  | "approve"
  | "redo"
  | "retry"
  | "skip"
  | "rewind"
  | "abort";

export interface ExtWorkflowNode {
  id: string;
  key: string;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
  position: number;
}

/** One node as sent in `PUT /api/ext/workflows/{id}`. Slice order is the position. */
export interface ExtWorkflowNodeInput {
  key: string;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
}

export interface ExtWorkflow {
  id: string;
  workspace_id: string;
  name: string;
  description: string;
  supervisor_agent_id: string;
  max_rewinds: number;
  creator_id: string;
  avatar_url: string | null;
  archived_at: string | null;
  created_at: string;
  updated_at: string;
  node_count: number;
  active_run_count: number;
  last_run_at: string | null;
  /** Populated by the detail endpoint only; list entries carry `[]`. */
  nodes: ExtWorkflowNode[];
}

export interface CreateExtWorkflowRequest {
  name: string;
  description?: string;
  supervisor_agent_id: string;
}

export interface UpdateExtWorkflowRequest {
  name?: string;
  description?: string;
  supervisor_agent_id?: string;
  max_rewinds?: number;
  /** Replaces the whole node set when present. */
  nodes?: ExtWorkflowNodeInput[];
}

export interface ExtWorkflowValidationError {
  node_key?: string;
  field: string;
  message: string;
}

export interface ExtWorkflowRunSummary {
  id: string;
  workspace_id: string;
  workflow_id: string;
  workflow_name: string;
  issue_id: string;
  issue_identifier: string;
  issue_title: string;
  triggered_by_type: string;
  triggered_by_id: string;
  status: ExtWorkflowRunStatus;
  rewinds_used: number;
  max_rewinds: number;
  started_at: string;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface ExtWorkflowStep {
  id: string;
  node_key: string;
  title: string;
  agent_id: string;
  issue_id: string;
  status: ExtWorkflowStepStatus;
  attempts: number;
  max_attempts: number;
  requires_review: boolean;
  depends_on: string[];
  pending_reason: string | null;
  last_feedback: string | null;
  escalation_reason: string | null;
  started_at: string | null;
  finished_at: string | null;
}

export interface ExtWorkflowRunEvent {
  id: string;
  step_id: string | null;
  kind: string;
  actor_type: string;
  actor_id: string | null;
  on_behalf_of: string | null;
  payload: Record<string, unknown>;
  created_at: string;
}

export interface ExtWorkflowRun extends ExtWorkflowRunSummary {
  steps: ExtWorkflowStep[];
  events: ExtWorkflowRunEvent[];
}

export interface ExtWorkflowRunList {
  runs: ExtWorkflowRunSummary[];
  total: number;
}

/** Set when the queried issue is itself a child issue (a step) of a run. */
export interface ExtWorkflowStepOf {
  run_id: string;
  step_id: string;
  node_key: string;
  index: number;
  total: number;
  parent_issue_id: string;
}

export interface ExtWorkflowIssueRuns {
  runs: ExtWorkflowRunSummary[];
  step_of: ExtWorkflowStepOf | null;
}

export interface DecideExtWorkflowStepRequest {
  action: ExtWorkflowDecisionAction;
  to?: string;
  reason?: string;
  feedback?: string;
  /** The step status the caller saw; the server answers 409 on mismatch. */
  expected_status: ExtWorkflowStepStatus;
}
