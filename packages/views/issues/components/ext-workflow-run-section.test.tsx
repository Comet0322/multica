// @vitest-environment jsdom
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";
import { ExtWorkflowRunSection, ExtWorkflowStepLine } from "./ext-workflow-run-section";

const mocks = vi.hoisted(() => ({
  decide: vi.fn(),
  cancel: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  issueRuns: { runs: [] as unknown[], step_of: null as unknown },
  run: undefined as unknown,
  userId: "user-1" as string | undefined,
  members: [{ user_id: "user-1", role: "member" }] as unknown[],
  workflowCreator: "someone-else",
}));

function step(over: Record<string, unknown>) {
  return {
    id: "step-x",
    node_key: "x",
    title: "X",
    agent_id: "ag-1",
    issue_id: "child-x",
    status: "pending",
    attempts: 0,
    max_attempts: 3,
    requires_review: false,
    depends_on: [],
    pending_reason: null,
    last_feedback: null,
    escalation_reason: null,
    started_at: null,
    finished_at: null,
    ...over,
  };
}

function makeRun(over: Record<string, unknown> = {}) {
  return {
    id: "run-1",
    workspace_id: "ws-1",
    workflow_id: "wf-1",
    workflow_name: "Release pipeline",
    issue_id: "issue-1",
    issue_identifier: "MUL-1",
    issue_title: "Ship it",
    triggered_by_type: "member",
    triggered_by_id: "user-1",
    status: "waiting_human",
    rewinds_used: 0,
    max_rewinds: 2,
    started_at: "2026-10-01T00:00:00Z",
    finished_at: null,
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    steps: [
      step({ id: "s1", node_key: "plan", title: "Plan", status: "done", attempts: 1, issue_id: "child-1" }),
      step({ id: "s2", node_key: "build", title: "Build", status: "awaiting_human", attempts: 2, depends_on: ["plan"], escalation_reason: "Tests keep failing", issue_id: "child-2" }),
      step({ id: "s3", node_key: "review", title: "Review", status: "awaiting_supervisor", attempts: 1, depends_on: ["build"], issue_id: "child-3" }),
      step({ id: "s4", node_key: "ship", title: "Ship", status: "pending", depends_on: ["review"], issue_id: "child-4" }),
    ],
    events: [
      { id: "e1", step_id: null, kind: "run_started", actor_type: "engine", actor_id: null, on_behalf_of: null, payload: {}, created_at: "2026-10-01T00:00:00Z" },
      { id: "e2", step_id: "s2", kind: "escalated", actor_type: "agent", actor_id: "ag-1", on_behalf_of: null, payload: { reason: "Tests keep failing" }, created_at: "2026-10-01T01:00:00Z" },
    ],
    ...over,
  };
}

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "issue-runs") return { data: mocks.issueRuns };
    if (queryKey[0] === "run") return { data: mocks.run };
    if (queryKey[0] === "members") return { data: mocks.members };
    if (queryKey[0] === "wf") return { data: { creator_id: mocks.workflowCreator } };
    if (queryKey[0] === "issue") return { data: { id: "issue-9", identifier: "MUL-9", title: "Parent" } };
    return { data: undefined };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowIssueRunsOptions: () => ({ queryKey: ["issue-runs"] }),
  extWorkflowRunOptions: () => ({ queryKey: ["run"] }),
  extWorkflowDetailOptions: () => ({ queryKey: ["wf"] }),
  useDecideExtWorkflowStep: () => ({ mutateAsync: mocks.decide, isPending: false }),
  useCancelExtWorkflowRun: () => ({ mutateAsync: mocks.cancel, isPending: false }),
}));
vi.mock("@multica/core/issues/queries", () => ({
  issueDetailOptions: () => ({ queryKey: ["issue"] }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } | null }) => unknown) =>
    sel({ user: mocks.userId ? { id: mocks.userId } : null }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    workflowDetail: (id: string) => `/acme/workflows/${id}`,
  }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: (type: string, id: string) => `${type}:${id}` }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.toastError, success: mocks.toastSuccess } }));
