// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enIssues from "../../../locales/en/issues.json";
import { AssigneePicker } from "./assignee-picker";

const WORKFLOWS = [
  { id: "wf-1", name: "Release pipeline", node_count: 3, archived_at: null },
  { id: "wf-2", name: "Empty flow", node_count: 0, archived_at: null },
  { id: "wf-3", name: "Retired flow", node_count: 2, archived_at: "2026-01-01T00:00:00Z" },
];

vi.mock("@tanstack/react-query", () => ({
  useQuery: ({ queryKey }: { queryKey: string[] }) => {
    if (queryKey[0] === "members") return { data: [{ user_id: "user-1", name: "Ada Lovelace", role: "member" }] };
    if (queryKey[0] === "agents") return { data: [{ id: "ag-1", name: "Planner", archived_at: null }] };
    if (queryKey[0] === "squads") return { data: [{ id: "sq-1", name: "Core squad", leader_id: "ag-1", archived_at: null }] };
    if (queryKey[0] === "ext-workflows") return { data: WORKFLOWS };
    return { data: [] };
  },
}));
vi.mock("@multica/core/ext-workflows", () => ({
  extWorkflowListOptions: () => ({ queryKey: ["ext-workflows", "workspace-1", "list"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/auth", () => ({ useAuthStore: () => ({ id: "user-1" }) }));
vi.mock("@multica/core/agents", () => ({ isAgentRuntimeBound: () => true }));
vi.mock("@multica/core/permissions", () => ({ canAssignAgentToIssue: () => ({ allowed: true }) }));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: () => "Someone" }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
  agentListOptions: () => ({ queryKey: ["agents"] }),
  squadListOptions: () => ({ queryKey: ["squads"] }),
  assigneeFrequencyOptions: () => ({ queryKey: ["frequency"] }),
}));
vi.mock("../../../common/actor-avatar", () => ({
  ActorAvatar: () => <span data-testid="actor-avatar" />,
}));

function renderPicker(onUpdate: () => void, props: Record<string, unknown> = {}) {
  return render(
    <I18nProvider locale="en" resources={{ en: { issues: enIssues } }}>
      <AssigneePicker assigneeType={null} assigneeId={null} onUpdate={onUpdate} open onOpenChange={() => {}} {...props} />
    </I18nProvider>,
  );
}

describe("AssigneePicker workflows group", () => {
  it("lists non-archived workflows in a group after Squads", async () => {
    renderPicker(vi.fn());
    const squads = await screen.findByText("Squads");
    const workflows = screen.getByText("Workflows");
    expect(squads.compareDocumentPosition(workflows) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "Release pipeline" })).toBeInTheDocument();
    expect(screen.queryByText("Retired flow")).not.toBeInTheDocument();
  });

  it("assigns the issue to a workflow", async () => {
    const onUpdate = vi.fn();
    renderPicker(onUpdate);
    await userEvent.click(await screen.findByRole("button", { name: "Release pipeline" }));
    expect(onUpdate).toHaveBeenCalledWith({ assignee_type: "workflow", assignee_id: "wf-1" });
  });

  it("disables a workflow that has no nodes yet", async () => {
    const onUpdate = vi.fn();
    renderPicker(onUpdate);
    const row = await screen.findByRole("button", { name: "Empty flow" });
    expect(row).toBeDisabled();
    await userEvent.click(row);
    expect(onUpdate).not.toHaveBeenCalled();
  });

  it("filters workflows by the search box", async () => {
    renderPicker(vi.fn());
    await userEvent.type(await screen.findByPlaceholderText("Assign to..."), "release");
    expect(screen.getByRole("button", { name: "Release pipeline" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Empty flow" })).not.toBeInTheDocument();
  });
});
