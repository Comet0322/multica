// @vitest-environment jsdom
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowDetailPage } from "./workflow-detail-page";

const mocks = vi.hoisted(() => ({
  update: vi.fn(),
  archive: vi.fn(),
  push: vi.fn(),
  toastError: vi.fn(),
  toastSuccess: vi.fn(),
  refetch: vi.fn(),
  detail: { data: undefined as unknown, isError: false, error: null as unknown },
}));

const WORKFLOW = {
  id: "wf-1",
  name: "Release pipeline",
  description: "",
  supervisor_agent_id: "ag-1",
  max_rewinds: 3,
  creator_id: "user-me",
  avatar_url: null,
  node_count: 2,
  active_run_count: 0,
  last_run_at: null,
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-02T00:00:00Z",
  nodes: [
    { id: "n1", key: "plan", title: "Plan", agent_id: "ag-1", prompt: "", requires_review: false, max_attempts: 3, depends_on: [], position: 0 },
    { id: "n2", key: "build", title: "Build", agent_id: "ag-1", prompt: "", requires_review: true, max_attempts: 2, depends_on: ["plan"], position: 1 },
  ],
};

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "ext-workflows") return { ...mocks.detail, refetch: mocks.refetch };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Planner", archived_at: null, runtime_id: "rt" }] };
    if (queryKey[0] === "members") return { data: [{ user_id: "user-me", name: "Me", role: "member" }] };
    return { data: undefined };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowDetailOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "detail", "wf-1"] }),
  extWorkflowRunsOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "runs", "wf-1"] }),
  useUpdateExtWorkflow: () => ({ mutateAsync: mocks.update, isPending: false }),
  useArchiveExtWorkflow: () => ({ mutateAsync: mocks.archive, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-me" } }),
}));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ workflows: () => "/acme/workflows", issueDetail: (id: string) => `/acme/issues/${id}` }),
}));
vi.mock("sonner", () => ({ toast: { error: mocks.toastError, success: mocks.toastSuccess } }));
vi.mock("../../navigation", () => ({
  useNavigation: () => ({ pathname: "/acme/workflows/wf-1", push: mocks.push }),
  useRowLink: () => () => ({}),
}));
vi.mock("../../layout/breadcrumb-header", () => ({
  BreadcrumbHeader: ({ leaf, actions }: { leaf: ReactNode; actions?: ReactNode }) => (
    <header>
      {leaf}
      {actions}
    </header>
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("./agent-select", () => ({
  AgentSelect: ({ ariaLabel, value, disabled }: { ariaLabel: string; value: string; disabled?: boolean }) => (
    <button type="button" aria-label={ariaLabel} disabled={disabled}>
      {value}
    </button>
  ),
}));
vi.mock("./workflow-runs-tab", () => ({ WorkflowRunsTab: () => <div>runs tab</div> }));

beforeEach(() => {
  mocks.update.mockReset();
  mocks.archive.mockReset();
  mocks.push.mockReset();
  mocks.toastError.mockReset();
  mocks.toastSuccess.mockReset();
  mocks.refetch.mockReset();
  mocks.detail = { data: WORKFLOW, isError: false, error: null };
});

const node = (n: number) => screen.getByRole("listitem", { name: `Node ${n}` });

describe("WorkflowDetailPage", () => {
  it("starts clean: Save is disabled until something changes", () => {
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
  });

  it("saves only the changed node set and fields", async () => {
    mocks.update.mockResolvedValue({ ...WORKFLOW, name: "Release v2" });
    renderWithI18n(<WorkflowDetailPage />);
    const name = screen.getByLabelText("Name");
    await userEvent.clear(name);
    await userEvent.type(name, "Release v2");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(mocks.update).toHaveBeenCalled());
    const payload = mocks.update.mock.calls[0]![0];
    expect(payload).toMatchObject({ id: "wf-1", name: "Release v2", supervisor_agent_id: "ag-1", max_rewinds: 3 });
    // Nodes were not touched, so they are not resent.
    expect(payload).not.toHaveProperty("nodes");
  });

  it("maps a 422 onto the offending row and keeps the draft", async () => {
    mocks.update.mockRejectedValue(
      Object.assign(new Error("validation_failed"), {
        errors: [{ node_key: "build", field: "agent_id", message: "agent is archived" }],
      }),
    );
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.type(within(node(1)).getByLabelText("Title"), " step");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await within(node(2)).findByText("agent is archived")).toBeInTheDocument();
    expect(mocks.toastError).toHaveBeenCalledWith("Fix the highlighted problems and save again.");
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Plan step");
    expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  });

  it("blocks saving a node set that fails local validation", async () => {
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.clear(within(node(2)).getByLabelText("Title"));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await within(node(2)).findByText("Title is required")).toBeInTheDocument();
    expect(mocks.update).not.toHaveBeenCalled();
  });

  it("guards tab switches while dirty, and warns before the page unloads", async () => {
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.type(within(node(1)).getByLabelText("Title"), "!");

    const unload = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(unload);
    expect(unload.defaultPrevented).toBe(true);

    await userEvent.click(screen.getByRole("button", { name: "Runs" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Discard unsaved changes?")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByText("runs tab")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Runs" }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Discard" }));
    expect(await screen.findByText("runs tab")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Nodes" }));
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Plan");
  });

  it("does not warn on unload when nothing changed", () => {
    renderWithI18n(<WorkflowDetailPage />);
    const unload = new Event("beforeunload", { cancelable: true });
    fireEvent(window, unload);
    expect(unload.defaultPrevented).toBe(false);
  });

  it("archives after confirmation and returns to the list", async () => {
    mocks.archive.mockResolvedValue(undefined);
    renderWithI18n(<WorkflowDetailPage />);
    await userEvent.click(screen.getAllByRole("button", { name: "Archive" })[0]!);
    const dialog = await screen.findByRole("alertdialog");
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
    await waitFor(() => expect(mocks.archive).toHaveBeenCalledWith("wf-1"));
    expect(mocks.push).toHaveBeenCalledWith("/acme/workflows");
  });
});

describe("WorkflowDetailPage for a member who cannot manage it", () => {
  it("hides Save and Archive and makes the inspector and editor read-only", () => {
    // Role member (see the members mock) and not the creator.
    mocks.detail = { data: { ...WORKFLOW, creator_id: "user-other" }, isError: false, error: null };
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Archive" })).not.toBeInTheDocument();

    expect(screen.getByLabelText("Name")).toBeDisabled();
    expect(screen.getByLabelText("Description")).toBeDisabled();
    expect(screen.getByLabelText("Max rewinds")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Supervisor" })).toBeDisabled();

    expect(screen.queryByRole("button", { name: "Add node" })).not.toBeInTheDocument();
    expect(within(node(1)).getByLabelText("Title")).toBeDisabled();
    expect(within(node(1)).queryByRole("button", { name: "Delete node" })).not.toBeInTheDocument();
  });
});

describe("WorkflowDetailPage load states", () => {
  it("shows a skeleton, not the editor, while loading", () => {
    mocks.detail = { data: undefined, isError: false, error: null };
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("shows an error with retry when the load fails, and no editor", async () => {
    mocks.detail = { data: undefined, isError: true, error: new Error("boom") };
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.getByRole("alert")).toHaveTextContent("Couldn't load workflow");
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(mocks.refetch).toHaveBeenCalledTimes(1);
  });

  it("shows not-found for a 404", () => {
    mocks.detail = { data: undefined, isError: true, error: new ApiError("nope", 404, "Not Found") };
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.getByText("Workflow not found")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Try again" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });

  it("keeps the editor when a background refetch fails", () => {
    mocks.detail = { data: WORKFLOW, isError: true, error: new Error("boom") };
    renderWithI18n(<WorkflowDetailPage />);
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });
});
