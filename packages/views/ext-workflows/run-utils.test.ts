// @vitest-environment node
import { describe, expect, it } from "vitest";
import { errorStatus, eventDetail, eventKey, rewindTargets, stepAncestors } from "./run-utils";

const steps = [
  { node_key: "plan", title: "Plan", depends_on: [] as string[] },
  { node_key: "build", title: "Build", depends_on: ["plan"] },
  { node_key: "docs", title: "Docs", depends_on: ["plan"] },
  { node_key: "ship", title: "Ship", depends_on: ["build", "docs"] },
];

describe("stepAncestors", () => {
  it("returns every transitive upstream key in step order, excluding the step itself", () => {
    expect(stepAncestors(steps, "ship")).toEqual(["plan", "build", "docs"]);
    expect(stepAncestors(steps, "build")).toEqual(["plan"]);
    expect(stepAncestors(steps, "plan")).toEqual([]);
  });
  it("terminates on malformed cyclic input", () => {
    const cyclic = [
      { node_key: "a", title: "A", depends_on: ["b"] },
      { node_key: "b", title: "B", depends_on: ["a"] },
    ];
    expect(() => stepAncestors(cyclic, "a")).not.toThrow();
  });
});

describe("rewindTargets", () => {
  it("offers the upstream steps and the step itself, in step order", () => {
    expect(rewindTargets(steps, "build").map((t) => t.key)).toEqual(["plan", "build"]);
    expect(rewindTargets(steps, "plan").map((t) => t.key)).toEqual(["plan"]);
  });
  it("marks which target is the step itself", () => {
    expect(rewindTargets(steps, "ship").map((t) => t.isSelf)).toEqual([false, false, false, true]);
  });
});

describe("event helpers", () => {
  it("passes known kinds through and downgrades unknown ones", () => {
    expect(eventKey("escalated")).toBe("escalated");
    expect(eventKey("brand_new")).toBe("unknown");
  });
  it("shows the most useful payload text", () => {
    expect(eventDetail({ reason: "no budget", feedback: "x" })).toBe("no budget");
    expect(eventDetail({ feedback: "tighten it" })).toBe("tighten it");
    expect(eventDetail({ action: "approve" })).toBeNull();
    expect(eventDetail(null)).toBeNull();
  });
});

describe("errorStatus", () => {
  it("reads an HTTP status off an ApiError-like value", () => {
    expect(errorStatus({ status: 409 })).toBe(409);
    expect(errorStatus(new Error("x"))).toBeUndefined();
    expect(errorStatus(null)).toBeUndefined();
  });
});
