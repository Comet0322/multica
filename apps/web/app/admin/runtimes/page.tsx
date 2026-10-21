"use client";

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import type { PlatformManagedRuntime } from "@multica/core/types";
import { useT } from "@multica/views/i18n";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@multica/ui/components/ui/table";

// Platform-operator page. The only place a managed runtime's implementation
// (daemon id, sandbox state) is shown; ordinary members never see it. The
// server answers 404 to anyone outside PLATFORM_ADMIN_EMAILS, and this page
// renders a plain not-found for them as well.

const adminKeys = {
  status: ["platform-admin", "status"] as const,
  runtimes: ["platform-admin", "managed-runtimes"] as const,
};

const EMPTY_FORM = {
  workspace_id: "",
  daemon_id: "",
  provider: "claude",
  display_name: "",
  owner_id: "",
};

export default function AdminRuntimesPage() {
  const { t } = useT("platform-admin");
  const qc = useQueryClient();
  const status = useQuery({
    queryKey: adminKeys.status,
    queryFn: () => api.getPlatformAdminStatus(),
  });
  const isAdmin = status.data?.is_admin === true;
  const list = useQuery({
    queryKey: adminKeys.runtimes,
    queryFn: () => api.listPlatformManagedRuntimes(),
    enabled: isAdmin,
    refetchInterval: 10_000,
  });

  const [createOpen, setCreateOpen] = useState(false);
  const [form, setForm] = useState(EMPTY_FORM);
  const [deleting, setDeleting] = useState<PlatformManagedRuntime | null>(null);

  const refresh = () => qc.invalidateQueries({ queryKey: adminKeys.runtimes });
  const fail = () => toast.error(t(($) => $.toast.error));

  const create = useMutation({
    mutationFn: () => api.createPlatformManagedRuntime(form),
    onSuccess: () => {
      toast.success(t(($) => $.toast.created));
      setCreateOpen(false);
      setForm(EMPTY_FORM);
      void refresh();
    },
    onError: fail,
  });
  const revoke = useMutation({
    mutationFn: (id: string) => api.revokePlatformManagedRuntimeTokens(id),
    onSuccess: (res) =>
      toast.success(t(($) => $.toast.revoked, { count: res.revoked })),
    onError: fail,
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.deletePlatformManagedRuntime(id),
    onSuccess: () => {
      toast.success(t(($) => $.toast.deleted));
      setDeleting(null);
      void refresh();
    },
    onError: fail,
  });

  if (status.isLoading) return null;
  if (!isAdmin) {
    return (
      <main className="flex min-h-screen items-center justify-center text-muted-foreground">
        {t(($) => $.not_found)}
      </main>
    );
  }

  const rows = list.data ?? [];
  const canCreate = Object.values(form).every((v) => v.trim() !== "");

  return (
    <main className="mx-auto max-w-7xl space-y-4 p-6">
      <header className="flex items-center justify-between">
        <h1 className="text-title font-semibold">{t(($) => $.title)}</h1>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={() => void refresh()}>
            {t(($) => $.refresh)}
          </Button>
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            {t(($) => $.new)}
          </Button>
        </div>
      </header>

      {rows.length === 0 ? (
        <p className="py-12 text-center text-muted-foreground">{t(($) => $.empty)}</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t(($) => $.col.name)}</TableHead>
              <TableHead>{t(($) => $.col.workspace)}</TableHead>
              <TableHead>{t(($) => $.col.daemon)}</TableHead>
              <TableHead>{t(($) => $.col.status)}</TableHead>
              <TableHead>{t(($) => $.col.sandbox)}</TableHead>
              <TableHead>{t(($) => $.col.queued)}</TableHead>
              <TableHead>{t(($) => $.col.active)}</TableHead>
              <TableHead>{t(($) => $.col.agents)}</TableHead>
              <TableHead>{t(($) => $.col.seen)}</TableHead>
              <TableHead>{t(($) => $.col.actions)}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((rt) => (
              <TableRow key={rt.id}>
                <TableCell>{rt.display_name}</TableCell>
                <TableCell>{rt.workspace_name}</TableCell>
                <TableCell className="font-mono text-caption">{rt.daemon_id}</TableCell>
                <TableCell>
                  <Badge variant={rt.status === "online" ? "default" : "secondary"}>
                    {rt.status}
                  </Badge>
                </TableCell>
                <TableCell>{rt.managed_state || "-"}</TableCell>
                <TableCell>{rt.queued_tasks}</TableCell>
                <TableCell>{rt.active_tasks}</TableCell>
                <TableCell>{rt.agent_count}</TableCell>
                <TableCell>
                  {rt.last_seen_at
                    ? new Date(rt.last_seen_at).toLocaleString()
                    : t(($) => $.never)}
                </TableCell>
                <TableCell className="space-x-2 whitespace-nowrap">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={revoke.isPending}
                    onClick={() => revoke.mutate(rt.id)}
                  >
                    {t(($) => $.revoke)}
                  </Button>
                  <Button variant="outline" size="sm" onClick={() => setDeleting(rt)}>
                    {t(($) => $.delete)}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(($) => $.new)}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            {(
              [
                ["workspace_id", t(($) => $.form.workspace_id)],
                ["daemon_id", t(($) => $.form.daemon_id)],
                ["provider", t(($) => $.form.provider)],
                ["display_name", t(($) => $.form.display_name)],
                ["owner_id", t(($) => $.form.owner_id)],
              ] as const
            ).map(([key, label]) => (
              <label key={key} className="block space-y-1 text-body">
                <span>{label}</span>
                <Input
                  value={form[key]}
                  onChange={(e) => setForm((f) => ({ ...f, [key]: e.target.value }))}
                />
              </label>
            ))}
          </div>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>
              {t(($) => $.form.cancel)}
            </DialogClose>
            <Button disabled={!canCreate || create.isPending} onClick={() => create.mutate()}>
              {t(($) => $.form.create)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={(open) => !open && setDeleting(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t(($) => $.delete)}</DialogTitle>
          </DialogHeader>
          <p className="text-body">
            {t(($) => $.confirm_delete, { name: deleting?.display_name ?? "" })}
          </p>
          <DialogFooter>
            <DialogClose render={<Button variant="outline" />}>
              {t(($) => $.form.cancel)}
            </DialogClose>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => deleting && remove.mutate(deleting.id)}
            >
              {t(($) => $.delete)}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </main>
  );
}
