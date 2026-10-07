// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  EXT_MAX_NODES,
  computeDepths,
  createRow,
  dependencyOptions,
  descendantIds,
  extractValidationErrors,
  layoutDag,
  mapServerErrors,
  moveRow,
  nodesToRows,
  removeRow,
  rowsToInput,
  serializeRows,
  setRowKey,
  setRowTitle,
  slugify,
  uniqueKey,
  validateRows,
  type NodeRow,
} from "./node-editor-helpers";

function row(id: string, key: string, over: Partial<NodeRow> = {}): NodeRow {
  return {
    rowId: id,
    key,
    keyTouched: true,
    title: key.toUpperCase(),
    agent_id: "agent-1",
    prompt: "",
    requires_review: false,
    max_attempts: 3,
    depends_on: [],
    ...over,
  };
}

describe("slugify / uniqueKey", () => {
  it("lowercases, strips accents and collapses punctuation", () => {
    expect(slugify("Review PR #12!")).toBe("review_pr_12");
    expect(slugify("Café résumé")).toBe("cafe_resume");
    expect(slugify("  --Hello   World--  ")).toBe("hello_world");
  });
  it("returns an empty string when nothing ASCII survives", () => {
    expect(slugify("设计方案")).toBe("");
  });
  it("caps at 40 characters without a trailing underscore", () => {
    const key = slugify(`${"a".repeat(39)} b`);
    expect(key.length).toBeLessThanOrEqual(40);
    expect(key.endsWith("_")).toBe(false);
  });
  it("uniquifies against taken keys and keeps the 40 character cap", () => {
    expect(uniqueKey("plan", new Set(["plan"]))).toBe("plan_2");
    expect(uniqueKey("plan", new Set(["plan", "plan_2"]))).toBe("plan_3");
    const long = "x".repeat(40);
    const next = uniqueKey(long, new Set([long]));
    expect(next.length).toBe(40);
    expect(next.endsWith("_2")).toBe(true);
  });
});

describe("row editing", () => {
  it("derives the key from the title until the key is edited by hand", () => {
    let rows = [createRow([])];
    rows[0] = { ...rows[0]!, keyTouched: false };
    const id = rows[0]!.rowId;
    rows = setRowTitle(rows, id, "Write the Spec");
    expect(rows[0]!.key).toBe("write_the_spec");
    rows = setRowKey(rows, id, "spec");
    rows = setRowTitle(rows, id, "Write the Spec v2");
    expect(rows[0]!.key).toBe("spec");
    expect(rows[0]!.keyTouched).toBe(true);
  });
  it("keeps derived keys unique across rows and falls back to node_N", () => {
    const a = { ...row("a", "plan"), keyTouched: true };
    const b = { ...createRow([a]), keyTouched: false };
    const rows = setRowTitle([a, b], b.rowId, "Plan");
    expect(rows[1]!.key).toBe("plan_2");
    const c = { ...createRow(rows), keyTouched: false };
    const rows2 = setRowTitle([...rows, c], c.rowId, "设计");
    expect(rows2[2]!.key).toMatch(/^node_3/);
  });
  it("removing a row strips it from every depends_on", () => {
    const rows = [row("a", "a"), row("b", "b", { depends_on: ["a"] }), row("c", "c", { depends_on: ["a", "b"] })];
    const next = removeRow(rows, "a");
    expect(next.map((r) => r.rowId)).toEqual(["b", "c"]);
    expect(next[0]!.depends_on).toEqual([]);
    expect(next[1]!.depends_on).toEqual(["b"]);
  });
  it("moves rows up and down and ignores out-of-range moves", () => {
    const rows = [row("a", "a"), row("b", "b"), row("c", "c")];
    expect(moveRow(rows, 1, -1).map((r) => r.rowId)).toEqual(["b", "a", "c"]);
    expect(moveRow(rows, 1, 1).map((r) => r.rowId)).toEqual(["a", "c", "b"]);
    expect(moveRow(rows, 0, -1)).toBe(rows);
    expect(moveRow(rows, 2, 1)).toBe(rows);
  });
});

