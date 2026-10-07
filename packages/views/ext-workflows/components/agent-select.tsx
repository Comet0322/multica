"use client";

import { useMemo, useState } from "react";
import { ChevronDown, UserPlus } from "lucide-react";
import { useAuthStore } from "@multica/core/auth";
import type { Agent } from "@multica/core/types";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { cn } from "@multica/ui/lib/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";
import { useT } from "../../i18n";
import {
  PickerEmpty,
  PickerItem,
  PickerSection,
} from "../../issues/components/pickers/property-picker";

interface AgentSelectProps {
  /** Every agent, archived included, so an archived current value can still show its name. */
  agents: Agent[];
  value: string;
  onChange: (agentId: string) => void;
  /** Accessible name of the trigger (the visible label lives outside). */
  ariaLabel: string;
  placeholder: string;
  /** Supervisors need a bound runtime; node agents only need to be non-archived. */
  requireRuntime?: boolean;
  disabled?: boolean;
  invalid?: boolean;
  className?: string;
}

export function AgentSelect({
  agents,
  value,
  onChange,
  ariaLabel,
  placeholder,
  requireRuntime = false,
  disabled = false,
  invalid = false,
  className,
}: AgentSelectProps) {
  const { t } = useT("ext-workflows");
  const currentUserId = useAuthStore((s) => s.user?.id ?? null);
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");

  const selectable = useMemo(
    () => agents.filter((a) => !a.archived_at && (!requireRuntime || !!a.runtime_id)),
    [agents, requireRuntime],
  );
  const q = filter.trim().toLowerCase();
  const matches = (a: Agent) =>
    !q || a.name.toLowerCase().includes(q) || matchesPinyin(a.name, q);
  const mine = selectable.filter((a) => currentUserId && a.owner_id === currentUserId).filter(matches);
  const others = selectable.filter((a) => !currentUserId || a.owner_id !== currentUserId).filter(matches);

  const selected = agents.find((a) => a.id === value) ?? null;
  const selectedLabel = selected
    ? selected.archived_at
      ? `${selected.name} ${t(($) => $.agent_select.archived_suffix)}`
      : selected.name
    : null;

  const pick = (id: string) => {
    onChange(id);
    setOpen(false);
    setFilter("");
  };

  const rows = (list: Agent[]) =>
    list.map((a) => (
      <PickerItem key={a.id} selected={value === a.id} onClick={() => pick(a.id)}>
        <ActorAvatar actorType="agent" actorId={a.id} size="sm" showStatusDot />
        <span className="truncate">{a.name}</span>
      </PickerItem>
    ));

  if (selectable.length === 0 && !selected) {
    return (
      <div
        className={cn(
          "rounded-lg border border-dashed bg-muted/30 px-3 py-2 text-body text-muted-foreground",
          className,
        )}
      >
        {t(($) => $.agent_select.no_agents)}
      </div>
    );
  }

  return (
    <Popover
      open={open}
      onOpenChange={(v) => {
        if (disabled) return;
        setOpen(v);
        if (!v) setFilter("");
      }}
    >
      <PopoverTrigger
        disabled={disabled}
        aria-label={ariaLabel}
        aria-invalid={invalid || undefined}
        className={cn(
          "flex w-full min-w-0 items-center gap-2 rounded-lg border bg-background px-2.5 py-1.5 text-left text-body transition-colors hover:bg-muted disabled:cursor-not-allowed disabled:opacity-60",
          invalid ? "border-destructive" : "border-border",
          className,
        )}
      >
        {selected ? (
          <ActorAvatar actorType="agent" actorId={selected.id} size="sm" showStatusDot />
        ) : (
          <UserPlus className="size-4 shrink-0 text-muted-foreground" />
        )}
        <span className={cn("min-w-0 flex-1 truncate", !selected && "text-muted-foreground")}>
          {selectedLabel ?? placeholder}
        </span>
        <ChevronDown
          className={cn("size-4 shrink-0 text-muted-foreground transition-transform", open && "rotate-180")}
        />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--anchor-width)] min-w-56 p-0">
        <div className="border-b px-2 py-1.5">
          <input
            autoFocus
            type="text"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder={t(($) => $.agent_select.search_placeholder)}
            className="w-full bg-transparent text-body outline-none placeholder:text-muted-foreground"
          />
        </div>
        <div className="max-h-72 overflow-y-auto p-1">
          {mine.length > 0 && (
            <PickerSection label={t(($) => $.agent_select.group_my_agents)}>{rows(mine)}</PickerSection>
          )}
          {others.length > 0 && (
            <PickerSection label={t(($) => $.agent_select.group_workspace_agents)}>
              {rows(others)}
            </PickerSection>
          )}
          {mine.length === 0 && others.length === 0 && <PickerEmpty />}
        </div>
      </PopoverContent>
    </Popover>
  );
}
