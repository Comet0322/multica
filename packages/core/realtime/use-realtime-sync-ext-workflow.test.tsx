/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { WSClient } from "../api/ws-client";
import { extWorkflowKeys } from "../ext-workflows/queries";
import { issueKeys } from "../issues/queries";
import { useRealtimeSync, type RealtimeSyncStores } from "./use-realtime-sync";

vi.mock("../platform/workspace-storage", () => ({
  getCurrentWsId: () => "ws-1",
  getCurrentSlug: () => "test-ws",
  createWorkspaceAwareStorage: (adapter: unknown) => adapter,
  registerForWorkspaceRehydration: () => {},
}));

vi.mock("../paths", () => ({
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

// Records ws.on handlers by event name and the single onAny handler.
function createRecordingWs() {
  const handlers: Record<string, (p: unknown) => void> = {};
  let any: ((msg: { type: string; payload: unknown }) => void) | null = null;
  const ws = {
    on: vi.fn((event: string, handler: (p: unknown) => void) => {
      handlers[event] = handler;
      return () => {};
    }),
    onAny: vi.fn((handler: (msg: { type: string; payload: unknown }) => void) => {
      any = handler;
      return () => {};
    }),
    onReconnect: vi.fn(() => () => {}),
  } as unknown as WSClient;
  return { ws, handlers, emit: (type: string, payload: unknown = {}) => any?.({ type, payload }) };
}

function createStores(): RealtimeSyncStores {
  return {
    authStore: Object.assign(() => ({}), {
      getState: () => ({ user: { id: "u1" } }),
      subscribe: () => () => {},
      setState: () => {},
      destroy: () => {},
    }),
  } as unknown as RealtimeSyncStores;
}

function createWrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

const invalidated = (qc: QueryClient, key: readonly unknown[]) => qc.getQueryState(key)?.isInvalidated;

describe("useRealtimeSync — ext workflow events", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.useFakeTimers();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  });

  afterEach(() => {
    vi.useRealTimers();
    qc.clear();
    vi.clearAllMocks();
  });

  it.each(["ext_workflow:created", "ext_workflow:updated", "ext_workflow:deleted"])(
    "%s invalidates the workflow list and detail caches only",
    (type) => {
      qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
      qc.setQueryData(extWorkflowKeys.detail("ws-1", "wf-1"), {});
      qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
      qc.setQueryData(extWorkflowKeys.list("ws-other"), []);

      const { ws, emit } = createRecordingWs();
      renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

      emit(type, { workflow_id: "wf-1" });
      vi.advanceTimersByTime(150);

      expect(invalidated(qc, extWorkflowKeys.list("ws-1"))).toBe(true);
      expect(invalidated(qc, extWorkflowKeys.detail("ws-1", "wf-1"))).toBe(true);
      expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(false);
      expect(invalidated(qc, extWorkflowKeys.list("ws-other"))).toBe(false);
    },
  );

  it("ext_workflow_run:updated refreshes run caches and the parent issue detail and children", () => {
    qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
    qc.setQueryData(extWorkflowKeys.issueRuns("ws-1", "issue-1"), {});
    qc.setQueryData(extWorkflowKeys.runs("ws-1", "wf-1"), {});
    qc.setQueryData(issueKeys.detail("ws-1", "issue-1"), {});
    qc.setQueryData(issueKeys.children("ws-1", "issue-1"), []);
    qc.setQueryData(issueKeys.detail("ws-1", "issue-2"), {});

    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    handlers["ext_workflow_run:updated"]?.({ run_id: "run-1", issue_id: "issue-1", workflow_id: "wf-1" });

    expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(true);
    expect(invalidated(qc, extWorkflowKeys.issueRuns("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, extWorkflowKeys.runs("ws-1", "wf-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.detail("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.children("ws-1", "issue-1"))).toBe(true);
    expect(invalidated(qc, issueKeys.detail("ws-1", "issue-2"))).toBe(false);
  });

  it("ext_workflow_run:updated tolerates a payload without issue_id", () => {
    qc.setQueryData(extWorkflowKeys.run("ws-1", "run-1"), {});
    const { ws, handlers } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    expect(() => handlers["ext_workflow_run:updated"]?.({ run_id: "run-1" })).not.toThrow();
    expect(invalidated(qc, extWorkflowKeys.run("ws-1", "run-1"))).toBe(true);
  });

  it("does not double-handle ext_workflow_run:updated through the generic prefix path", () => {
    qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
    const { ws, emit } = createRecordingWs();
    renderHook(() => useRealtimeSync(ws, createStores()), { wrapper: createWrapper(qc) });

    emit("ext_workflow_run:updated", { run_id: "run-1", issue_id: "issue-1", workflow_id: "wf-1" });
    vi.advanceTimersByTime(150);

    // The prefix path would have run the `ext_workflow_run` entry (absent by design);
    // the list cache is untouched by the generic dispatcher.
    expect(invalidated(qc, extWorkflowKeys.list("ws-1"))).toBe(false);
  });
});
