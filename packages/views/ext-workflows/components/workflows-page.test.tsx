// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { renderWithI18n } from "../../test/i18n";
import { WorkflowsPage } from "./workflows-page";

const WORKFLOWS = [
  {
    id: "wf-1",
    name: "Release pipeline",
    description: "Ship it",
    supervisor_agent_id: "ag-1",
    creator_id: "user-me",
    avatar_url: null,
    node_count: 4,
    active_run_count: 2,
    last_run_at: "2026-10-01T00:00:00Z",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  },
  {
    id: "wf-2",
    name: "Docs sweep",
    description: "",
    supervisor_agent_id: "ag-1",
    creator_id: "user-other",
    avatar_url: null,
    node_count: 1,
    active_run_count: 0,
    last_run_at: null,
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  },
];

const mocks = vi.hoisted(() => ({
  archive: vi.fn(),
  openModal: vi.fn(),
  role: "member" as "member" | "admin",
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "ext-workflows") return { data: WORKFLOWS, isLoading: false };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Supervisor Bot" }] };
    if (queryKey[0] === "members")
      return { data: [{ user_id: "user-me", name: "Me", role: mocks.role }] };
    return { data: [] };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowListOptions: () => ({ queryKey: ["ext-workflows", "ws-1", "list"] }),
  useArchiveExtWorkflow: () => ({ mutateAsync: mocks.archive, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (s: { user: { id: string } }) => unknown) => selector({ user: { id: "user-me" } }),
}));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1" }),
  useWorkspacePaths: () => ({ workflowDetail: (id: string) => `/acme/workflows/${id}` }),
}));
vi.mock("@multica/core/modals", () => ({
  useModalStore: { getState: () => ({ open: mocks.openModal }) },
}));
vi.mock("../../navigation", () => ({
  useRowLink: () => (path: string) => ({ "data-href": path }),
  useIntentNavigate: () => vi.fn(),
}));
vi.mock("../../layout/collection-page", () => ({
  CollectionPageHeader: ({ title, actions }: { title: ReactNode; actions?: ReactNode }) => (
    <header>
      <h1>{title}</h1>
      {actions}
    </header>
  ),
  CollectionPageHeaderAction: ({ label, onClick }: { label: string; onClick: () => void }) => (
    <button onClick={onClick}>{label}</button>
  ),
  CollectionPageState: ({ title }: { title: ReactNode }) => <div>{title}</div>,
}));
vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: ({ actorId }: { actorId: string }) => <span data-testid={`avatar-${actorId}`} />,
}));

beforeEach(() => {
  mocks.archive.mockReset().mockResolvedValue(undefined);
  mocks.openModal.mockReset();
  mocks.role = "member";
});

describe("WorkflowsPage", () => {
  it("lists workflows with supervisor, node count, active runs and last run", () => {
    renderWithI18n(<WorkflowsPage />);
    const row = screen.getByText("Release pipeline").closest("[data-href]") as HTMLElement;
    expect(row).toHaveAttribute("data-href", "/acme/workflows/wf-1");
    expect(within(row).getByText("Supervisor Bot")).toBeInTheDocument();
    expect(within(row).getByText("4")).toBeInTheDocument();
    expect(within(row).getByText("2")).toBeInTheDocument();
    const other = screen.getByText("Docs sweep").closest("[data-href]") as HTMLElement;
    expect(within(other).getByText("Never")).toBeInTheDocument();
  });

  it("opens the create dialog from the header button", async () => {
    renderWithI18n(<WorkflowsPage />);
    await userEvent.click(screen.getByRole("button", { name: "New workflow" }));
    expect(mocks.openModal).toHaveBeenCalledWith("create-ext-workflow");
  });

  it("offers archive only on workflows the viewer can manage, and confirms first", async () => {
    renderWithI18n(<WorkflowsPage />);
    // Member: only the workflow they created has the row menu.
    expect(screen.getAllByRole("button", { name: "Workflow actions" })).toHaveLength(1);
    await userEvent.click(screen.getByRole("button", { name: "Workflow actions" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Archive" }));
    expect(mocks.archive).not.toHaveBeenCalled();
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("Archive this workflow?")).toBeInTheDocument();
    await userEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
    expect(mocks.archive).toHaveBeenCalledWith("wf-1");
  });

  it("lets workspace admins archive any workflow", () => {
    mocks.role = "admin";
    renderWithI18n(<WorkflowsPage />);
    expect(screen.getAllByRole("button", { name: "Workflow actions" })).toHaveLength(2);
  });
});
