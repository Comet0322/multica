import { z } from "zod";
import type {
  ExtWorkflow,
  ExtWorkflowIssueRuns,
  ExtWorkflowRun,
  ExtWorkflowRunList,
  ExtWorkflowRunStatus,
  ExtWorkflowStepStatus,
  ExtWorkflowValidationError,
} from "./types";

// ext-workflow: lenient response schemas. Every optional field has a default
// and every enum falls back to "unknown", so an older client talking to a newer
// backend keeps rendering. Parsed with `parseWithFallback` in ApiClient.

const RUN_STATUSES: readonly string[] = ["running", "waiting_human", "done", "failed", "cancelled"];
const STEP_STATUSES: readonly string[] = [
  "pending",
  "running",
  "awaiting_supervisor",
  "awaiting_human",
  "done",
  "skipped",
  "failed",
  "cancelled",
];

const nullableString = z
  .string()
  .nullable()
  .optional()
  .transform((v) => v ?? null);

export const ExtWorkflowRunStatusSchema = z
  .string()
  .transform((v): ExtWorkflowRunStatus => (RUN_STATUSES.includes(v) ? (v as ExtWorkflowRunStatus) : "unknown"))
  .catch("unknown");

export const ExtWorkflowStepStatusSchema = z
  .string()
  .transform((v): ExtWorkflowStepStatus => (STEP_STATUSES.includes(v) ? (v as ExtWorkflowStepStatus) : "unknown"))
  .catch("unknown");

export const ExtWorkflowNodeSchema = z
  .object({
    id: z.string(),
    key: z.string(),
    title: z.string().default(""),
    agent_id: z.string(),
    prompt: z.string().default(""),
    requires_review: z.boolean().default(false),
    max_attempts: z.number().default(3),
    depends_on: z.array(z.string()).nullable().optional().transform((v) => v ?? []),
    position: z.number().default(0),
  })
  .loose();

export const ExtWorkflowSchema = z
  .object({
    id: z.string(),
    workspace_id: z.string(),
    name: z.string(),
    description: z.string().default(""),
    supervisor_agent_id: z.string(),
    max_rewinds: z.number().default(3),
    creator_id: z.string(),
    avatar_url: nullableString,
    archived_at: nullableString,
    created_at: z.string(),
    updated_at: z.string(),
    node_count: z.number().default(0),
    active_run_count: z.number().default(0),
    last_run_at: nullableString,
    nodes: z.array(ExtWorkflowNodeSchema).nullable().optional().transform((v) => v ?? []),
  })
  .loose();

export const ExtWorkflowListSchema = z.object({
  workflows: z.array(ExtWorkflowSchema).default([]),
});

export const ExtWorkflowValidationErrorSchema = z
  .object({
    node_key: z.string().optional(),
    field: z.string().default(""),
    message: z.string().default(""),
  })
  .loose();

export const ExtWorkflowValidationBodySchema = z.object({
  error: z.literal("validation_failed"),
  errors: z.array(ExtWorkflowValidationErrorSchema).default([]),
});

export const ExtWorkflowRunSummarySchema = z
  .object({
    id: z.string(),
    workspace_id: z.string().default(""),
    workflow_id: z.string(),
    workflow_name: z.string().default(""),
    issue_id: z.string(),
    issue_identifier: z.string().default(""),
    issue_title: z.string().default(""),
    triggered_by_type: z.string().default(""),
    triggered_by_id: z.string().default(""),
    status: ExtWorkflowRunStatusSchema,
    rewinds_used: z.number().default(0),
    max_rewinds: z.number().default(0),
    started_at: z.string().default(""),
    finished_at: nullableString,
    created_at: z.string().default(""),
    updated_at: z.string().default(""),
  })
  .loose();

export const ExtWorkflowStepSchema = z
  .object({
    id: z.string(),
    node_key: z.string(),
    title: z.string().default(""),
    agent_id: z.string().default(""),
    issue_id: z.string().default(""),
    status: ExtWorkflowStepStatusSchema,
    attempts: z.number().default(0),
    max_attempts: z.number().default(1),
    requires_review: z.boolean().default(false),
    depends_on: z.array(z.string()).nullable().optional().transform((v) => v ?? []),
    pending_reason: nullableString,
    last_feedback: nullableString,
    escalation_reason: nullableString,
    started_at: nullableString,
    finished_at: nullableString,
  })
  .loose();

export const ExtWorkflowRunEventSchema = z
  .object({
    id: z.string(),
    step_id: nullableString,
    kind: z.string().default(""),
    actor_type: z.string().default(""),
    actor_id: nullableString,
    on_behalf_of: nullableString,
    payload: z.record(z.string(), z.unknown()).nullable().optional().transform((v) => v ?? {}),
    created_at: z.string().default(""),
  })
  .loose();

export const ExtWorkflowRunSchema = ExtWorkflowRunSummarySchema.extend({
  steps: z.array(ExtWorkflowStepSchema).nullable().optional().transform((v) => v ?? []),
  events: z.array(ExtWorkflowRunEventSchema).nullable().optional().transform((v) => v ?? []),
});

export const ExtWorkflowRunListSchema = z.object({
  runs: z.array(ExtWorkflowRunSummarySchema).default([]),
  total: z.number().default(0),
});

export const ExtWorkflowStepOfSchema = z
  .object({
    run_id: z.string(),
    step_id: z.string(),
    node_key: z.string().default(""),
    index: z.number().default(0),
    total: z.number().default(0),
    parent_issue_id: z.string(),
  })
  .loose();

export const ExtWorkflowIssueRunsSchema = z.object({
  runs: z.array(ExtWorkflowRunSummarySchema).default([]),
  step_of: ExtWorkflowStepOfSchema.nullable().optional().transform((v) => v ?? null),
});

// Fallbacks handed to parseWithFallback when a response cannot be parsed at
// all: empty, renderable, never throwing.
export const EMPTY_EXT_WORKFLOW_LIST: { workflows: ExtWorkflow[] } = { workflows: [] };

export const EMPTY_EXT_WORKFLOW: ExtWorkflow = {
  id: "",
  workspace_id: "",
  name: "",
  description: "",
  supervisor_agent_id: "",
  max_rewinds: 3,
  creator_id: "",
  avatar_url: null,
  archived_at: null,
  created_at: "",
  updated_at: "",
  node_count: 0,
  active_run_count: 0,
  last_run_at: null,
  nodes: [],
};

export const EMPTY_EXT_WORKFLOW_RUN_LIST: ExtWorkflowRunList = { runs: [], total: 0 };

export const EMPTY_EXT_WORKFLOW_ISSUE_RUNS: ExtWorkflowIssueRuns = { runs: [], step_of: null };

export const EMPTY_EXT_WORKFLOW_RUN: ExtWorkflowRun = {
  id: "",
  workspace_id: "",
  workflow_id: "",
  workflow_name: "",
  issue_id: "",
  issue_identifier: "",
  issue_title: "",
  triggered_by_type: "",
  triggered_by_id: "",
  status: "unknown",
  rewinds_used: 0,
  max_rewinds: 0,
  started_at: "",
  finished_at: null,
  created_at: "",
  updated_at: "",
  steps: [],
  events: [],
};

export type { ExtWorkflowValidationError };
