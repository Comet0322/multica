// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  decisionActionKey,
  errorStatus,
  eventDetail,
  eventFacts,
  eventKey,
  failureReasonKey,
  rewindTargets,
  stepAncestors,
  timelineEntries,
} from "./run-utils";

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
  it("does not repeat a step failure's reason code, showing its error instead", () => {
    expect(eventDetail({ reason: "ended_without_finishing", attempt: 1 }, "step_failed")).toBeNull();
    expect(eventDetail({ reason: "failed", error: "exit status 1" }, "step_failed")).toBe("exit status 1");
    expect(eventDetail({ reason: "no budget" }, "escalated")).toBe("no budget");
  });
});

describe("eventFacts", () => {
  const runSteps = [
    { id: "s1", title: "Plan" },
    { id: "s2", title: "Build" },
  ];
  const ev = (kind: string, step_id: string | null, payload: Record<string, unknown> = {}) => ({ kind, step_id, payload });

  it.each([
    ["run_started", null, {}, { stepTitle: null, action: null, attempt: null, failureReason: null }],
    ["step_started", "s1", { attempt: 2 }, { stepTitle: "Plan", action: null, attempt: 2, failureReason: null }],
    ["step_finished", "s2", { attempt: 1, review: true }, { stepTitle: "Build", action: null, attempt: 1, failureReason: null }],
    [
      "step_failed",
      "s2",
      { attempt: 3, reason: "ended_without_finishing" },
      { stepTitle: "Build", action: null, attempt: 3, failureReason: "ended_without_finishing" },
    ],
    ["decision", "s1", { action: "approve" }, { stepTitle: "Plan", action: "approve", attempt: null, failureReason: null }],
    ["escalated", "s2", { reason: "stuck" }, { stepTitle: "Build", action: null, attempt: null, failureReason: null }],
    ["step_started", "gone", { attempt: 1 }, { stepTitle: null, action: null, attempt: 1, failureReason: null }],
  ] as const)("%s on %s", (kind, stepId, payload, want) => {
    expect(eventFacts(ev(kind, stepId, payload), runSteps)).toEqual(want);
  });

  it("ignores malformed payload values", () => {
    expect(eventFacts(ev("step_failed", "s1", { attempt: "2", reason: 7 }), runSteps)).toEqual({
      stepTitle: "Plan",
      action: null,
      attempt: null,
      failureReason: null,
    });
    expect(eventFacts(ev("decision", "s1", { action: "" }), runSteps).action).toBeNull();
    expect(eventFacts({ kind: "decision", step_id: "s1", payload: null }, runSteps).action).toBeNull();
  });
});

describe("label keys", () => {
  it("maps known decision actions and leaves unknown ones to the caller", () => {
    expect(decisionActionKey("approve")).toBe("approve");
    expect(decisionActionKey("escalate")).toBe("escalate");
    expect(decisionActionKey("teleport")).toBeNull();
  });
  it("maps known step failure reasons and leaves unknown ones to the caller", () => {
    for (const reason of ["ended_without_finishing", "cancelled", "task_missing", "dispatch_failed", "failed"]) {
      expect(failureReasonKey(reason)).toBe(reason);
    }
    expect(failureReasonKey("agent_error")).toBeNull();
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

  it("does not show a step failure's reason code as its detail", () => {
    const entries = timelineEntries([ev("f", "step_failed", "s1", { reason: "task_missing", attempt: 1 })]);
    expect(entries.map((e) => e.detail)).toEqual([null]);
  });
});
