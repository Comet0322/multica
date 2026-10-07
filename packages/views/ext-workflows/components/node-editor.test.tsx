// @vitest-environment jsdom
import { useState, type ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import type { NodeRow } from "../node-editor-helpers";
import { NodeEditor, type NodeEditorProps } from "./node-editor";

// Render the popup inline so dependency options are queryable without a portal.
vi.mock("@multica/ui/components/ui/popover", () => ({
  Popover: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  PopoverTrigger: ({ children, ...props }: { children: ReactNode }) => (
    <button type="button" {...props}>
      {children}
    </button>
  ),
  PopoverContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
}));
vi.mock("./agent-select", () => ({
  AgentSelect: ({
    ariaLabel,
    value,
    onChange,
  }: {
    ariaLabel: string;
    value: string;
    onChange: (id: string) => void;
  }) => (
    <button type="button" aria-label={ariaLabel} onClick={() => onChange("ag-2")}>
      {value || "no agent"}
    </button>
  ),
}));

const AGENTS = [
  { id: "ag-1", name: "Planner", archived_at: null },
  { id: "ag-2", name: "Builder", archived_at: null },
  { id: "ag-old", name: "Retired", archived_at: "2026-01-01T00:00:00Z" },
] as unknown as Agent[];

function mk(id: string, key: string, title: string, deps: string[] = [], over: Partial<NodeRow> = {}): NodeRow {
  return {
    rowId: id,
    key,
    keyTouched: true,
    title,
    agent_id: "ag-1",
    prompt: "",
    requires_review: false,
    max_attempts: 3,
    depends_on: deps,
    ...over,
  };
}

const CHAIN = [mk("a", "plan", "Plan"), mk("b", "build", "Build", ["a"]), mk("c", "ship", "Ship", ["b"])];

function Harness({
  initial,
  onRows,
  ...rest
}: { initial: NodeRow[]; onRows?: (rows: NodeRow[]) => void } & Partial<NodeEditorProps>) {
  const [rows, setRows] = useState(initial);
  return (
    <NodeEditor
      rows={rows}
      onChange={(next) => {
        setRows(next);
        onRows?.(next);
      }}
      agents={AGENTS}
      validation={null}
      serverErrors={null}
      activeRunCount={0}
      {...rest}
    />
  );
}

const node = (n: number) => screen.getByRole("listitem", { name: `Node ${n}` });

describe("NodeEditor dependency options", () => {
  it("offers nothing that would create a cycle", () => {
    renderWithI18n(<Harness initial={CHAIN} />);
    // Plan is upstream of both others, so every other node is downstream of it.
    expect(
      within(node(1)).getByText("No other node can be added without creating a cycle."),
    ).toBeInTheDocument();
    // Build may depend on Plan, never on Ship (which depends on Build).
    expect(within(node(2)).getAllByText("Plan").length).toBeGreaterThan(0);
    expect(within(node(2)).queryByText("Ship")).not.toBeInTheDocument();
    // Ship may depend on Plan or Build.
    expect(within(node(3)).getAllByText("Plan").length).toBeGreaterThan(0);
    expect(within(node(3)).getAllByText("Build").length).toBeGreaterThan(0);
  });

  it("toggles a dependency on and off", async () => {
    const onRows = vi.fn();
    renderWithI18n(<Harness initial={CHAIN} onRows={onRows} />);
    await userEvent.click(within(node(3)).getByRole("button", { name: "Plan" }));
    expect(onRows).toHaveBeenLastCalledWith(
      expect.arrayContaining([expect.objectContaining({ rowId: "c", depends_on: ["b", "a"] })]),
    );
    await userEvent.click(within(node(3)).getByRole("button", { name: "Plan" }));
    expect(onRows).toHaveBeenLastCalledWith(
      expect.arrayContaining([expect.objectContaining({ rowId: "c", depends_on: ["b"] })]),
    );
  });
});

describe("NodeEditor row editing", () => {
  it("derives the key from the title until the key is edited", async () => {
    renderWithI18n(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add node" }));
    await userEvent.type(within(node(1)).getByLabelText("Title"), "Write docs");
    expect(within(node(1)).getByLabelText("Key")).toHaveValue("write_docs");
    await userEvent.clear(within(node(1)).getByLabelText("Key"));
    await userEvent.type(within(node(1)).getByLabelText("Key"), "docs");
    await userEvent.type(within(node(1)).getByLabelText("Title"), " v2");
    expect(within(node(1)).getByLabelText("Key")).toHaveValue("docs");
  });

  it("reorders and deletes rows, dropping dangling dependencies", async () => {
    const onRows = vi.fn();
    renderWithI18n(<Harness initial={CHAIN} onRows={onRows} />);
    await userEvent.click(within(node(1)).getByRole("button", { name: "Move down" }));
    expect(onRows.mock.lastCall![0].map((r: NodeRow) => r.rowId)).toEqual(["b", "a", "c"]);
    expect(within(node(1)).getByLabelText("Title")).toHaveValue("Build");
    await userEvent.click(within(node(2)).getByRole("button", { name: "Delete node" }));
    const rows = onRows.mock.lastCall![0] as NodeRow[];
    expect(rows.map((r) => r.rowId)).toEqual(["b", "c"]);
    expect(rows[0]!.depends_on).toEqual([]);
  });

  it("states the empty case once: no graph hint next to the nodes hint", () => {
    renderWithI18n(<Harness initial={[]} />);
    expect(screen.getByText("No nodes yet. Add the first step.")).toBeInTheDocument();
    expect(screen.queryByText("Add nodes to see the flow.")).not.toBeInTheDocument();
  });

  it("shows the prompt only when expanded", async () => {
    renderWithI18n(<Harness initial={CHAIN} />);
    expect(within(node(1)).queryByLabelText("Prompt")).not.toBeInTheDocument();
    await userEvent.click(within(node(1)).getByRole("button", { name: "Show prompt" }));
    expect(within(node(1)).getByLabelText("Prompt")).toBeInTheDocument();
    await userEvent.click(within(node(1)).getByRole("button", { name: "Hide prompt" }));
    expect(within(node(1)).queryByLabelText("Prompt")).not.toBeInTheDocument();
  });

  it("warns when a row's agent is archived", () => {
    renderWithI18n(<Harness initial={[mk("a", "plan", "Plan", [], { agent_id: "ag-old" })]} />);
    expect(
      within(node(1)).getByText("This agent is archived. Choose another agent."),
    ).toBeInTheDocument();
  });
});

describe("NodeEditor error display", () => {
  it("shows client validation on the row that has the problem", () => {
    renderWithI18n(
      <Harness
        initial={CHAIN}
        validation={{ rows: { b: { title: "title_required" } }, general: ["no_nodes"] }}
      />,
    );
    expect(within(node(2)).getByText("Title is required")).toBeInTheDocument();
    expect(within(node(1)).queryByText("Title is required")).not.toBeInTheDocument();
    expect(screen.getByText("Add at least one node")).toBeInTheDocument();
  });

  it("maps server 422 errors onto rows and lists keyless ones above the rows", () => {
    renderWithI18n(
      <Harness
        initial={CHAIN}
        serverErrors={{
          byRow: { c: { agent_id: "agent is archived", depends_on: "cycle: ship -> build -> ship" } },
          general: ["supervisor is archived"],
        }}
      />,
    );
    expect(within(node(3)).getByText("agent is archived")).toBeInTheDocument();
    expect(within(node(3)).getByText("cycle: ship -> build -> ship")).toBeInTheDocument();
    expect(within(node(1)).queryByText("agent is archived")).not.toBeInTheDocument();
    expect(screen.getByText("supervisor is archived")).toBeInTheDocument();
  });

  it("notes that runs in progress keep the old definition", () => {
    renderWithI18n(<Harness initial={CHAIN} activeRunCount={2} />);
    expect(screen.getByText("2 runs in progress keep using the previous definition.")).toBeInTheDocument();
  });

  it("is read-only for viewers who cannot manage the workflow", () => {
    renderWithI18n(<Harness initial={CHAIN} readOnly />);
    expect(screen.queryByRole("button", { name: "Add node" })).not.toBeInTheDocument();
    expect(within(node(1)).getByLabelText("Title")).toBeDisabled();
    expect(within(node(1)).queryByRole("button", { name: "Delete node" })).not.toBeInTheDocument();
  });
});
