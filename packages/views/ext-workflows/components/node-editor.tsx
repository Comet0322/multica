"use client";

import { useState } from "react";
import { ArrowDown, ArrowUp, ChevronDown, ChevronRight, Plus, Trash2 } from "lucide-react";
import type { Agent } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Switch } from "@multica/ui/components/ui/switch";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { PickerItem } from "../../issues/components/pickers/property-picker";
import {
  createRow,
  dependencyOptions,
  moveRow,
  patchRow,
  removeRow,
  setRowKey,
  setRowTitle,
  type GeneralErrorCode,
  type MappedServerErrors,
  type NodeField,
  type NodeRow,
  type RowErrorCode,
  type RowErrors,
} from "../node-editor-helpers";
import { AgentSelect } from "./agent-select";
import { DagPreview } from "./dag-preview";

export interface NodeEditorProps {
  rows: NodeRow[];
  onChange: (rows: NodeRow[]) => void;
  agents: Agent[];
  /** Client validation, passed only once the user has tried to save. */
  validation: { rows: RowErrors; general: GeneralErrorCode[] } | null;
  serverErrors: MappedServerErrors | null;
  /** Runs in progress keep using the previous definition. */
  activeRunCount: number;
  readOnly?: boolean;
}

const labelOf = (r: Pick<NodeRow, "title" | "key">) => r.title.trim() || r.key;

export function NodeEditor({
  rows,
  onChange,
  agents,
  validation,
  serverErrors,
  activeRunCount,
  readOnly = false,
}: NodeEditorProps) {
  const { t } = useT("ext-workflows");
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const toggleExpanded = (rowId: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(rowId)) next.delete(rowId);
      else next.add(rowId);
      return next;
    });

  const messagesFor = (row: NodeRow): Partial<Record<NodeField, string>> => {
    const out: Partial<Record<NodeField, string>> = {};
    const client = validation?.rows[row.rowId];
    if (client) {
      for (const [field, code] of Object.entries(client)) {
        out[field as NodeField] = t(($) => $.errors[code as RowErrorCode]);
      }
    }
    const server = serverErrors?.byRow[row.rowId];
    if (server) {
      for (const [field, message] of Object.entries(server)) {
        out[field as NodeField] ??= message;
      }
    }
    return out;
  };

  const general = [
    ...(validation?.general.map((code) => t(($) => $.errors[code])) ?? []),
    ...(serverErrors?.general ?? []),
  ];

  return (
    <div className="space-y-4">
      {activeRunCount > 0 && (
        <p className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-caption">
          {t(($) => $.nodes.active_runs_note, { count: activeRunCount })}
        </p>
      )}

      {general.length > 0 && (
        <ul role="alert" className="space-y-1 text-caption text-destructive">
          {general.map((message, i) => (
            <li key={`${i}-${message}`}>{message}</li>
          ))}
        </ul>
      )}

      {rows.length > 0 && <DagPreview rows={rows} />}

      {rows.length === 0 ? (
        <p className="rounded-lg border border-dashed px-4 py-6 text-center text-body text-muted-foreground">
          {t(($) => $.nodes.empty)}
        </p>
      ) : (
        <ol className="space-y-2">
          {rows.map((row, index) => (
            <NodeRowEditor
              key={row.rowId}
              row={row}
              index={index}
              total={rows.length}
              rows={rows}
              agents={agents}
              messages={messagesFor(row)}
              isExpanded={expanded.has(row.rowId) || !!messagesFor(row).prompt}
              readOnly={readOnly}
              onToggleExpanded={() => toggleExpanded(row.rowId)}
              onTitle={(title) => onChange(setRowTitle(rows, row.rowId, title))}
              onKey={(key) => onChange(setRowKey(rows, row.rowId, key))}
              onPatch={(patch) => onChange(patchRow(rows, row.rowId, patch))}
              onMove={(delta) => onChange(moveRow(rows, index, delta))}
              onDelete={() => onChange(removeRow(rows, row.rowId))}
            />
          ))}
        </ol>
      )}

      {!readOnly && (
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={() => onChange([...rows, createRow(rows)])}
        >
          <Plus className="size-3.5" />
          {t(($) => $.nodes.add)}
        </Button>
      )}
    </div>
  );
}

