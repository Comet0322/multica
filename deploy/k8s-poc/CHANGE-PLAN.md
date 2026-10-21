# Change plan — managed runtime on Kubernetes (from the v0.6.1 PoC)

Goal: a company-wide platform where agents run in autoscaled Kubernetes Sandboxes, ordinary users never see how the
runtime is built, and platform admins get a separate permissioned page. Evidence: `REPORT.md` (findings F1–F8).
Items marked **(confirm)** were not verified in code; check before implementing.

## 0. Decisions this plan assumes

| # | Decision | Why |
|---|---|---|
| D1 | **One Sandbox (+PVC) per managed agent**, with its own `daemon_id`. | `ClaimAgentTask` requires `agent.runtime_id = task.runtime_id`, so an agent has exactly one runtime. Per-project Sandboxes would need enqueue-time routing and claim SQL changes. PoC T4 proved this shape: suspend/resume keeps workdir and session. F3 rules out replicas sharing a `daemon_id`. |
| D2 | Daemon credential = existing **`mdt_` daemon token**, bound to `(workspace_id, daemon_id)`. | The table, queries, cache, revocation, and `register` / batch-claim handling already exist. Missing: issuance and enforcement. |
| D3 | Controller is a **separate process** that talks to the server over an internal API; it does not read the DB. | Keeps the v0.6.1 control plane unchanged and the controller replaceable. |
| D4 | "Admin" for the hidden-runtime page means **platform operators**, not workspace admins. | Workspace admins are users of the platform and must not see infrastructure. |
| D5 | v1 continuity is **per issue, per agent** (what T4 tested). | Multica's workdir is keyed by issue. Cross-issue "project" context does not exist in v0.6.1; it is a product feature, not infra. |

## 1. Workstream A — Daemon API authorization (fixes F1; prerequisite for everything)

**Problem.** `requireDaemonRuntimeAccess` and `requireDaemonTaskAccessWithWorkspace` (`server/internal/handler/daemon.go`)
end in `requireDaemonWorkspaceAccess`, which checks workspace membership only. The `mdt_` branch compares workspace but
never `daemon_id`.

**Change.** One function, `authorizeDaemonRuntime(w, r, rt)`, called from both helpers so every per-runtime and per-task route inherits it:

| Credential | Rule |
|---|---|
| `mdt_` | `rt.workspace_id == token.workspace_id` **and** `rt.daemon_id == token.daemon_id` |
| `mul_` / JWT / `mcn_` on a **managed** runtime | deny |
| `mul_` / JWT / `mcn_` otherwise | workspace member **and** `rt.owner_id == requester` (a NULL owner is denied) |

Call sites that do not go through those two helpers and need the same rule (verified):
- `DaemonHeartbeat` (`daemon.go:1150`) calls `requireDaemonWorkspaceAccess` directly.
- `DaemonDeregister` (`daemon.go:992`) and the batch claim `ClaimTasksByRuntime` (`daemon.go:1808`) use `verifyDaemonWorkspaceAccess`, a second workspace-only helper. Consequence beyond claim theft: **any member can deregister (take offline) any runtime in the workspace**. The batch claim's body `daemon_id` is self-declared, so it is not a security check for PAT/JWT.
- The WebSocket identity (`daemon_ws.go`, `buildDaemonWebSocketIdentity`) builds a runtime set per connection; filter it with the same rule.
Already covered once the helpers change: `RecoverOrphanedTasks` (`task_lifecycle.go`), `ClaimTaskByRuntime`, `ResolveTaskSkillBundles`, `ListPendingTasksByRuntime`, `ExtendTaskPrepareLease`, `GetTaskStatus`.
Workspace-level reads (`GetDaemonWorkspaceRepos`, the `*GCCheck` endpoints) stay workspace-scoped.

**Rollout (compatibility).** Installed local daemons authenticate with the owner's PAT; the owner rule keeps them working.
Cases that could break are not knowable from code alone: a daemon run by someone other than `owner_id` (ownership transfer,
admin-run shared daemons). So ship in two steps behind `DAEMON_AUTHZ_MODE=off|log|enforce`:
1. **log**: allow, but emit `multica_daemon_authz_denied_total{credential,reason}` and a structured log. Run one release; review denials.
2. **enforce**: default for managed runtimes immediately (nothing legacy uses them); for local runtimes after the log review.

**Tests** (Go, `testutil.Call`; table-driven): credentials {`mdt_` own / other daemon / other workspace, PAT owner / non-owner member / outsider, JWT, `mcn_`}
× routes {claim, batch claim, heartbeat, deregister, start, complete, fail, messages, recover-orphans, WS connect}. Plus a regression
test for the PoC repro: a plain member claiming a task on another member's runtime must get 404.

## 2. Workstream B — Managed runtime model and token issuance (F4)

