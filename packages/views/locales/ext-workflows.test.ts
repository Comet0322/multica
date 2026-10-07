// @vitest-environment node
import { describe, expect, it } from "vitest";
import { RESOURCES } from "./index";

const LOCALES = ["en", "zh-Hans", "ko", "ja", "fr"] as const;

describe("ext-workflows locale wiring", () => {
  for (const locale of LOCALES) {
    it(`${locale}: registers the ext-workflows namespace`, () => {
      const ns = RESOURCES[locale]["ext-workflows"] as Record<string, unknown> | undefined;
      expect(ns).toBeDefined();
      expect(ns).toHaveProperty("page.title");
      expect(ns).toHaveProperty("run_section.action_approve");
      expect(ns).toHaveProperty("step_status.awaiting_human");
    });

    it(`${locale}: adds the cross-namespace keys`, () => {
      const r = RESOURCES[locale] as Record<string, Record<string, unknown>>;
      expect(r.layout).toHaveProperty("nav.workflows");
      expect(r.layout).toHaveProperty("tab.workflow");
      expect(r.settings).toHaveProperty("shortcuts.actions.goWorkflows.label");
      expect(r.issues).toHaveProperty("pickers.assignee.workflows_group");
      expect(r.issues).toHaveProperty("pickers.assignee.workflow_needs_nodes");
      expect(r.modals).toHaveProperty("run_confirm.create_will_start_workflow");
      expect(r.inbox).toHaveProperty("types.ext_workflow_escalation");
    });
  }
});