vi.mock("../../navigation", () => ({
  AppLink: ({ children, href, ...props }: { children: ReactNode; href: string }) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));

beforeEach(() => {
  mocks.decide.mockReset().mockResolvedValue(undefined);
  mocks.cancel.mockReset().mockResolvedValue(undefined);
  mocks.toastError.mockReset();
  mocks.toastSuccess.mockReset();
  mocks.userId = "user-1";
  mocks.members = [{ user_id: "user-1", role: "member" }];
  mocks.workflowCreator = "someone-else";
  const run = makeRun();
  mocks.run = run;
  mocks.issueRuns = { runs: [run], step_of: null };
});

describe("ExtWorkflowRunSection", () => {
  it("renders nothing when the issue has no run", () => {
    mocks.issueRuns = { runs: [], step_of: null };
    const { container } = renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the run status, rewinds and every step with agent, attempts and child link", () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByText("Workflow run")).toBeInTheDocument();
    expect(screen.getAllByText("Needs you").length).toBeGreaterThan(0);
    expect(screen.getByText("Rewinds 0/2")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Release pipeline" })).toHaveAttribute("href", "/acme/workflows/wf-1");
    for (const title of ["Plan", "Build", "Review", "Ship"]) expect(screen.getByText(title)).toBeInTheDocument();
    expect(screen.getByText("Attempt 2/3")).toBeInTheDocument();
    const links = screen.getAllByRole("link", { name: "Open step issue" });
    expect(links.map((l) => l.getAttribute("href"))).toEqual([
      "/acme/issues/child-1",
      "/acme/issues/child-2",
      "/acme/issues/child-3",
      "/acme/issues/child-4",
    ]);
    expect(screen.getByText("Tests keep failing")).toBeInTheDocument();
  });

  it("offers decisions only for the step awaiting a human", () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    for (const name of ["Approve", "Redo…", "Retry", "Skip", "Rewind to…", "Abort…"]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
    // The supervisor-review, done and pending steps get no buttons.
    expect(screen.getByText("Review").closest("li")!.querySelectorAll("button")).toHaveLength(0);
    expect(screen.getByText("Plan").closest("li")!.querySelectorAll("button")).toHaveLength(0);
  });

  it("hides decisions and cancel from a member who may not decide", () => {
    mocks.userId = "user-2";
    mocks.members = [{ user_id: "user-2", role: "member" }];
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
    expect(screen.getByText("Build")).toBeInTheDocument();
  });

  it("allows workspace admins and the workflow creator to decide", () => {
    mocks.userId = "user-2";
    mocks.members = [{ user_id: "user-2", role: "admin" }];
    const { unmount } = renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
    unmount();
    mocks.members = [{ user_id: "user-2", role: "member" }];
    mocks.workflowCreator = "user-2";
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByRole("button", { name: "Approve" })).toBeInTheDocument();
  });

  it("offers no decisions when no step awaits a human", () => {
    const run = makeRun({
      status: "running",
      steps: [step({ id: "s1", node_key: "plan", title: "Plan", status: "awaiting_supervisor" })],
    });
    mocks.run = run;
    mocks.issueRuns = { runs: [run], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Approve" })).not.toBeInTheDocument();
  });

  it("approves with the step's current status as expected_status", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(mocks.decide).toHaveBeenCalledWith({
      runId: "run-1",
      stepId: "s2",
      action: "approve",
      expected_status: "awaiting_human",
    });
    expect(mocks.toastSuccess).toHaveBeenCalledWith("Decision sent");
  });

  it("retries and skips directly", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(mocks.decide).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: "retry", stepId: "s2", expected_status: "awaiting_human" }),
    );
    await userEvent.click(screen.getByRole("button", { name: "Skip" }));
    expect(mocks.decide).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: "skip", stepId: "s2", expected_status: "awaiting_human" }),
    );
  });

  it("redo asks for feedback first", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Redo…" }));
    const dialog = await screen.findByRole("dialog");
    const confirm = within(dialog).getByRole("button", { name: "Redo" });
    expect(confirm).toBeDisabled();
    await userEvent.type(within(dialog).getByLabelText("Feedback for the agent"), "Cover the edge cases");
    await userEvent.click(confirm);
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "redo",
        feedback: "Cover the edge cases",
        expected_status: "awaiting_human",
      }),
    );
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("rewind offers the upstream steps and the step itself", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Rewind to…" }));
    const dialog = await screen.findByRole("dialog");
    const select = within(dialog).getByLabelText("Return to") as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(["plan", "build"]);
    expect(within(dialog).getByRole("option", { name: "Build (this step)" })).toBeInTheDocument();
    await userEvent.selectOptions(select, "plan");
    await userEvent.type(within(dialog).getByLabelText("Feedback for the agent"), "Redo the plan");
    await userEvent.click(within(dialog).getByRole("button", { name: "Rewind" }));
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "rewind",
        to: "plan",
        feedback: "Redo the plan",
        expected_status: "awaiting_human",
      }),
    );
  });

  it("abort requires a reason", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Abort…" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "Out of scope");
    await userEvent.click(within(dialog).getByRole("button", { name: "Abort run" }));
    await waitFor(() =>
      expect(mocks.decide).toHaveBeenCalledWith({
        runId: "run-1",
        stepId: "s2",
        action: "abort",
        reason: "Out of scope",
        expected_status: "awaiting_human",
      }),
    );
  });

  it("disables redo and retry at the attempt limit and rewind at the rewind limit", () => {
    const run = makeRun({
      rewinds_used: 2,
      steps: [
        step({ id: "s2", node_key: "build", title: "Build", status: "awaiting_human", attempts: 3, max_attempts: 3 }),
      ],
    });
    mocks.run = run;
    mocks.issueRuns = { runs: [run], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.getByRole("button", { name: "Redo…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Rewind to…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Approve" })).toBeEnabled();
    expect(screen.getByText("Attempt limit reached")).toBeInTheDocument();
    expect(screen.getByText("Rewind limit reached")).toBeInTheDocument();
  });

  it("explains a 409 status mismatch and a 403", async () => {
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }));
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() =>
      expect(mocks.toastError).toHaveBeenCalledWith("This step changed. Review its new state and try again."),
    );
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("forbidden"), { status: 403 }));
    await userEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalledWith("You can't decide on this run"));
  });

  it("keeps a dialog open when the decision fails", async () => {
    mocks.decide.mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }));
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Abort…" }));
    const dialog = await screen.findByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Reason"), "x");
    await userEvent.click(within(dialog).getByRole("button", { name: "Abort run" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalled());
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("cancels an active run after confirmation, and hides Cancel once it finished", async () => {
    const { unmount } = renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(mocks.cancel).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel run" }));
    await waitFor(() => expect(mocks.cancel).toHaveBeenCalledWith("run-1"));
    unmount();

    const done = makeRun({ status: "done", finished_at: "2026-10-02T00:00:00Z", steps: [] });
    mocks.run = done;
    mocks.issueRuns = { runs: [done], step_of: null };
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByRole("button", { name: "Cancel run" })).not.toBeInTheDocument();
  });

  it("shows the timeline only when expanded", async () => {
    renderWithI18n(<ExtWorkflowRunSection issueId="issue-1" />);
    expect(screen.queryByText("Run started")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Timeline" }));
    expect(screen.getByText("Run started")).toBeInTheDocument();
    expect(screen.getByText("Escalated to a human")).toBeInTheDocument();
    expect(screen.getAllByText("Tests keep failing").length).toBeGreaterThan(1);
  });
});

describe("ExtWorkflowStepLine", () => {
  it("renders nothing for an issue that is not a workflow step", () => {
    const { container } = renderWithI18n(<ExtWorkflowStepLine issueId="child-1" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows 'Workflow step k of n · parent' linking to the parent issue", () => {
    mocks.issueRuns = {
      runs: [],
      step_of: { run_id: "run-1", step_id: "s2", node_key: "build", index: 2, total: 4, parent_issue_id: "issue-9" },
    };
    renderWithI18n(<ExtWorkflowStepLine issueId="child-2" />);
    const link = screen.getByRole("link", { name: /Workflow step 2 of 4/ });
    expect(link).toHaveAttribute("href", "/acme/issues/issue-9");
    expect(link).toHaveTextContent("Workflow step 2 of 4 · MUL-9");
  });
});