1. **Migration**: `agent_runtime.managed BOOLEAN NOT NULL DEFAULT false` (no FK, per repo rules). Regenerate sqlc. **(confirm)** the two
   `UpsertAgentRuntime` statements (`runtime.sql`, `ON CONFLICT … DO UPDATE`) must not reset `managed`; `owner_id` already uses
   `COALESCE(EXCLUDED.owner_id, agent_runtime.owner_id)` — copy that pattern.
2. **Internal API** (separate route group, bearer = `MULTICA_CONTROLLER_SECRET`, constant-time compare, not exposed by ingress):
   - `POST /internal/v1/managed-runtimes` — create/upsert the runtime row (`managed=true`, `visibility=public`, display name, provider, creating user as `owner_id`).
   - `POST /internal/v1/daemon-tokens` — `{workspace_id, daemon_id, ttl_seconds ≤ 86400}` → raw `mdt_` returned once (`CreateDaemonToken`).
   - `DELETE /internal/v1/daemon-tokens` — revoke by `(workspace_id, daemon_id)` using `DeleteDaemonTokensByWorkspaceAndDaemons` + cache invalidate (same code path as `workspace_revoke.go`).
   - `GET /internal/v1/managed-runtimes/state` — per runtime: status, queued task count, active task count, last task finished at. The controller polls it.
3. **Agent creation**: a "Managed" runtime choice creates the agent and the managed runtime together; the agent shows as provisioning until the first register.

## 3. Workstream C — Hiding the runtime from users (F5) and the admin page

**C1 Server-side redaction** in the runtime and task response builders. For `managed` runtimes, ordinary callers get:

| Field | Value |
|---|---|
| `name` | display name only (no host/pod suffix) |
| `daemon_id`, `device_info`, `launch_header` | `null` / `""` |
| `metadata` | `{}` |
| `owner_id` | `null` |
| `status` | **projected**: `online` while the Sandbox is `Running` *or* `Suspended` and resumable; `offline` only when unavailable |
| `managed` | `true` (new optional field) |

Tasks on managed runtimes omit `work_dir` / `relative_work_dir`. The status projection matters: the UI (`derive-presence`, `derive-health`, `isAgentRuntimeBound`) reads
runtime status to mark agents offline; a suspended Sandbox would otherwise make every idle agent look dead. The DB status stays truthful (claim requires `online`).
**(confirm / audit)** other places that publish runtime payloads: WebSocket `runtime:*` events, inbox/activity text, agent responses (`runtime_id` stays; `runtime_mode` is
`cloud`/`local` today, decide what a managed agent reports).

**C2 API compatibility** (repo rule): update `packages/core/api/schema.ts` (`managed` optional, default `false`; `device_info`/`metadata` tolerate empty), add malformed-response tests.

**C3 Web UI** (`packages/views/runtimes/`): for `managed` hide the machine grouping, CLI version, update, rename, delete, connect-remote and profile actions
(`runtime-detail.tsx`, `runtime-list.tsx`, `runtime-machines.ts`, `machine-cli-section.tsx`, `update-section.tsx`, `delete-runtime-dialog.tsx`); show provider, availability, usage. All copy through i18n (en + zh at minimum, per conventions pages).

**C4 Platform admin page**: `PLATFORM_ADMIN_EMAILS` allowlist in v1 (a `platform_admin` flag later) → `RequirePlatformAdmin` middleware →
`/api/admin/managed-runtimes` (list: workspace, agent, Sandbox name, node, state, last active, queued/active tasks) and `/{id}` (detail, token expiry, events), with actions suspend / resume / revoke token / delete.
Web-only page under `apps/web` at `/admin/runtimes` (`admin` is already in `reserved_slugs.json`, so no slug change).

## 4. Workstream D — Daemon managed mode (F2, F4, F6)

| Change | Detail |
|---|---|
| Credential from file | `--token-file` / `MULTICA_DAEMON_TOKEN_FILE`; no `multica login`, no `config.json` write; client re-reads the file when it changes so the controller can rotate tokens. |
| Child environment | Verify and test that the agent subprocess env contains neither the daemon token nor its path. (PoC showed `MULTICA_TOKEN` in the agent env is the task token `mat_`, which is correctly scoped.) |
| Disable local-only features | auto-update, auto-reload, user-CLI discovery/self-heal, disk-usage reporting. |
| Drain on SIGTERM | stop claiming; wait up to `MULTICA_DAEMON_DRAIN_TIMEOUT`; then cancel and report failure with a distinct retryable reason (PoC T5: cancelled tasks were not retried despite `max_attempts=2`). **(confirm)** retry rules in `server/pkg/taskfailure` and the task service. |
| Keep deregister on SIGTERM | With one `daemon_id` per Sandbox, offline-on-suspend and re-register-on-resume is the correct behavior (T4). F3 only matters for the rejected replicas model. |
| Optional, later | run the agent under a different uid than the daemon (`syscall.Credential` in the agent launcher). With A and B in place a stolen `mdt_` can only act on that one runtime, expires ≤ 24 h, and is revocable, so this is defense in depth, not a blocker. |

## 5. Workstream E — Runtime controller (new process)

