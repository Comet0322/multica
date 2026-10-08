import "./env";

import { expect, test, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

// Engine semantics (transitions, rewinds, decisions) are covered by the Go
// tests. This browser test covers the path a member takes: build a workflow in
// the editor, assign an issue to it, and see the run panel and the child issues.
test("a member builds a workflow, assigns an issue, and sees the run and its child issues", async ({ page }) => {
  test.setTimeout(180_000);
  const api = await createTestApi();
  const db = new pg.Client(process.env.DATABASE_URL);
  await db.connect();
  let runtimeId: string | undefined;
  const agentIds: string[] = [];
  let workspaceId: string | undefined;
  let parentId: string | undefined;
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    workspaceId = workspace.id;
    // A previous run that died before its cleanup must not leave an "E2E Flow"
    // behind: the list's empty state is asserted below.
    await purgeExtWorkflowRows(db, workspace.id);
    const user = await db.query<{ id: string }>(`SELECT id FROM "user" WHERE email = $1`, [api.getEmail()]);
    const userId = user.rows[0]!.id;
    const runtime = await db.query<{ id: string }>(
      `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
       VALUES ($1, 'Workflow verification', 'cloud', 'e2e_ext_workflow', 'online', 'E2E fixture', '{}'::jsonb, $2, now()) RETURNING id`,
      [workspace.id, userId],
    );
    runtimeId = runtime.rows[0]!.id;
    for (const name of ["Flow Planner", "Flow Builder"]) {
      const agent = await db.query<{ id: string }>(
        `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id)
         VALUES ($1, $2, '', '', 'cloud', '{}'::jsonb, $3, 'private', 'private', 1, $4) RETURNING id`,
        [workspace.id, name, runtimeId, userId],
      );
      agentIds.push(agent.rows[0]!.id);
    }

    await page.addInitScript((token) => {
      localStorage.setItem("multica_token", token!);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());
    await page.setViewportSize({ width: 1440, height: 1000 });

    // 1. Sidebar entry and empty list.
    await page.goto(`/${workspace.slug}/issues`, { waitUntil: "domcontentloaded" });
    await page.getByRole("link", { name: "Workflows" }).click();
    // The first visit compiles the route on a cold dev server.
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows$`), { timeout: 30_000 });
    await expect(page.getByText("No workflows yet.")).toBeVisible({ timeout: 30_000 });

    // 2. Create dialog: name + supervisor, then land on the detail page.
    await page.getByRole("button", { name: "New workflow" }).first().click();
    const createDialog = page.getByRole("dialog");
    await createDialog.getByLabel("Name").fill("E2E Flow");
    await createDialog.getByRole("button", { name: "Supervisor agent" }).click();
    await pickAgent(page, "Flow Planner");
    await createDialog.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows/[0-9a-f-]{36}$`), { timeout: 30_000 });
    await expect(page.getByRole("heading", { name: "E2E Flow" })).toBeVisible();

    // 3. Node editor: two nodes, the second depends on the first.
    await page.getByRole("button", { name: "Add node" }).click();
    await page.getByRole("button", { name: "Add node" }).click();
    const node1 = page.getByRole("listitem", { name: "Node 1" });
    const node2 = page.getByRole("listitem", { name: "Node 2" });
    // A new node starts without an agent; each step's worker is chosen explicitly.
    await expect(node1.getByRole("button", { name: "Agent" })).toHaveText(/Select agent/);
    await node1.getByLabel("Title").fill("Plan the work");
    await node1.getByRole("button", { name: "Agent" }).click();
    await pickAgent(page, "Flow Planner");
    await node2.getByLabel("Title").fill("Build it");
    await expect(node2.getByLabel("Key")).toHaveValue("build_it");
    await node2.getByRole("button", { name: "Agent" }).click();
    await pickAgent(page, "Flow Builder");
    await node2.getByRole("button", { name: "Depends on" }).click();
    await page.getByRole("button", { name: "Plan the work" }).click();
    await page.keyboard.press("Escape");
    await expect(page.locator("[data-dag-edge]")).toHaveCount(1);

    // 4. Save, then reload: the definition persisted.
    await expect(page.getByText("Unsaved changes")).toBeVisible();
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByText("Unsaved changes")).toBeHidden({ timeout: 30_000 });
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title")).toHaveValue("Build it", {
      timeout: 30_000,
    });
    await expect(page.locator("[data-dag-node]")).toHaveCount(2);

    // 5. A local validation error is shown on the row and blocks the save.
    await page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title").fill("");
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByText("Title is required")).toBeVisible();
    await page.getByRole("button", { name: "Discard changes" }).click();
    await expect(page.getByRole("listitem", { name: "Node 2" }).getByLabel("Title")).toHaveValue("Build it");

    // 6. Assign an issue to the workflow from the issue's assignee picker.
    const parent = await api.createIssue("E2E workflow parent", { status: "todo" });
    parentId = parent.id;
    await page.goto(`/${workspace.slug}/issues/${parent.id}`, { waitUntil: "domcontentloaded" });
    // Wait for the loaded issue before touching its sidebar, then target the
    // Assignee property row only. The sidebar re-renders while the issue's
    // queries settle, which can close a picker that just opened, so retry
    // open-and-pick until the confirmation dialog is up.
    await expect(page.getByText("E2E workflow parent").first()).toBeVisible({ timeout: 45_000 });
    const assigneeRow = page.getByText("Assignee", { exact: true }).locator("xpath=..");
    const assigneeTrigger = assigneeRow.getByRole("button", { name: "Unassigned" });
    await expect(assigneeTrigger).toBeVisible({ timeout: 45_000 });
    const pickerSearch = page.getByPlaceholder("Assign to...");
    const confirmAssignment = page.getByRole("button", { name: "Confirm assignment" });
    await expect(async () => {
      if (!(await confirmAssignment.isVisible())) {
        if (!(await pickerSearch.isVisible())) await assigneeTrigger.click({ timeout: 5_000 });
        await expect(pickerSearch).toBeVisible({ timeout: 2_000 });
        await page.getByRole("button", { name: /E2E Flow/ }).click({ timeout: 2_000 });
      }
      await expect(confirmAssignment).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 60_000 });
    await confirmAssignment.click();

    // 7. The run panel lists both steps, each linking to its child issue.
    const runPanel = page.locator("[data-ext-workflow-run]");
    await expect(runPanel).toBeVisible({ timeout: 45_000 });
    await expect(runPanel.getByRole("button", { name: /Workflow run/ })).toBeVisible();
    await expect(runPanel.getByText("Plan the work", { exact: true })).toBeVisible();
    await expect(runPanel.getByText("Build it", { exact: true })).toBeVisible();
    await expect(runPanel.getByRole("link", { name: "Open step issue" })).toHaveCount(2);

    const children = await db.query<{ id: string; title: string }>(
      `SELECT id, title FROM issue WHERE parent_issue_id = $1 ORDER BY created_at`,
      [parent.id],
    );
    expect(children.rows.map((r) => r.title).sort()).toEqual([
      "E2E workflow parent · Build it",
      "E2E workflow parent · Plan the work",
    ]);
    const run = await db.query<{ status: string }>(
      `SELECT status FROM ext_workflow_run WHERE issue_id = $1`,
      [parent.id],
    );
    expect(run.rows).toEqual([{ status: "running" }]);

    // 8. A child issue says which step it is and links back to the parent.
    await runPanel.getByRole("link", { name: "Open step issue" }).first().click();
    await expect(page.getByRole("link", { name: /Workflow step 1 of 2/ })).toBeVisible({ timeout: 45_000 });

    // 9. The workflow's Runs tab lists the run.
    await page.goto(`/${workspace.slug}/workflows`, { waitUntil: "domcontentloaded" });
    await page.getByText("E2E Flow").click();
    await page.getByRole("button", { name: "Runs" }).click();
    await expect(page.getByText("E2E workflow parent")).toBeVisible({ timeout: 30_000 });
  } finally {
    // Each statement is guarded so one failure cannot skip the rest, and the
    // connection is always closed.
    try {
      if (workspaceId) await purgeExtWorkflowRows(db, workspaceId, guarded);
      if (parentId) {
        await guarded("child issue tasks", () =>
          db.query(
            `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1 OR id = $1)`,
            [parentId],
          ),
        );
        await guarded("child issues", () => db.query(`DELETE FROM issue WHERE parent_issue_id = $1`, [parentId]));
      }
      await guarded("api cleanup", () => api.cleanup());
      for (const id of agentIds) await guarded(`agent ${id}`, () => db.query(`DELETE FROM agent WHERE id = $1`, [id]));
      if (runtimeId) await guarded("runtime", () => db.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]));
    } finally {
      await db.end();
    }
  }
});