describe("cycle-safe dependency options", () => {
  // a <- b <- c  (b depends on a, c depends on b); d is independent.
  const rows = [
    row("a", "a"),
    row("b", "b", { depends_on: ["a"] }),
    row("c", "c", { depends_on: ["b"] }),
    row("d", "d"),
  ];
  it("descendantIds returns everything downstream, transitively", () => {
    expect([...descendantIds(rows, "a")].sort()).toEqual(["b", "c"]);
    expect([...descendantIds(rows, "b")]).toEqual(["c"]);
    expect([...descendantIds(rows, "d")]).toEqual([]);
  });
  it("excludes self and every downstream node, keeps the rest", () => {
    expect(dependencyOptions(rows, "a").map((r) => r.rowId)).toEqual(["d"]);
    expect(dependencyOptions(rows, "b").map((r) => r.rowId)).toEqual(["a", "d"]);
    expect(dependencyOptions(rows, "c").map((r) => r.rowId)).toEqual(["a", "b", "d"]);
    expect(dependencyOptions(rows, "d").map((r) => r.rowId)).toEqual(["a", "b", "c"]);
  });
  it("never offers an option that would close a cycle", () => {
    for (const target of rows) {
      for (const option of dependencyOptions(rows, target.rowId)) {
        const next = rows.map((r) =>
          r.rowId === target.rowId ? { ...r, depends_on: [...r.depends_on, option.rowId] } : r,
        );
        expect(validateRows(next).rows[target.rowId]?.depends_on).toBeUndefined();
      }
    }
  });
  it("terminates on an already-cyclic graph", () => {
    const cyclic = [row("a", "a", { depends_on: ["b"] }), row("b", "b", { depends_on: ["a"] })];
    expect(() => descendantIds(cyclic, "a")).not.toThrow();
  });
});

describe("validateRows", () => {
  it("accepts a valid graph", () => {
    const rows = [row("a", "a"), row("b", "b", { depends_on: ["a"] })];
    const v = validateRows(rows);
    expect(v.valid).toBe(true);
    expect(v.rows).toEqual({});
    expect(v.general).toEqual([]);
  });
  it("flags key problems", () => {
    const v = validateRows([
      row("a", ""),
      row("b", "Bad Key"),
      row("c", "dup"),
      row("d", "dup"),
    ]);
    expect(v.rows.a?.key).toBe("key_required");
    expect(v.rows.b?.key).toBe("key_invalid");
    expect(v.rows.c?.key).toBe("key_duplicate");
    expect(v.rows.d?.key).toBe("key_duplicate");
    expect(v.valid).toBe(false);
  });
  it("flags title, agent, attempts and prompt size", () => {
    const v = validateRows([
      row("a", "a", { title: "  ", agent_id: "", max_attempts: 11, prompt: "x".repeat(20001) }),
      row("b", "b", { max_attempts: 0 }),
      row("c", "c", { max_attempts: 2.5 }),
      row("d", "d", { prompt: "é".repeat(10001) }),
    ]);
    expect(v.rows.a).toMatchObject({
      title: "title_required",
      agent_id: "agent_required",
      max_attempts: "max_attempts_range",
      prompt: "prompt_too_long",
    });
    expect(v.rows.b?.max_attempts).toBe("max_attempts_range");
    expect(v.rows.c?.max_attempts).toBe("max_attempts_range");
    expect(v.rows.d?.prompt).toBe("prompt_too_long");
  });
  it("flags self, unknown and cyclic dependencies", () => {
    const v = validateRows([
      row("a", "a", { depends_on: ["a"] }),
      row("b", "b", { depends_on: ["ghost"] }),
      row("c", "c", { depends_on: ["d"] }),
      row("d", "d", { depends_on: ["c"] }),
    ]);
    expect(v.rows.a?.depends_on).toBe("self_dependency");
    expect(v.rows.b?.depends_on).toBe("unknown_dependency");
    expect(v.rows.c?.depends_on).toBe("cycle");
    expect(v.rows.d?.depends_on).toBe("cycle");
  });
  it("flags node-count problems", () => {
    expect(validateRows([]).general).toEqual(["no_nodes"]);
    const many = Array.from({ length: EXT_MAX_NODES + 1 }, (_, i) => row(`r${i}`, `n${i}`));
    expect(validateRows(many).general).toEqual(["too_many_nodes"]);
  });
});

