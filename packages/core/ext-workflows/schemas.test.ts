import { describe, expect, it } from "vitest";
import {
  ExtWorkflowIssueRunsSchema,
  ExtWorkflowListSchema,
  ExtWorkflowRunListSchema,
  ExtWorkflowRunSchema,
  ExtWorkflowSchema,
  ExtWorkflowValidationBodySchema,
} from "./schemas";

const baseWorkflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Release",
  supervisor_agent_id: "agent-1",
  creator_id: "user-1",
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

const baseRun = {
  id: "run-1",
  workflow_id: "wf-1",
  issue_id: "issue-1",
};

describe("ExtWorkflowSchema drift tolerance", () => {
  it("defaults every optional field when an older backend omits it", () => {
    const parsed = ExtWorkflowSchema.parse(baseWorkflow);
    expect(parsed.description).toBe("");
    expect(parsed.max_rewinds).toBe(3);
    expect(parsed.avatar_url).toBeNull();
    expect(parsed.archived_at).toBeNull();
    expect(parsed.last_run_at).toBeNull();
    expect(parsed.node_count).toBe(0);
    expect(parsed.active_run_count).toBe(0);
    expect(parsed.nodes).toEqual([]);
  });

  it("treats explicit nulls like missing values", () => {
    const parsed = ExtWorkflowSchema.parse({
      ...baseWorkflow,
      avatar_url: null,
      archived_at: null,
      last_run_at: null,
      nodes: null,
    });
    expect(parsed.nodes).toEqual([]);
    expect(parsed.avatar_url).toBeNull();
  });

  it("keeps unknown extra fields", () => {
    const parsed = ExtWorkflowSchema.parse({ ...baseWorkflow, future_field: 1 }) as Record<string, unknown>;
    expect(parsed.future_field).toBe(1);
  });

  it("normalises node depends_on null to an empty list and defaults node fields", () => {
    const parsed = ExtWorkflowSchema.parse({
      ...baseWorkflow,
      nodes: [{ id: "n1", key: "build", agent_id: "agent-2", depends_on: null }],
    });
    expect(parsed.nodes[0]).toMatchObject({
      key: "build",
      title: "",
      prompt: "",
      requires_review: false,
      max_attempts: 3,
      depends_on: [],
      position: 0,
    });
  });

  it("rejects a response without the identity fields so the caller falls back", () => {
    expect(ExtWorkflowSchema.safeParse({ name: "no id" }).success).toBe(false);
    expect(ExtWorkflowSchema.safeParse("not an object").success).toBe(false);
  });
});

describe("ExtWorkflowListSchema", () => {
  it("defaults a missing workflows array", () => {
    expect(ExtWorkflowListSchema.parse({}).workflows).toEqual([]);
  });

  it("rejects a bare array (the old squad-style shape) so the caller falls back", () => {
    expect(ExtWorkflowListSchema.safeParse([baseWorkflow]).success).toBe(false);
  });
});

describe("ExtWorkflowRunSchema enum fallbacks", () => {
  it("maps an unknown run status and unknown step status to 'unknown'", () => {
    const parsed = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "paused_by_future_feature",
      steps: [{ id: "s1", node_key: "a", status: "teleporting" }],
    });
    expect(parsed.status).toBe("unknown");
    expect(parsed.steps[0]?.status).toBe("unknown");
  });

  it("maps a missing or non-string status to 'unknown'", () => {
    expect(ExtWorkflowRunSchema.parse({ ...baseRun }).status).toBe("unknown");
    expect(ExtWorkflowRunSchema.parse({ ...baseRun, status: 7 }).status).toBe("unknown");
  });

  it("passes known statuses through", () => {
    const parsed = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "waiting_human",
      steps: [{ id: "s1", node_key: "a", status: "awaiting_human" }],
    });
    expect(parsed.status).toBe("waiting_human");
    expect(parsed.steps[0]?.status).toBe("awaiting_human");
  });

  it("defaults steps, events, and nullable step fields", () => {
    const parsed = ExtWorkflowRunSchema.parse({ ...baseRun, status: "running", steps: null, events: undefined });
    expect(parsed.steps).toEqual([]);
    expect(parsed.events).toEqual([]);
    expect(parsed.finished_at).toBeNull();

    const withStep = ExtWorkflowRunSchema.parse({
      ...baseRun,
      status: "running",
      steps: [{ id: "s1", node_key: "a", status: "pending" }],
      events: [{ id: "e1", kind: "run_started", payload: null }],
    });
    expect(withStep.steps[0]).toMatchObject({
      attempts: 0,
      max_attempts: 1,
      depends_on: [],
      pending_reason: null,
      last_feedback: null,
      escalation_reason: null,
      started_at: null,
      finished_at: null,
    });
    expect(withStep.events[0]).toMatchObject({ step_id: null, actor_id: null, on_behalf_of: null, payload: {} });
  });
});

describe("run list and issue runs schemas", () => {
  it("defaults list fields", () => {
    expect(ExtWorkflowRunListSchema.parse({})).toEqual({ runs: [], total: 0 });
  });

  it("defaults step_of to null and keeps a child issue's step_of", () => {
    expect(ExtWorkflowIssueRunsSchema.parse({ runs: [] }).step_of).toBeNull();
    const parsed = ExtWorkflowIssueRunsSchema.parse({
      runs: [{ ...baseRun, status: "running" }],
      step_of: { run_id: "run-1", step_id: "s1", parent_issue_id: "p1", node_key: "a", index: 1, total: 3 },
    });
    expect(parsed.step_of).toMatchObject({ index: 1, total: 3, parent_issue_id: "p1" });
  });
});

describe("ExtWorkflowValidationBodySchema", () => {
  it("parses the 422 body and defaults missing parts", () => {
    const parsed = ExtWorkflowValidationBodySchema.parse({
      error: "validation_failed",
      errors: [{ node_key: "a", field: "depends_on", message: "cycle" }, { field: "nodes" }],
    });
    expect(parsed.errors[0]).toMatchObject({ node_key: "a", field: "depends_on", message: "cycle" });
    expect(parsed.errors[1]).toMatchObject({ field: "nodes", message: "" });
  });

  it("does not match other error bodies", () => {
    expect(ExtWorkflowValidationBodySchema.safeParse({ error: "forbidden" }).success).toBe(false);
  });
});