// Picks an agent in the open picker and waits for the picker to finish closing,
// so the next picker's options are the only ones on the page.
async function pickAgent(page: Page, name: string) {
  const popover = page.locator('[data-slot="popover-content"]');
  await popover.getByRole("button", { name: new RegExp(name) }).click();
  await expect(popover).toHaveCount(0);
}

type Step = (label: string, fn: () => Promise<unknown>) => Promise<unknown>;

// No foreign keys: delete the ext rows explicitly, run tasks and children
// before their parents. `step` wraps each statement: plain before the test
// (a failure should fail it), guarded during cleanup.
async function purgeExtWorkflowRows(db: pg.Client, workspaceId: string, step: Step = (_label, fn) => fn()) {
  await step("ext workflow run tasks", () =>
    db.query(
      `DELETE FROM agent_task_queue WHERE ext_workflow_run_id IN (SELECT id FROM ext_workflow_run WHERE workspace_id = $1)`,
      [workspaceId],
    ),
  );
  for (const table of ["ext_workflow_run_event", "ext_workflow_run_step", "ext_workflow_run", "ext_workflow_node", "ext_workflow"]) {
    await step(table, () => db.query(`DELETE FROM ${table} WHERE workspace_id = $1`, [workspaceId]));
  }
}

async function guarded(label: string, fn: () => Promise<unknown>) {
  try {
    await fn();
  } catch (err) {
    console.warn(`ext-workflow e2e cleanup (${label}) failed:`, err);
  }
}
