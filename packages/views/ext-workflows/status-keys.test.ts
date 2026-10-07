// @vitest-environment node
import { describe, expect, it } from "vitest";
import { runStatusKey, stepStatusKey } from "./status-keys";

describe("status keys", () => {
  it("passes known statuses through", () => {
    expect(stepStatusKey("awaiting_human")).toBe("awaiting_human");
    expect(runStatusKey("waiting_human")).toBe("waiting_human");
  });
  it("downgrades unknown server enums to unknown", () => {
    expect(stepStatusKey("some_future_status")).toBe("unknown");
    expect(runStatusKey("paused")).toBe("unknown");
    expect(stepStatusKey(undefined)).toBe("unknown");
  });
});
