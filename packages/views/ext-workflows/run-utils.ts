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
export function eventDetail(payload: Record<string, unknown> | null | undefined): string | null {
  for (const field of ["reason", "feedback"]) {
    const value = payload?.[field];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return null;
}

/** HTTP status of an ApiError-like value, without importing the class (it may be mocked). */
export function errorStatus(err: unknown): number | undefined {
  if (err && typeof err === "object" && "status" in err) {
    const status = (err as { status?: unknown }).status;
    return typeof status === "number" ? status : undefined;
  }
  return undefined;
}