describe("server error mapping", () => {
  const rows = [row("a", "plan"), row("b", "build")];
  it("maps node errors onto rows by key and known field", () => {
    const mapped = mapServerErrors(
      [
        { node_key: "build", field: "agent_id", message: "agent is archived" },
        { node_key: "plan", field: "depends_on", message: "cycle: plan -> build -> plan" },
      ],
      rows,
    );
    expect(mapped.byRow.b).toEqual({ agent_id: "agent is archived" });
    expect(mapped.byRow.a).toEqual({ depends_on: "cycle: plan -> build -> plan" });
    expect(mapped.general).toEqual([]);
  });
  it("puts unknown fields on the row and keyless errors in general", () => {
    const mapped = mapServerErrors(
      [
        { node_key: "plan", field: "something_new", message: "nope" },
        { field: "supervisor_agent_id", message: "supervisor is archived" },
        { node_key: "gone", field: "key", message: "stale" },
      ],
      rows,
    );
    expect(mapped.byRow.a).toEqual({ row: "nope" });
    expect(mapped.general).toEqual(["supervisor is archived", "stale"]);
  });
  it("extractValidationErrors duck-types the typed 422 error", () => {
    const err = Object.assign(new Error("validation_failed"), {
      errors: [{ node_key: "plan", field: "key", message: "bad" }],
    });
    expect(extractValidationErrors(err)).toEqual([{ node_key: "plan", field: "key", message: "bad" }]);
    expect(extractValidationErrors(new Error("boom"))).toBeNull();
    expect(extractValidationErrors({ errors: "nope" })).toBeNull();
    expect(extractValidationErrors(null)).toBeNull();
  });
});

describe("API round trip", () => {
  const nodes = [
    { id: "n2", key: "build", title: "Build", agent_id: "ag", prompt: "p2", requires_review: true, max_attempts: 2, depends_on: ["plan"], position: 1 },
    { id: "n1", key: "plan", title: "Plan", agent_id: "ag", prompt: "p1", requires_review: false, max_attempts: 3, depends_on: [], position: 0 },
  ];
  it("sorts by position, resolves dependencies to row ids and back to keys", () => {
    const rows = nodesToRows(nodes);
    expect(rows.map((r) => r.key)).toEqual(["plan", "build"]);
    expect(rows[1]!.depends_on).toEqual([rows[0]!.rowId]);
    expect(rowsToInput(rows)).toEqual([
      { key: "plan", title: "Plan", agent_id: "ag", prompt: "p1", requires_review: false, max_attempts: 3, depends_on: [] },
      { key: "build", title: "Build", agent_id: "ag", prompt: "p2", requires_review: true, max_attempts: 2, depends_on: ["plan"] },
    ]);
  });
  it("keeps an unknown dependency visible so validation can flag it", () => {
    const rows = nodesToRows([{ ...nodes[1]!, depends_on: ["ghost"] }]);
    expect(validateRows(rows).rows[rows[0]!.rowId]?.depends_on).toBe("unknown_dependency");
    expect(rowsToInput(rows)[0]!.depends_on).toEqual(["ghost"]);
  });
  it("serializeRows ignores row ids, so a fresh load is not dirty", () => {
    expect(serializeRows(nodesToRows(nodes))).toBe(serializeRows(nodesToRows(nodes)));
  });
});

describe("DAG layout", () => {
  const rows = [
    row("a", "a"),
    row("b", "b", { depends_on: ["a"] }),
    row("c", "c", { depends_on: ["a"] }),
    row("d", "d", { depends_on: ["b", "c"] }),
  ];
  it("computes longest-path depth", () => {
    const depths = computeDepths(rows);
    expect([depths.get("a"), depths.get("b"), depths.get("c"), depths.get("d")]).toEqual([0, 1, 1, 2]);
  });
  it("does not loop on cycles", () => {
    const depths = computeDepths([row("a", "a", { depends_on: ["b"] }), row("b", "b", { depends_on: ["a"] })]);
    expect(depths.size).toBe(2);
  });
  it("places nodes in columns by depth and emits one edge per dependency", () => {
    const layout = layoutDag(rows);
    const byId = new Map(layout.nodes.map((n) => [n.rowId, n]));
    expect(byId.get("a")!.x).toBeLessThan(byId.get("b")!.x);
    expect(byId.get("b")!.x).toBe(byId.get("c")!.x);
    expect(byId.get("b")!.y).not.toBe(byId.get("c")!.y);
    expect(byId.get("b")!.x).toBeLessThan(byId.get("d")!.x);
    expect(layout.edges).toHaveLength(4);
    expect(layout.edges.every((e) => e.d.startsWith("M "))).toBe(true);
    expect(layout.width).toBeGreaterThan(byId.get("d")!.x);
    expect(layout.height).toBeGreaterThan(byId.get("c")!.y);
  });
  it("labels with the title, then the key, and skips edges to missing nodes", () => {
    const layout = layoutDag([
      row("a", "plan", { title: "" }),
      row("b", "build", { depends_on: ["a", "ghost"] }),
    ]);
    expect(layout.nodes[0]!.label).toBe("plan");
    expect(layout.edges).toHaveLength(1);
  });
  it("returns an empty layout for no rows", () => {
    expect(layoutDag([])).toEqual({ nodes: [], edges: [], width: 0, height: 0 });
  });
});
