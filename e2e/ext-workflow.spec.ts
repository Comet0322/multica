import "./env";

import { expect, test } from "@playwright/test";
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
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows$`));
    await expect(page.getByText("No workflows yet.")).toBeVisible({ timeout: 30_000 });

    // 2. Create dialog: name + supervisor, then land on the detail page.
    await page.getByRole("button", { name: "New workflow" }).first().click();
    const createDialog = page.getByRole("dialog");
    await createDialog.getByLabel("Name").fill("E2E Flow");
    await createDialog.getByRole("button", { name: "Supervisor agent" }).click();
    await page.getByRole("button", { name: /Flow Planner/ }).click();
    await createDialog.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL(new RegExp(`/${workspace.slug}/workflows/[0-9a-f-]{36}$`), { timeout: 30_000 });
    await expect(page.getByRole("heading", { name: "E2E Flow" })).toBeVisible();

    // 3. Node editor: two nodes, the second depends on the first.
    await page.getByRole("button", { name: "Add node" }).click();
    await page.getByRole("button", { name: "Add node" }).click();
    const node1 = page.getByRole("listitem", { name: "Node 1" });
    const node2 = page.getByRole("listitem", { name: "Node 2" });
    await node1.getByLabel("Title").fill("Plan the work");
    await node2.getByLabel("Title").fill("Build it");
    await expect(node2.getByLabel("Key")).toHaveValue("build_it");
    await node2.getByRole("button", { name: "Agent" }).click();
    await page.getByRole("button", { name: /Flow Builder/ }).click();
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
    await page.getByRole("button", { name: "Unassigned" }).first().click({ timeout: 45_000 });
    await page.getByRole("button", { name: /E2E Flow/ }).click();
    await page.getByRole("button", { name: "Confirm assignment" }).click();

    // 7. The run panel lists both steps, each linking to its child issue.
    await expect(page.getByText("Workflow run")).toBeVisible({ timeout: 45_000 });
    await expect(page.getByText("Plan the work", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("Build it", { exact: true }).first()).toBeVisible();
    await expect(page.getByRole("link", { name: "Open step issue" })).toHaveCount(2);

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
    await page.getByRole("link", { name: "Open step issue" }).first().click();
    await expect(page.getByRole("link", { name: /Workflow step 1 of 2/ })).toBeVisible({ timeout: 45_000 });

    // 9. The workflow's Runs tab lists the run.
    await page.goto(`/${workspace.slug}/workflows`, { waitUntil: "domcontentloaded" });
    await page.getByText("E2E Flow").click();
    await page.getByRole("button", { name: "Runs" }).click();
    await expect(page.getByText("E2E workflow parent")).toBeVisible({ timeout: 30_000 });
  } finally {
    if (workspaceId) {
      // No foreign keys: delete ext rows explicitly, children and tasks before their parent.
      await db.query(`DELETE FROM ext_workflow_run_event WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_run_step WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_run WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow_node WHERE workspace_id = $1`, [workspaceId]);
      await db.query(`DELETE FROM ext_workflow WHERE workspace_id = $1`, [workspaceId]);
    }
    if (parentId) {
      await db.query(
        `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id = $1 OR id = $1)`,
        [parentId],
      );
      await db.query(`DELETE FROM issue WHERE parent_issue_id = $1`, [parentId]);
    }
    await api.cleanup();
    for (const id of agentIds) await db.query(`DELETE FROM agent WHERE id = $1`, [id]);
    if (runtimeId) await db.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]);
    await db.end();
  }
});
