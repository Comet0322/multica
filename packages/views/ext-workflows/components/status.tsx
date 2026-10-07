"use client";

import {
  Ban,
  CheckCircle2,
  Circle,
  Eye,
  HelpCircle,
  Loader2,
  SkipForward,
  UserRound,
  XCircle,
} from "lucide-react";
import { Badge } from "@multica/ui/components/ui/badge";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { runStatusKey, stepStatusKey, type RunStatusKey, type StepStatusKey } from "../status-keys";

const STEP_ICON: Record<StepStatusKey, { Icon: typeof Circle; className: string }> = {
  pending: { Icon: Circle, className: "text-muted-foreground" },
  running: { Icon: Loader2, className: "text-brand animate-spin" },
  awaiting_supervisor: { Icon: Eye, className: "text-warning" },
  awaiting_human: { Icon: UserRound, className: "text-warning" },
  done: { Icon: CheckCircle2, className: "text-success" },
  skipped: { Icon: SkipForward, className: "text-muted-foreground" },
  failed: { Icon: XCircle, className: "text-destructive" },
  cancelled: { Icon: Ban, className: "text-muted-foreground" },
  unknown: { Icon: HelpCircle, className: "text-muted-foreground" },
};

export function StepStatusIcon({ status, className }: { status: string; className?: string }) {
  const { t } = useT("ext-workflows");
  const key = stepStatusKey(status);
  const { Icon, className: tone } = STEP_ICON[key];
  return (
    <Icon
      role="img"
      aria-label={t(($) => $.step_status[key])}
      className={cn("size-3.5 shrink-0", tone, className)}
    />
  );
}

const RUN_VARIANT: Record<RunStatusKey, { variant: "secondary" | "destructive" | "outline"; className?: string }> = {
  running: { variant: "secondary", className: "text-brand" },
  waiting_human: { variant: "secondary", className: "text-warning" },
  done: { variant: "secondary", className: "text-success" },
  failed: { variant: "destructive" },
  cancelled: { variant: "outline", className: "text-muted-foreground" },
  unknown: { variant: "outline", className: "text-muted-foreground" },
};

export function RunStatusBadge({ status, className }: { status: string; className?: string }) {
  const { t } = useT("ext-workflows");
  const key = runStatusKey(status);
  const { variant, className: tone } = RUN_VARIANT[key];
  return (
    <Badge variant={variant} className={cn(tone, className)}>
      {t(($) => $.run_status[key])}
    </Badge>
  );
}

export function useStepStatusLabel(): (status: string) => string {
  const { t } = useT("ext-workflows");
  return (status) => {
    const key = stepStatusKey(status);
    return t(($) => $.step_status[key]);
  };
}
