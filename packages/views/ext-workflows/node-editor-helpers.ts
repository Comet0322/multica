import type {
  ExtWorkflowNode,
  ExtWorkflowNodeInput,
  ExtWorkflowValidationError,
} from "@multica/core/ext-workflows";

export const EXT_MAX_NODES = 50;
export const EXT_MAX_PROMPT_BYTES = 20000;
export const EXT_KEY_PATTERN = /^[a-z0-9_-]{1,40}$/;
const DEFAULT_MAX_ATTEMPTS = 3;
const MISSING_PREFIX = "missing:";

/** Editor row. `depends_on` holds ROW IDS (not keys): renaming a key never rewrites other rows. */
export interface NodeRow {
  rowId: string;
  key: string;
  /** false while the key still follows the title; true once edited or loaded from the server. */
  keyTouched: boolean;
  title: string;
  agent_id: string;
  prompt: string;
  requires_review: boolean;
  max_attempts: number;
  depends_on: string[];
}

export type NodeField =
  | "key"
  | "title"
  | "agent_id"
  | "prompt"
  | "max_attempts"
  | "depends_on"
  | "row";

export type RowErrorCode =
  | "key_required"
  | "key_invalid"
  | "key_duplicate"
  | "title_required"
  | "agent_required"
  | "prompt_too_long"
  | "max_attempts_range"
  | "self_dependency"
  | "unknown_dependency"
  | "cycle";

export type GeneralErrorCode = "too_many_nodes" | "no_nodes";
export type RowErrors = Record<string, Partial<Record<NodeField, RowErrorCode>>>;

export interface RowValidation {
  rows: RowErrors;
  general: GeneralErrorCode[];
  valid: boolean;
}

let rowSeq = 0;
export function newRowId(): string {
  rowSeq += 1;
  return `row-${rowSeq}`;
}

// ---------------------------------------------------------------- keys

