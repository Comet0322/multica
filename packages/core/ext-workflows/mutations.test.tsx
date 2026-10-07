/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, act } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { ExtWorkflowValidationFailed } from "./errors";
import { useCreateExtWorkflow, useDecideExtWorkflowStep, useUpdateExtWorkflow } from "./mutations";
import { extWorkflowKeys } from "./queries";

const wrapper = (qc: QueryClient) =>
  function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };

afterEach(() => vi.restoreAllMocks());

describe("ext workflow mutations", () => {
  it("create invalidates the list for the given workspace", async () => {
    const qc = new QueryClient();
    qc.setQueryData(extWorkflowKeys.list("ws-1"), []);
    setApiInstance({ createExtWorkflow: vi.fn().mockResolvedValue({ id: "wf-1" }) } as unknown as ApiClient);
    const { result } = renderHook(() => useCreateExtWorkflow("ws-1"), { wrapper: wrapper(qc) });
    await act(() => result.current.mutateAsync({ name: "x", supervisor_agent_id: "a" }));
    expect(qc.getQueryState(extWorkflowKeys.list("ws-1"))?.isInvalidated).toBe(true);
  });

  it("update seeds the detail cache and rejects with ExtWorkflowValidationFailed", async () => {
    const qc = new QueryClient();
    const updateExtWorkflow = vi
      .fn()
      .mockResolvedValueOnce({ id: "wf-1", name: "n", nodes: [] })
      .mockRejectedValueOnce(new ExtWorkflowValidationFailed([{ field: "nodes", message: "bad" }]));
    setApiInstance({ updateExtWorkflow } as unknown as ApiClient);
    const { result } = renderHook(() => useUpdateExtWorkflow("ws-1"), { wrapper: wrapper(qc) });
    await act(() => result.current.mutateAsync({ id: "wf-1", name: "n" }));
    expect(qc.getQueryData(extWorkflowKeys.detail("ws-1", "wf-1"))).toMatchObject({ id: "wf-1" });
    await expect(act(() => result.current.mutateAsync({ id: "wf-1", nodes: [] }))).rejects.toMatchObject({
      errors: [{ field: "nodes", message: "bad" }],
    });
  });

  it("decide exposes the HTTP status on failure and invalidates run caches", async () => {
    const qc = new QueryClient();
    qc.setQueryData(extWorkflowKeys.run("ws-1", "r1"), {});
    setApiInstance({
      decideExtWorkflowStep: vi.fn().mockRejectedValue(new ApiError("mismatch", 409, "Conflict")),
    } as unknown as ApiClient);
    const { result } = renderHook(() => useDecideExtWorkflowStep("ws-1"), { wrapper: wrapper(qc) });
    await expect(
      act(() => result.current.mutateAsync({ runId: "r1", stepId: "s1", action: "approve", expected_status: "awaiting_human" })),
    ).rejects.toMatchObject({ status: 409 });
    expect(qc.getQueryState(extWorkflowKeys.run("ws-1", "r1"))?.isInvalidated).toBe(true);
  });

  it("decide resolves with null on a 204 and still invalidates run caches", async () => {
    const qc = new QueryClient();
    qc.setQueryData(extWorkflowKeys.run("ws-1", "r1"), {});
    setApiInstance({ decideExtWorkflowStep: vi.fn().mockResolvedValue(null) } as unknown as ApiClient);
    const { result } = renderHook(() => useDecideExtWorkflowStep("ws-1"), { wrapper: wrapper(qc) });
    await expect(
      act(() => result.current.mutateAsync({ runId: "r1", stepId: "s1", action: "approve", expected_status: "awaiting_human" })),
    ).resolves.toBeNull();
    expect(qc.getQueryState(extWorkflowKeys.run("ws-1", "r1"))?.isInvalidated).toBe(true);
  });
});
