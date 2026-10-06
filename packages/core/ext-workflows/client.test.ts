// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient, ApiError } from "../api/client";
import { ExtWorkflowValidationFailed } from "./errors";

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown, status = 200) {
  const fetch = vi.fn().mockImplementation(
    async () => new Response(status === 204 ? null : JSON.stringify(body), { status }),
  );
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

const workflow = {
  id: "wf-1",
  workspace_id: "ws-1",
  name: "Release",
  supervisor_agent_id: "agent-1",
  creator_id: "user-1",
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
};

const api = () => new ApiClient("https://api.example.test");

function lastCall(fetch: ReturnType<typeof vi.fn>) {
  const [url, init] = fetch.mock.calls.at(-1)!;
  return { url: new URL(url as string), init: init as RequestInit };
}

describe("ext workflow API client", () => {
  it("unwraps the workflow list and applies defaults", async () => {
    const fetch = respond({ workflows: [workflow] });
    const list = await api().listExtWorkflows();
    expect(lastCall(fetch).url.pathname).toBe("/api/ext/workflows");
    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({ id: "wf-1", node_count: 0, nodes: [], max_rewinds: 3 });
  });

  it.each([[[]], [{ workflows: "wrong" }], ["nope"], [null]])(
    "falls back to an empty list on a malformed list response: %j",
    async (body) => {
      respond(body);
      await expect(api().listExtWorkflows()).resolves.toEqual([]);
    },
  );

  it("falls back to an empty workflow when the detail response lacks an id", async () => {
    respond({ name: "no id" });
    await expect(api().getExtWorkflow("wf-1")).resolves.toMatchObject({ id: "", nodes: [] });
  });

  it("creates with POST and parses the response", async () => {
    const fetch = respond(workflow, 201);
    const created = await api().createExtWorkflow({ name: "Release", supervisor_agent_id: "agent-1" });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ name: "Release", supervisor_agent_id: "agent-1" });
    expect(created.id).toBe("wf-1");
  });

  it("sends the node set with PUT", async () => {
    const fetch = respond({ ...workflow, nodes: [] });
    await api().updateExtWorkflow("wf-1", {
      max_rewinds: 2,
      nodes: [
        { key: "a", title: "A", agent_id: "agent-2", prompt: "", requires_review: false, max_attempts: 3, depends_on: [] },
      ],
    });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1");
    expect(init.method).toBe("PUT");
    expect(JSON.parse(init.body as string).nodes[0].key).toBe("a");
  });

  it("throws ExtWorkflowValidationFailed with typed errors on a 422", async () => {
    respond(
      {
        error: "validation_failed",
        errors: [{ node_key: "a", field: "depends_on", message: "node is part of a dependency cycle" }],
      },
      422,
    );
    const err = await api().updateExtWorkflow("wf-1", { nodes: [] }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ExtWorkflowValidationFailed);
    expect((err as ExtWorkflowValidationFailed).errors).toEqual([
      { node_key: "a", field: "depends_on", message: "node is part of a dependency cycle" },
    ]);
  });

  it("keeps a 422 with an unrecognised body as a plain ApiError", async () => {
    respond({ error: "something_else" }, 422);
    await expect(api().updateExtWorkflow("wf-1", { name: "x" })).rejects.toBeInstanceOf(ApiError);
  });

  it("does not convert other failures", async () => {
    respond({ error: "insufficient permissions" }, 403);
    const err = await api().updateExtWorkflow("wf-1", { name: "x" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
  });

  it("archives with DELETE", async () => {
    const fetch = respond(null, 204);
    await expect(api().archiveExtWorkflow("wf-1")).resolves.toBeUndefined();
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1");
    expect(init.method).toBe("DELETE");
  });

  it("passes pagination to the run history and parses it", async () => {
    const fetch = respond({ runs: [{ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "running" }], total: 1 });
    const page = await api().listExtWorkflowRuns("wf-1", { limit: 20, offset: 40 });
    const { url } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflows/wf-1/runs");
    expect(Object.fromEntries(url.searchParams)).toEqual({ limit: "20", offset: "40" });
    expect(page.total).toBe(1);
    expect(page.runs[0]?.status).toBe("running");
  });

  it("queries issue runs by issue id and defaults step_of to null", async () => {
    const fetch = respond({ runs: [] });
    const result = await api().getIssueExtWorkflowRuns("issue 1");
    expect(lastCall(fetch).url.search).toBe("?issue_id=issue%201");
    expect(result).toEqual({ runs: [], step_of: null });
  });

  it("maps an unknown run status to 'unknown' on the run detail", async () => {
    respond({ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "paused_by_future", steps: [{ id: "s1", node_key: "a", status: "warp" }] });
    const run = await api().getExtWorkflowRun("r1");
    expect(run.status).toBe("unknown");
    expect(run.steps[0]?.status).toBe("unknown");
  });

  it("posts a decision with expected_status and parses the returned run", async () => {
    const fetch = respond({ id: "r1", workflow_id: "wf-1", issue_id: "i1", status: "running" });
    const run = await api().decideExtWorkflowStep("r1", "s1", { action: "approve", expected_status: "awaiting_human" });
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflow-runs/r1/steps/s1/decision");
    expect(init.method).toBe("POST");
    expect(JSON.parse(init.body as string)).toEqual({ action: "approve", expected_status: "awaiting_human" });
    expect(run.status).toBe("running");
  });

  it("surfaces a 409 status mismatch as an ApiError", async () => {
    respond({ error: "status mismatch" }, 409);
    const err = await api()
      .decideExtWorkflowStep("r1", "s1", { action: "approve", expected_status: "awaiting_human" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
  });

  it("cancels a run with POST", async () => {
    const fetch = respond(null, 204);
    await expect(api().cancelExtWorkflowRun("r1")).resolves.toBeUndefined();
    const { url, init } = lastCall(fetch);
    expect(url.pathname).toBe("/api/ext/workflow-runs/r1/cancel");
    expect(init.method).toBe("POST");
  });
});
