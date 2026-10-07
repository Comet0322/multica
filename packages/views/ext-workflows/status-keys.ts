// Pure status helpers. Unknown server enums downgrade to "unknown" (AGENTS.md:
// API Compatibility), so a newer backend never crashes an older client.

export const STEP_STATUS_KEYS = [
  "pending",
  "running",
  "awaiting_supervisor",
  "awaiting_human",
  "done",
  "skipped",
  "failed",
  "cancelled",
] as const;
export type StepStatusKey = (typeof STEP_STATUS_KEYS)[number] | "unknown";

export const RUN_STATUS_KEYS = ["running", "waiting_human", "done", "failed", "cancelled"] as const;
export type RunStatusKey = (typeof RUN_STATUS_KEYS)[number] | "unknown";

export function stepStatusKey(status: string | null | undefined): StepStatusKey {
  return (STEP_STATUS_KEYS as readonly string[]).includes(status ?? "")
    ? (status as StepStatusKey)
    : "unknown";
}

export function runStatusKey(status: string | null | undefined): RunStatusKey {
  return (RUN_STATUS_KEYS as readonly string[]).includes(status ?? "")
    ? (status as RunStatusKey)
    : "unknown";
}

/** A run is "active" while it can still change: running or waiting for a human. */
export function isActiveRunStatus(status: string | null | undefined): boolean {
  const key = runStatusKey(status);
  return key === "running" || key === "waiting_human";
}