export function slugify(title: string): string {
  return title
    .normalize("NFKD")
    .replace(/[\u0300-\u036f]/g, "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_+|_+$/g, "")
    .slice(0, 40)
    .replace(/_+$/, "");
}

export function uniqueKey(base: string, taken: ReadonlySet<string>): string {
  if (!taken.has(base)) return base;
  for (let i = 2; ; i += 1) {
    const suffix = `_${i}`;
    const candidate = base.slice(0, 40 - suffix.length) + suffix;
    if (!taken.has(candidate)) return candidate;
  }
}

function deriveKey(title: string, position: number, taken: ReadonlySet<string>): string {
  return uniqueKey(slugify(title) || `node_${position}`, taken);
}

// ---------------------------------------------------------------- row editing

export function createRow(existing: readonly NodeRow[], agentId = ""): NodeRow {
  const taken = new Set(existing.map((r) => r.key));
  return {
    rowId: newRowId(),
    key: uniqueKey(`node_${existing.length + 1}`, taken),
    keyTouched: false,
    title: "",
    agent_id: agentId,
    prompt: "",
    requires_review: false,
    max_attempts: DEFAULT_MAX_ATTEMPTS,
    depends_on: [],
  };
}

export function setRowTitle(rows: NodeRow[], rowId: string, title: string): NodeRow[] {
  const index = rows.findIndex((r) => r.rowId === rowId);
  if (index < 0) return rows;
  const current = rows[index]!;
  if (current.keyTouched) {
    return rows.map((r) => (r.rowId === rowId ? { ...r, title } : r));
  }
  const taken = new Set(rows.filter((r) => r.rowId !== rowId).map((r) => r.key));
  const key = deriveKey(title, index + 1, taken);
  return rows.map((r) => (r.rowId === rowId ? { ...r, title, key } : r));
}

export function setRowKey(rows: NodeRow[], rowId: string, key: string): NodeRow[] {
  return rows.map((r) => (r.rowId === rowId ? { ...r, key, keyTouched: true } : r));
}

export function patchRow(rows: NodeRow[], rowId: string, patch: Partial<NodeRow>): NodeRow[] {
  return rows.map((r) => (r.rowId === rowId ? { ...r, ...patch } : r));
}

export function removeRow(rows: NodeRow[], rowId: string): NodeRow[] {
  return rows
    .filter((r) => r.rowId !== rowId)
    .map((r) =>
      r.depends_on.includes(rowId)
        ? { ...r, depends_on: r.depends_on.filter((d) => d !== rowId) }
        : r,
    );
}

export function moveRow(rows: NodeRow[], index: number, delta: -1 | 1): NodeRow[] {
  const target = index + delta;
  if (index < 0 || index >= rows.length || target < 0 || target >= rows.length) return rows;
  const next = [...rows];
  const [moved] = next.splice(index, 1);
  next.splice(target, 0, moved!);
  return next;
}

// ---------------------------------------------------------------- dependencies

/** Rows that depend (transitively) on `rowId`, excluding itself. Terminates on cyclic input. */
export function descendantIds(rows: readonly NodeRow[], rowId: string): Set<string> {
  const children = new Map<string, string[]>();
  for (const r of rows) {
    for (const d of r.depends_on) {
      const list = children.get(d);
      if (list) list.push(r.rowId);
      else children.set(d, [r.rowId]);
    }
  }
  const out = new Set<string>();
  const stack = [rowId];
  while (stack.length > 0) {
    const id = stack.pop()!;
    for (const child of children.get(id) ?? []) {
      if (!out.has(child)) {
        out.add(child);
        stack.push(child);
      }
    }
  }
  out.delete(rowId);
  return out;
}

/**
 * Rows `rowId` may depend on. Excludes itself and every downstream row:
 * depending on a descendant would close a cycle.
 */
export function dependencyOptions(rows: readonly NodeRow[], rowId: string): NodeRow[] {
  const blocked = descendantIds(rows, rowId);
  return rows.filter((r) => r.rowId !== rowId && !blocked.has(r.rowId));
}

// ---------------------------------------------------------------- validation

function utf8Length(value: string): number {
  return new TextEncoder().encode(value).length;
}

function cyclicIds(rows: readonly NodeRow[]): Set<string> {
  const ids = new Set(rows.map((r) => r.rowId));
  const indegree = new Map<string, number>();
  const out = new Map<string, string[]>();
  for (const r of rows) indegree.set(r.rowId, 0);
  for (const r of rows) {
    for (const d of new Set(r.depends_on)) {
      if (d === r.rowId || !ids.has(d)) continue;
      indegree.set(r.rowId, (indegree.get(r.rowId) ?? 0) + 1);
      const list = out.get(d);
      if (list) list.push(r.rowId);
      else out.set(d, [r.rowId]);
    }
  }
  const queue = rows.filter((r) => (indegree.get(r.rowId) ?? 0) === 0).map((r) => r.rowId);
  const done = new Set<string>();
  while (queue.length > 0) {
    const id = queue.shift()!;
    done.add(id);
    for (const next of out.get(id) ?? []) {
      const left = (indegree.get(next) ?? 0) - 1;
      indegree.set(next, left);
      if (left === 0) queue.push(next);
    }
  }
  return new Set(rows.filter((r) => !done.has(r.rowId)).map((r) => r.rowId));
}

export function validateRows(rows: readonly NodeRow[]): RowValidation {
  const errors: RowErrors = {};
  const general: GeneralErrorCode[] = [];
  const set = (rowId: string, field: NodeField, code: RowErrorCode) => {
    errors[rowId] = { ...errors[rowId], [field]: code };
  };

  if (rows.length === 0) general.push("no_nodes");
  if (rows.length > EXT_MAX_NODES) general.push("too_many_nodes");

  const keyCount = new Map<string, number>();
  for (const r of rows) keyCount.set(r.key, (keyCount.get(r.key) ?? 0) + 1);
  const ids = new Set(rows.map((r) => r.rowId));
  const cyclic = cyclicIds(rows);

  for (const r of rows) {
    if (r.key === "") set(r.rowId, "key", "key_required");
    else if (!EXT_KEY_PATTERN.test(r.key)) set(r.rowId, "key", "key_invalid");
    else if ((keyCount.get(r.key) ?? 0) > 1) set(r.rowId, "key", "key_duplicate");

    if (r.title.trim() === "") set(r.rowId, "title", "title_required");
    if (r.agent_id === "") set(r.rowId, "agent_id", "agent_required");
    if (utf8Length(r.prompt) > EXT_MAX_PROMPT_BYTES) set(r.rowId, "prompt", "prompt_too_long");
    if (!Number.isInteger(r.max_attempts) || r.max_attempts < 1 || r.max_attempts > 10) {
      set(r.rowId, "max_attempts", "max_attempts_range");
    }

    if (r.depends_on.includes(r.rowId)) set(r.rowId, "depends_on", "self_dependency");
    else if (r.depends_on.some((d) => !ids.has(d))) set(r.rowId, "depends_on", "unknown_dependency");
    else if (cyclic.has(r.rowId)) set(r.rowId, "depends_on", "cycle");
  }

  return { rows: errors, general, valid: Object.keys(errors).length === 0 && general.length === 0 };
}

// ---------------------------------------------------------------- server errors

const NODE_FIELDS: ReadonlySet<string> = new Set([
  "key",
  "title",
  "agent_id",
  "prompt",
  "max_attempts",
  "depends_on",
]);

export interface MappedServerErrors {
  byRow: Record<string, Partial<Record<NodeField, string>>>;
  general: string[];
}

/** Maps 422 `errors[]` onto rows by `node_key`. Anything that has no row goes to `general`. */
export function mapServerErrors(
  errors: readonly ExtWorkflowValidationError[],
  rows: readonly NodeRow[],
): MappedServerErrors {
  const byRow: MappedServerErrors["byRow"] = {};
  const general: string[] = [];
  for (const e of errors) {
    const row = e.node_key ? rows.find((r) => r.key === e.node_key) : undefined;
    if (!row) {
      general.push(e.message);
      continue;
    }
    const field = (NODE_FIELDS.has(e.field) ? e.field : "row") as NodeField;
    byRow[row.rowId] = { ...byRow[row.rowId], [field]: e.message };
  }
  return { byRow, general };
}

/** Duck-types the typed 422 error (`ExtWorkflowValidationFailed`) so views need not import the class. */
export function extractValidationErrors(err: unknown): ExtWorkflowValidationError[] | null {
  if (!err || typeof err !== "object") return null;
  const errors = (err as { errors?: unknown }).errors;
  if (!Array.isArray(errors)) return null;
  const valid = errors.filter(
    (e): e is ExtWorkflowValidationError =>
      !!e &&
      typeof e === "object" &&
      typeof (e as { field?: unknown }).field === "string" &&
      typeof (e as { message?: unknown }).message === "string",
  );
  return valid.length > 0 ? valid : null;
}

// ---------------------------------------------------------------- API mapping

export function nodesToRows(nodes: readonly ExtWorkflowNode[]): NodeRow[] {
  const sorted = [...nodes].sort((a, b) => a.position - b.position);
  const idByKey = new Map<string, string>();
  const rows: NodeRow[] = sorted.map((n) => {
    const rowId = newRowId();
    idByKey.set(n.key, rowId);
    return {
      rowId,
      key: n.key,
      keyTouched: true,
      title: n.title,
      agent_id: n.agent_id,
      prompt: n.prompt,
      requires_review: n.requires_review,
      max_attempts: n.max_attempts,
      depends_on: [],
    };
  });
  rows.forEach((row, i) => {
    row.depends_on = sorted[i]!.depends_on.map((key) => idByKey.get(key) ?? `${MISSING_PREFIX}${key}`);
  });
  return rows;
}

export function rowsToInput(rows: readonly NodeRow[]): ExtWorkflowNodeInput[] {
  const keyById = new Map(rows.map((r) => [r.rowId, r.key]));
  const keyOf = (id: string) =>
    keyById.get(id) ?? (id.startsWith(MISSING_PREFIX) ? id.slice(MISSING_PREFIX.length) : "");
  return rows.map((r) => ({
    key: r.key,
    title: r.title.trim(),
    agent_id: r.agent_id,
    prompt: r.prompt,
    requires_review: r.requires_review,
    max_attempts: r.max_attempts,
    depends_on: r.depends_on.map(keyOf),
  }));
}

export function serializeRows(rows: readonly NodeRow[]): string {
  return JSON.stringify(rowsToInput(rows));
}

// ---------------------------------------------------------------- DAG layout

export const DAG_NODE_W = 168;
export const DAG_NODE_H = 40;
export const DAG_COL_GAP = 56;
export const DAG_ROW_GAP = 14;
export const DAG_PAD = 12;

/** Longest-path depth per row id. Dependencies on missing rows are ignored; cycles do not loop. */
export function computeDepths(
  rows: readonly Pick<NodeRow, "rowId" | "depends_on">[],
): Map<string, number> {
  const byId = new Map(rows.map((r) => [r.rowId, r]));
  const memo = new Map<string, number>();
  const visiting = new Set<string>();
  const depth = (id: string): number => {
    const known = memo.get(id);
    if (known !== undefined) return known;
    if (visiting.has(id)) return 0;
    visiting.add(id);
    let d = 0;
    for (const dep of byId.get(id)?.depends_on ?? []) {
      if (dep !== id && byId.has(dep)) d = Math.max(d, depth(dep) + 1);
    }
    visiting.delete(id);
    memo.set(id, d);
    return d;
  };
  for (const r of rows) depth(r.rowId);
  return memo;
}

export interface DagNode {
  rowId: string;
  label: string;
  x: number;
  y: number;
  w: number;
  h: number;
}
export interface DagEdge {
  from: string;
  to: string;
  d: string;
}
export interface DagLayout {
  nodes: DagNode[];
  edges: DagEdge[];
  width: number;
  height: number;
}

export function layoutDag(
  rows: readonly Pick<NodeRow, "rowId" | "key" | "title" | "depends_on">[],
): DagLayout {
  if (rows.length === 0) return { nodes: [], edges: [], width: 0, height: 0 };
  const depths = computeDepths(rows);
  const perColumn = new Map<number, number>();
  const nodes: DagNode[] = rows.map((r) => {
    const col = depths.get(r.rowId) ?? 0;
    const slot = perColumn.get(col) ?? 0;
    perColumn.set(col, slot + 1);
    return {
      rowId: r.rowId,
      label: r.title.trim() || r.key,
      x: DAG_PAD + col * (DAG_NODE_W + DAG_COL_GAP),
      y: DAG_PAD + slot * (DAG_NODE_H + DAG_ROW_GAP),
      w: DAG_NODE_W,
      h: DAG_NODE_H,
    };
  });
  const byId = new Map(nodes.map((n) => [n.rowId, n]));
  const edges: DagEdge[] = [];
  for (const r of rows) {
    const to = byId.get(r.rowId)!;
    for (const dep of r.depends_on) {
      const from = byId.get(dep);
      if (!from || dep === r.rowId) continue;
      const x1 = from.x + from.w;
      const y1 = from.y + from.h / 2;
      const x2 = to.x;
      const y2 = to.y + to.h / 2;
      const mid = DAG_COL_GAP / 2;
      edges.push({
        from: dep,
        to: r.rowId,
        d: `M ${x1} ${y1} C ${x1 + mid} ${y1}, ${x2 - mid} ${y2}, ${x2} ${y2}`,
      });
    }
  }
  const columns = Math.max(...depths.values()) + 1;
  const tallest = Math.max(...perColumn.values());
  return {
    nodes,
    edges,
    width: DAG_PAD * 2 + columns * DAG_NODE_W + (columns - 1) * DAG_COL_GAP,
    height: DAG_PAD * 2 + tallest * DAG_NODE_H + (tallest - 1) * DAG_ROW_GAP,
  };
}
