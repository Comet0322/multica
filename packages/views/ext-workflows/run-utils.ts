// Pure helpers for the issue run section (no React, no core imports).

interface StepLike {
  node_key: string;
  title: string;
  depends_on: string[];
}

/** Transitive upstream node keys of `nodeKey`, in step order, excluding itself. */
export function stepAncestors(steps: readonly StepLike[], nodeKey: string): string[] {
  const byKey = new Map(steps.map((s) => [s.node_key, s]));
  const seen = new Set<string>();
  const stack = [...(byKey.get(nodeKey)?.depends_on ?? [])];
  while (stack.length > 0) {
    const key = stack.pop()!;
    if (seen.has(key) || key === nodeKey) continue;
    seen.add(key);
    stack.push(...(byKey.get(key)?.depends_on ?? []));
  }
  return steps.map((s) => s.node_key).filter((k) => seen.has(k));
}

export interface RewindTarget {
  key: string;
  title: string;
  isSelf: boolean;
}

/** A rewind may return to any upstream step, or to the step itself. */
export function rewindTargets(steps: readonly StepLike[], nodeKey: string): RewindTarget[] {
  const allowed = new Set([...stepAncestors(steps, nodeKey), nodeKey]);
  return steps
    .filter((s) => allowed.has(s.node_key))
    .map((s) => ({ key: s.node_key, title: s.title, isSelf: s.node_key === nodeKey }));
}

const EVENT_KINDS = [
  "run_started",
  "step_started",
  "step_finished",
  "step_failed",
  "decision",
  "rewind",
  "rewind_requested",
  "escalated",
  "run_finished",
  "run_cancelled",
  "protocol_error",
] as const;
export type EventKey = (typeof EVENT_KINDS)[number] | "unknown";

export function eventKey(kind: string): EventKey {
  return (EVENT_KINDS as readonly string[]).includes(kind) ? (kind as EventKey) : "unknown";
}

/** The most informative free text in an event payload, if any. */
export function eventDetail(payload: Record<string, unknown> | null | undefined, kind?: string): string | null {
  // A step failure's `reason` is a code shown in the row's summary; its free
  // text is the task error.
  const fields = kind === "step_failed" ? ["error"] : ["reason", "feedback"];
  for (const field of fields) {
    const value = payload?.[field];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return null;
}

const DECISION_ACTIONS = ["approve", "redo", "retry", "skip", "rewind", "abort", "escalate"] as const;
export type DecisionActionKey = (typeof DECISION_ACTIONS)[number];

/** The label key for a decision action, or null for one this client does not know. */
export function decisionActionKey(action: string): DecisionActionKey | null {
  return (DECISION_ACTIONS as readonly string[]).includes(action) ? (action as DecisionActionKey) : null;
}

const FAILURE_REASONS = ["ended_without_finishing", "cancelled", "task_missing", "dispatch_failed", "failed"] as const;
export type FailureReasonKey = (typeof FAILURE_REASONS)[number];

/** The label key for a step failure reason, or null for one this client does not know. */
export function failureReasonKey(reason: string): FailureReasonKey | null {
  return (FAILURE_REASONS as readonly string[]).includes(reason) ? (reason as FailureReasonKey) : null;
}

export interface EventFacts {
  /** Title of the event's step; null for run-level events or an unknown step. */
  stepTitle: string | null;
  /** Raw decision action, for `decision` events. */
  action: string | null;
  /** Step attempt, for step_started / step_finished / step_failed. */
  attempt: number | null;
  /** Raw failure reason code, for `step_failed`. */
  failureReason: string | null;
}

const ATTEMPT_KINDS = new Set(["step_started", "step_finished", "step_failed"]);

function nonEmptyString(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

/** What a timeline row says about an event beyond its kind. */
export function eventFacts(
  event: EventLike,
  steps: readonly { id: string; title: string }[],
): EventFacts {
  const payload = event.payload ?? {};
  const attempt = payload.attempt;
  return {
    stepTitle: event.step_id ? (steps.find((s) => s.id === event.step_id)?.title ?? null) : null,
    action: event.kind === "decision" ? nonEmptyString(payload.action) : null,
    attempt: ATTEMPT_KINDS.has(event.kind) && typeof attempt === "number" ? attempt : null,
    failureReason: event.kind === "step_failed" ? nonEmptyString(payload.reason) : null,
  };
}

interface EventLike {
  kind: string;
  step_id: string | null;
  payload: Record<string, unknown> | null | undefined;
}

/** The event a decision records right after itself, when the decision has one. */
const DECISION_FOLLOW_UP: Record<string, string> = { rewind: "rewind", escalate: "escalated" };

/**
 * Timeline rows, each act listed once: a rewind or escalate decision is folded
 * into the rewind/escalated event it produced, which keeps the decision's text.
 */
export function timelineEntries<E extends EventLike>(events: readonly E[]): { event: E; detail: string | null }[] {
  const out: { event: E; detail: string | null }[] = [];
  let carried: string | null = null;
  events.forEach((event, i) => {
    const next = events[i + 1];
    const action = event.payload?.action;
    if (
      event.kind === "decision" &&
      typeof action === "string" &&
      next &&
      next.step_id === event.step_id &&
      next.kind === DECISION_FOLLOW_UP[action]
    ) {
      carried = eventDetail(event.payload);
      return;
    }
    out.push({ event, detail: eventDetail(event.payload, event.kind) ?? carried });
    carried = null;
  });
  return out;
}

/** HTTP status of an ApiError-like value, without importing the class (it may be mocked). */
export function errorStatus(err: unknown): number | undefined {
  if (err && typeof err === "object" && "status" in err) {
    const status = (err as { status?: unknown }).status;
    return typeof status === "number" ? status : undefined;
  }
  return undefined;
}