function NodeRowEditor({
  row,
  index,
  total,
  rows,
  agents,
  messages,
  isExpanded,
  readOnly,
  onToggleExpanded,
  onTitle,
  onKey,
  onPatch,
  onMove,
  onDelete,
}: {
  row: NodeRow;
  index: number;
  total: number;
  rows: NodeRow[];
  agents: Agent[];
  messages: Partial<Record<NodeField, string>>;
  isExpanded: boolean;
  readOnly: boolean;
  onToggleExpanded: () => void;
  onTitle: (title: string) => void;
  onKey: (key: string) => void;
  onPatch: (patch: Partial<NodeRow>) => void;
  onMove: (delta: -1 | 1) => void;
  onDelete: () => void;
}) {
  const { t } = useT("ext-workflows");
  const archivedAgent = !!agents.find((a) => a.id === row.agent_id)?.archived_at;
  const messageList = Object.values(messages);
  const hasError = (field: NodeField) => !!messages[field];

  return (
    <li aria-label={t(($) => $.nodes.node_n, { n: index + 1 })} className="space-y-2 rounded-lg border bg-background p-3">
      <div className="flex items-center gap-2">
        <span className="w-5 shrink-0 text-center text-micro tabular-nums text-muted-foreground">{index + 1}</span>
        <Input
          aria-label={t(($) => $.nodes.title_label)}
          aria-invalid={hasError("title") || undefined}
          value={row.title}
          disabled={readOnly}
          placeholder={t(($) => $.nodes.title_placeholder)}
          onChange={(e) => onTitle(e.target.value)}
          className="min-w-0 flex-1"
        />
        <Input
          aria-label={t(($) => $.nodes.key_label)}
          aria-invalid={hasError("key") || undefined}
          value={row.key}
          disabled={readOnly}
          onChange={(e) => onKey(e.target.value)}
          className="w-36 shrink-0 font-mono text-caption"
        />
        {!readOnly && (
          <div className="flex shrink-0 items-center">
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.move_up)}
              disabled={index === 0}
              onClick={() => onMove(-1)}
            >
              <ArrowUp />
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.move_down)}
              disabled={index === total - 1}
              onClick={() => onMove(1)}
            >
              <ArrowDown />
            </Button>
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              aria-label={t(($) => $.nodes.delete)}
              onClick={onDelete}
            >
              <Trash2 />
            </Button>
          </div>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2 pl-7">
        <AgentSelect
          agents={agents}
          value={row.agent_id}
          onChange={(agent_id) => onPatch({ agent_id })}
          ariaLabel={t(($) => $.nodes.agent_label)}
          placeholder={t(($) => $.nodes.agent_placeholder)}
          disabled={readOnly}
          invalid={hasError("agent_id") || archivedAgent}
          className="w-52"
        />
        <DependsPicker row={row} rows={rows} readOnly={readOnly} invalid={hasError("depends_on")} onChange={(depends_on) => onPatch({ depends_on })} />
        <label className="flex items-center gap-1.5 text-caption">
          <Switch
            size="sm"
            checked={row.requires_review}
            disabled={readOnly}
            onCheckedChange={(requires_review) => onPatch({ requires_review })}
          />
          {t(($) => $.nodes.review_label)}
        </label>
        <label className="flex items-center gap-1.5 text-caption text-muted-foreground">
          {t(($) => $.nodes.max_attempts_label)}
          <Input
            type="number"
            min={1}
            max={10}
            // A cleared input parses to NaN; keep it in the row for validation
            // but never hand NaN to the controlled input.
            value={Number.isNaN(row.max_attempts) ? "" : row.max_attempts}
            disabled={readOnly}
            aria-invalid={hasError("max_attempts") || undefined}
            onChange={(e) => onPatch({ max_attempts: e.target.valueAsNumber })}
            className="w-16"
          />
        </label>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          aria-expanded={isExpanded}
          aria-label={isExpanded ? t(($) => $.nodes.collapse) : t(($) => $.nodes.expand)}
          onClick={onToggleExpanded}
        >
          {isExpanded ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
          {t(($) => $.nodes.prompt_label)}
        </Button>
      </div>

      {isExpanded && (
        <div className="pl-7">
          <Textarea
            aria-label={t(($) => $.nodes.prompt_label)}
            aria-invalid={hasError("prompt") || undefined}
            value={row.prompt}
            disabled={readOnly}
            placeholder={t(($) => $.nodes.prompt_placeholder)}
            rows={6}
            onChange={(e) => onPatch({ prompt: e.target.value })}
          />
        </div>
      )}

      {(messageList.length > 0 || archivedAgent) && (
        <ul className="space-y-0.5 pl-7 text-caption text-destructive">
          {archivedAgent && <li>{t(($) => $.nodes.archived_agent)}</li>}
          {messageList.map((m) => (
            <li key={m}>{m}</li>
          ))}
        </ul>
      )}
    </li>
  );
}

function DependsPicker({
  row,
  rows,
  readOnly,
  invalid,
  onChange,
}: {
  row: NodeRow;
  rows: NodeRow[];
  readOnly: boolean;
  invalid: boolean;
  onChange: (dependsOn: string[]) => void;
}) {
  const { t } = useT("ext-workflows");
  const [open, setOpen] = useState(false);
  // Only choices that cannot close a cycle are offered.
  const options = dependencyOptions(rows, row.rowId);
  const selected = row.depends_on
    .map((id) => rows.find((r) => r.rowId === id))
    .filter((r): r is NodeRow => !!r);
  const toggle = (id: string) =>
    onChange(row.depends_on.includes(id) ? row.depends_on.filter((d) => d !== id) : [...row.depends_on, id]);

  return (
    <Popover open={open} onOpenChange={readOnly ? undefined : setOpen}>
      <PopoverTrigger
        disabled={readOnly}
        aria-label={t(($) => $.nodes.depends_label)}
        className={cn(
          "flex max-w-64 min-w-32 items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-left text-caption hover:bg-muted disabled:opacity-60",
          invalid ? "border-destructive" : "border-border",
        )}
      >
        <span className="shrink-0 text-muted-foreground">{t(($) => $.nodes.depends_label)}:</span>
        <span className="min-w-0 truncate">
          {selected.length === 0 ? t(($) => $.nodes.depends_none) : selected.map(labelOf).join(", ")}
        </span>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-1">
        {options.length === 0 ? (
          <p className="px-2 py-1.5 text-caption text-muted-foreground">{t(($) => $.nodes.depends_no_options)}</p>
        ) : (
          options.map((option) => (
            <PickerItem key={option.rowId} selected={row.depends_on.includes(option.rowId)} onClick={() => toggle(option.rowId)}>
              <span className="truncate">{labelOf(option)}</span>
            </PickerItem>
          ))
        )}
      </PopoverContent>
    </Popover>
  );
}
