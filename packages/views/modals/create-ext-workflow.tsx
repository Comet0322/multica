"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useCreateExtWorkflow } from "@multica/core/ext-workflows";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { agentListOptions } from "@multica/core/workspace/queries";
import { isImeComposing } from "@multica/core/utils";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { AgentSelect } from "../ext-workflows/components/agent-select";
import { useT } from "../i18n";
import { useNavigation } from "../navigation";

export function CreateExtWorkflowModal({ onClose }: { onClose: () => void }) {
  const { t } = useT("ext-workflows");
  const router = useNavigation();
  const wsPaths = useWorkspacePaths();
  const wsId = useWorkspaceId();
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const create = useCreateExtWorkflow(wsId);

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [supervisorId, setSupervisorId] = useState("");

  const canSubmit = !!name.trim() && !!supervisorId && !create.isPending;

  const submit = async () => {
    if (!canSubmit) return;
    try {
      const workflow = await create.mutateAsync({
        name: name.trim(),
        description: description.trim(),
        supervisor_agent_id: supervisorId,
      });
      onClose();
      toast.success(t(($) => $.create.toast_created));
      // The detail page opens on the Nodes tab, where a new workflow starts.
      router.push(wsPaths.workflowDetail(workflow.id));
    } catch (err) {
      toast.error(err instanceof Error && err.message ? err.message : t(($) => $.create.toast_failed));
    }
  };

  return (
    <Dialog open onOpenChange={(v) => { if (!v) onClose(); }}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.create.title)}</DialogTitle>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-name" className="text-caption text-muted-foreground">
              {t(($) => $.create.name_label)}
            </Label>
            <Input
              id="ext-workflow-name"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t(($) => $.create.name_placeholder)}
              onKeyDown={(e) => {
                if (isImeComposing(e)) return;
                if (e.key === "Enter") void submit();
              }}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="ext-workflow-description" className="text-caption text-muted-foreground">
              {t(($) => $.create.description_label)}
            </Label>
            <Textarea
              id="ext-workflow-description"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder={t(($) => $.create.description_placeholder)}
              rows={2}
            />
          </div>
          <div className="space-y-1.5">
            <div className="text-caption font-medium text-muted-foreground">
              {t(($) => $.create.supervisor_label)}
            </div>
            <p className="text-caption text-muted-foreground">{t(($) => $.create.supervisor_hint)}</p>
            <AgentSelect
              agents={agents}
              value={supervisorId}
              onChange={setSupervisorId}
              ariaLabel={t(($) => $.create.supervisor_label)}
              placeholder={t(($) => $.create.supervisor_placeholder)}
              requireRuntime
            />
          </div>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            {t(($) => $.create.cancel)}
          </Button>
          <Button type="button" onClick={() => void submit()} disabled={!canSubmit} aria-busy={create.isPending}>
            {create.isPending ? t(($) => $.create.submitting) : t(($) => $.create.submit)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