`server/cmd/runtime-controller` (Go, controller-runtime; leader election). Per managed runtime it owns: a `Sandbox` from a `SandboxTemplate`, its PVC, a token `Secret`.

State machine: `Absent → Running → Suspended → Running … → Deleted`.
- **Create**: on the internal `managed-runtimes` event → mint token → write Secret → create Sandbox (PVC via `volumeClaimTemplates`).
- **Wake (F8)**: queued tasks > 0 and Sandbox `Suspended` → set `operatingMode: Running`. PoC: resume to first task done ≈ 3 s.
- **Suspend**: no active tasks and idle > `IDLE_SUSPEND_AFTER` → `Suspended`. Never suspend with an active task (daemon drain is only the node-eviction safety net).
- **Rotate**: mint a new token at half TTL, update the Secret.
- **Fail closed**: Sandbox stuck > N minutes or crash-looping → mark runtime unavailable, fail queued tasks with a clear reason instead of waiting forever.
- **Delete**: agent archived/deleted → revoke tokens, delete Sandbox; PVC retained `PVC_RETENTION_DAYS` then garbage-collected.
- **Quotas**: per-workspace caps on Sandboxes and PVC GiB, via the existing `entitlement` package. **(confirm)** its API fits.

## 6. Workstream F — Sandbox template and cluster (no Multica code)

`automountServiceAccountToken: false` · `enableServiceLinks: false` · NetworkPolicy allowing only the Multica server and the LLM gateway (PoC showed Postgres and the API server reachable) ·
`runtimeClassName` gVisor or Kata (needs real nodes; kind cannot test it) · non-root, `readOnlyRootFilesystem` where possible, dropped capabilities, seccomp · `fsGroup` for the PVC · resource requests/limits · warm pool only with post-adoption identity (F7): the entrypoint waits for a token file, then starts the daemon — env injection forces a fresh pod.

## 7. Out of scope but blocking for real agents: the LLM credential

A real `claude`/`codex` needs a model API key inside the Sandbox, and the agent (a user-controlled process) can read anything the pod can. The PoC used a fake agent, so this was **not** exercised.
Needed before launch: an LLM gateway that issues short-lived, per-workspace, rate-limited keys (also gives cost accounting), or equivalent. This is its own workstream.

## 8. Delivery order

| PR | Content | Size | Depends on |
|---|---|---|---|
| 1 | A: `authorizeDaemonRuntime`, `DAEMON_AUTHZ_MODE=log`, metrics, tests | S–M | — |
| 2 | B: migration, internal API, token issuance/revoke | M | — |
| 3 | D: daemon managed mode (token file, env check, flags, drain) | M | 2 (token) |
| 4 | A enforce for managed + C1/C2 redaction and status projection | M | 1, 2 |
| 5 | E: controller + Sandbox template manifests (F) | L | 2, 3 |
| 6 | C3/C4: UI hiding and admin page | M | 4 |
| 7 | ~~A enforce for local runtimes~~ — deferred (decision 3) | S | 1 + one release |
| — | LLM gateway | L | independent, gates GA |

PRs 1 and 2 can start now in parallel. Use atomic conventional commits and the repo PR template.

## 9. Acceptance: rerun the PoC against the changes

`setup.sh` and the probes are reusable. Expected flips: member claim on another runtime → 404 (T6/F1); `mdt_` for daemon A on daemon B's runtime → 404 (T8);
member runtime view has no `daemon_id` / `device_info` / `metadata` / paths (T6); no token in `config.json` or agent env (T7); suspended-with-queued-task wakes without human action (F8); kill mid-task → retryable (T5).

## 10. Decisions (resolved)

1. Platform admin: env email allowlist (`PLATFORM_ADMIN_EMAILS`). A DB flag / SSO group can replace it later behind the same helper.
2. Granularity: one Sandbox per agent (D1) — no objection raised; treated as accepted.
3. Fix **managed** runtimes only for now. PR 7 (owner rule on local runtimes) is deferred; F1 stays open for local runtimes and must be listed as a known gap in PR 4.
4. Controller lives in this repo: `server/cmd/runtime-controller`.
5. LLM: the company runs LiteLLM, and all pods share **one** key. Consequences below.
6. Work happens on branches of the user's fork (`origin` = Comet0322/multica); nothing is sent to `multica-ai/multica`.

### Consequence of a single shared LiteLLM key (decision 5)

The agent process can read anything in its pod, so the shared key is readable by any user's agent: it can be exfiltrated and used outside the cluster, and one workspace's usage cannot be told apart or limited from another's.
Cheap mitigations, in order of value:
- Restrict the key in LiteLLM: model allowlist, budget, rate limit, and (if supported) source-IP allowlist to the cluster egress.
- NetworkPolicy so pods reach LiteLLM only, plus the Multica server.
- Put the key in an env var of the agent process only (not in a file, not in the daemon's `/proc` environ visible to the agent uid).
- Later: LiteLLM virtual keys minted per Sandbox by the controller (key per workspace/agent with budget) — a controller change, not a Multica server change.
