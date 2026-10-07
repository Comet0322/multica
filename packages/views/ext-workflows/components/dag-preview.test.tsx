// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { DagPreview } from "./dag-preview";

const rows = [
  { rowId: "a", key: "plan", title: "Plan", depends_on: [] },
  { rowId: "b", key: "build", title: "Build", depends_on: ["a"] },
  { rowId: "c", key: "ship", title: "Ship", depends_on: ["a", "b"] },
];

describe("DagPreview", () => {
  it("draws one box per node and one edge per dependency", () => {
    const { container } = renderWithI18n(<DagPreview rows={rows} />);
    expect(screen.getByRole("img", { name: "Workflow graph" })).toBeInTheDocument();
    expect(container.querySelectorAll("[data-dag-node]")).toHaveLength(3);
    expect(container.querySelectorAll("[data-dag-edge]")).toHaveLength(3);
    expect(screen.getByText("Plan")).toBeInTheDocument();
  });

  it("shows a hint instead of an empty graph", () => {
    renderWithI18n(<DagPreview rows={[]} />);
    expect(screen.getByText("Add nodes to see the flow.")).toBeInTheDocument();
  });
});
