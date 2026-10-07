// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../test/i18n";
import { CreateExtWorkflowModal } from "./create-ext-workflow";

const mocks = vi.hoisted(() => ({
  create: vi.fn(),
  push: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: [{ id: "ag-1", name: "Supervisor Bot" }] }),
}));
vi.mock("@multica/core/ext-workflows", () => ({
  useCreateExtWorkflow: () => ({ mutateAsync: mocks.create, isPending: false }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ workflowDetail: (id: string) => `/acme/workflows/${id}` }),
}));
vi.mock("@multica/core/utils", () => ({ isImeComposing: () => false }));
vi.mock("sonner", () => ({
  toast: { success: mocks.toastSuccess, error: mocks.toastError },
}));
vi.mock("../navigation", () => ({ useNavigation: () => ({ push: mocks.push }) }));
// The picker is covered by its own usage in the node editor; here it is a stub.
vi.mock("../ext-workflows/components/agent-select", () => ({
  AgentSelect: ({ ariaLabel, onChange }: { ariaLabel: string; onChange: (id: string) => void }) => (
    <button type="button" aria-label={ariaLabel} onClick={() => onChange("ag-1")}>
      pick supervisor
    </button>
  ),
}));

beforeEach(() => {
  mocks.create.mockReset().mockResolvedValue({ id: "wf-9" });
  mocks.push.mockReset();
  mocks.toastSuccess.mockReset();
  mocks.toastError.mockReset();
});

describe("CreateExtWorkflowModal", () => {
  it("keeps Create disabled until a name and a supervisor are set", async () => {
    renderWithI18n(<CreateExtWorkflowModal onClose={vi.fn()} />);
    const submit = screen.getByRole("button", { name: "Create" });
    expect(submit).toBeDisabled();
    await userEvent.type(screen.getByLabelText("Name"), "Release pipeline");
    expect(submit).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    expect(submit).toBeEnabled();
  });

  it("creates the workflow and opens its detail page", async () => {
    const onClose = vi.fn();
    renderWithI18n(<CreateExtWorkflowModal onClose={onClose} />);
    await userEvent.type(screen.getByLabelText("Name"), "  Release pipeline ");
    await userEvent.type(screen.getByLabelText("Description"), "Ship it");
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(mocks.create).toHaveBeenCalledWith({
        name: "Release pipeline",
        description: "Ship it",
        supervisor_agent_id: "ag-1",
      }),
    );
    expect(onClose).toHaveBeenCalled();
    expect(mocks.push).toHaveBeenCalledWith("/acme/workflows/wf-9");
    expect(mocks.toastSuccess).toHaveBeenCalledWith("Workflow created");
  });

  it("stays open and shows the error when creation fails", async () => {
    mocks.create.mockRejectedValue(new Error("supervisor has no runtime"));
    const onClose = vi.fn();
    renderWithI18n(<CreateExtWorkflowModal onClose={onClose} />);
    await userEvent.type(screen.getByLabelText("Name"), "X");
    await userEvent.click(screen.getByRole("button", { name: "Supervisor agent" }));
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(mocks.toastError).toHaveBeenCalledWith("supervisor has no runtime"));
    expect(onClose).not.toHaveBeenCalled();
    expect(mocks.push).not.toHaveBeenCalled();
  });
});
