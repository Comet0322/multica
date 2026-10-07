// @vitest-environment jsdom
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowRunsTab } from "./workflow-runs-tab";

const state = vi.hoisted(() => ({
  runs: [] as unknown[],
  total: 0,
  isError: false,
  hasData: true,
  refetch: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({
    data: state.hasData ? { runs: state.runs, total: state.total } : undefined,
    isLoading: false,
    isError: state.isError,
    error: state.isError ? new Error("boom") : null,
    refetch: state.refetch,
  }),
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowRunsOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "runs", "wf-1"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("../../navigation", () => ({
  useRowLink: () => (path: string) => ({ "data-href": path }),
}));

describe("WorkflowRunsTab", () => {
  beforeEach(() => {
    state.isError = false;
    state.hasData = true;
    state.refetch.mockReset();
  });

  it("shows an error with retry instead of the empty state when loading fails", async () => {
    state.isError = true;
    state.hasData = false;
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    expect(screen.getByRole("alert")).toHaveTextContent("Couldn't load runs");
    expect(screen.queryByText(/No runs yet/)).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(state.refetch).toHaveBeenCalledTimes(1);
  });

  it("lists runs with issue, status and times; rows open the issue", () => {
    state.total = 1;
    state.runs = [
      {
        id: "run-1",
        issue_id: "iss-1",
        issue_identifier: "MUL-12",
        issue_title: "Ship the thing",
        status: "waiting_human",
        started_at: "2026-10-01T00:00:00Z",
        finished_at: null,
      },
    ];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    const row = screen.getByText("Ship the thing").closest("[data-href]") as HTMLElement;
    expect(row).toHaveAttribute("data-href", "/acme/issues/iss-1");
    expect(within(row).getByText("MUL-12")).toBeInTheDocument();
    expect(within(row).getByText("Needs you")).toBeInTheDocument();
  });

  it("explains how to start a run when there are none", () => {
    state.total = 0;
    state.runs = [];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    expect(
      screen.getByText("No runs yet. Assign an issue to this workflow to start one."),
    ).toBeInTheDocument();
  });

  it("falls back to the unknown status for a future server enum", () => {
    state.total = 1;
    state.runs = [
      { id: "r", issue_id: "i", issue_identifier: "MUL-1", issue_title: "T", status: "paused", started_at: "2026-10-01T00:00:00Z", finished_at: null },
    ];
    renderWithI18n(<WorkflowRunsTab workflowId="wf-1" />);
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });
});
