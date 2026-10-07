// @vitest-environment node
import { describe, expect, it } from "vitest";
import { errorStatus, eventDetail, eventKey, rewindTargets, stepAncestors, timelineEntries } from "./run-utils";

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

describe("timelineEntries", () => {
  const ev = (id: string, kind: string, step_id: string | null, payload: Record<string, unknown> = {}) => ({ id, kind, step_id, payload });

  it("lists a rewind once, carrying the decision's feedback", () => {
    const entries = timelineEntries([
      ev("d", "decision", "s2", { action: "rewind", to: "base", feedback: "produce base v2" }),
      ev("r", "rewind", "s2", { from: "use", to: "base", reset: ["base", "use"] }),
    ]);
    expect(entries.map((e) => [e.event.id, e.detail])).toEqual([["r", "produce base v2"]]);
  });

  it("lists an escalation once", () => {
    const entries = timelineEntries([
      ev("d", "decision", "s2", { action: "escalate", reason: "needs a product call" }),
      ev("x", "escalated", "s2", { reason: "needs a product call" }),
    ]);
    expect(entries.map((e) => [e.event.id, e.detail])).toEqual([["x", "needs a product call"]]);
  });

  it("keeps decisions that are not followed by their own event", () => {
    const entries = timelineEntries([
      ev("d1", "decision", "s1", { action: "retry" }),
      ev("d2", "decision", "s2", { action: "rewind", to: "s1", feedback: "x" }),
      ev("r", "rewind", "s3", {}),
      ev("x", "escalated", "s2", { reason: "auto" }),
    ]);
    expect(entries.map((e) => e.event.id)).toEqual(["d1", "d2", "r", "x"]);
  });
});
