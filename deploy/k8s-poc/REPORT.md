# Managed runtime on Kubernetes — PoC report (Multica v0.6.1)

Branch `test/managed-runtime-v0.6.1`, base tag `v0.6.1`. No `server/` business code was changed; the only
server-side addition is one test file (`server/internal/service/runtime_claim_concurrency_test.go`).

## Verdict

**Not a rewrite. Small-to-medium changes, with one server-side authorization fix as a hard prerequisite.**

- The control plane (server, DB, scheduling, claim semantics) works unchanged with daemons running as
  Kubernetes `Sandbox` objects (kubernetes-sigs/agent-sandbox v1.0.7): register, heartbeat, claim, run, resume.
- Project-scoped continuity works: suspend a Sandbox, resume it, and the next task reuses the same workdir and session.
- What blocks "company-wide, runtime hidden" is not Kubernetes. It is how v0.6.1 treats the daemon
  credential and the daemon API (findings F1–F3). Those need server and daemon changes, not a new backend.

## Setup

kind (1 node) · Agent Sandbox v1.0.7 · server + Postgres from the v0.6.1 `Dockerfile` · daemon image = v0.6.1
`multica` + a fake `claude` CLI (`fake-claude`, no LLM) · isolated Postgres for Go tests (the developer's own DB was not used).
Rebuild from scratch with `deploy/k8s-poc/setup.sh` (62 s; verified by a clean rebuild plus a 20-task smoke run).

## Results

| # | Item | Result | Evidence |
|---|---|---|---|
| T0 | Baseline | **PASS** | Image builds. `go test` on daemon, handler, service, middleware: 7659 passed. |
| T1 | Claim concurrency | **PASS** | 50 tasks × 16 concurrent claimers × 15 runs under `-race`: no double claim, none left queued; `max_concurrent_tasks=5` respected. |
| T2 | Daemon inside a Sandbox | **PASS** | Registered as a runtime, heartbeat steady, 21/21 tasks `completed`. Needs a login wrapper (F4). |
| T3 | On-demand start / warm pool | **PASS, with a design constraint** | Cold Sandbox: Ready 0.98 s, runtime online 0.99 s (images preloaded, lower bound). `SandboxClaim` from a warm pool: Ready 0.12 s. Claim **with** env injection does not adopt a warm pod (fresh pod, ~0.9 s). |
| T3b | Replicas sharing one `daemon_id` | **FAIL (blocker for that model)** | See F3. |
| T4 | Same-project continuity | **PASS** | Suspend → pod gone, PVC kept, runtime `offline`. Task queued meanwhile stays `queued`. Resume → same runtime id `online`, 2nd task done 3.1 s later, `workdir_uses=2`, same session id resumed, same workdir path. |
| T5 | Pod killed mid-task | **PASS (no stuck state), but work is lost** | SIGTERM and force-kill both: task → `failed` ("task cancelled by server") within ~6 s; pod recreated by the Sandbox controller (Running again within ~5 s in both cases, sampled every 5 s). `attempt 1 / max_attempts 2` but no retry observed. The actor of the cancel is inferred (timing), the log does not name it. |
| T6 | Information exposure | **FAIL (as expected)** | See F5. |
| T7 | In-pod isolation probe | **FAIL** | See F2. |
| T8 | Token scope | **Mixed** | `mdt_` is workspace-bound (cross-workspace 404, rejected on user API) but **not** daemon-bound. Task token `mat_` is workspace-bound. User PAT reaches every workspace of that user. |
| T9 | One-shot / drain | **GAP** | No one-shot, idle-exit or drain flag. SIGTERM cancels the context and aborts running tasks. |

## Findings (ordered by severity)

**F1 — Daemon API authorizes by workspace membership only (critical, server).**
`requireDaemonRuntimeAccess` → `requireDaemonWorkspaceAccess`. Any member's JWT or PAT can act as the daemon of
any runtime in the workspace. Proven: a user who is only a plain `member` of workspace B claimed a task queued on
another member's runtime and received the full payload, including `auth_token`, agent `instructions`, `skills`
and the initiator's email (`results/t8-stolen-claim-keys.json`). A user outside B and an `mdt_` scoped to A were rejected.
Impact: hiding the runtime is impossible while any user can speak the daemon protocol. Fix: daemon routes must
require a daemon-class credential bound to `(workspace_id, daemon_id)` and the runtime must belong to that daemon.

**F2 — The agent can read the daemon's credential (critical, daemon/pod).**
The agent subprocess (uid 10001, non-root) reads the user-level PAT (`mul_`) from `$HOME/.multica/config.json` and from
`/proc/*/environ` of the daemon. That PAT lists and reaches every workspace the user belongs to. The task token (`mat_`) is
correctly scoped. Also: the Kubernetes service-account token is mounted; Postgres :5432 and the API server :443 are
reachable; service-link env vars leak service names. Fix: a narrow daemon token (F1), run the agent under a different uid
than the daemon, `automountServiceAccountToken: false`, `enableServiceLinks: false`, NetworkPolicy allowing only the server
and the LLM gateway, sandbox runtime class (gVisor/Kata) on real nodes.

**F3 — Replicas cannot share a `daemon_id` (blocker for the "N replicas, one runtime" model).**
A replica's graceful exit calls `/api/daemon/deregister`; the server then keeps the runtime offline
(`preserved_offline`) even though the surviving replica keeps heartbeating. A task queued afterwards stayed `queued` until a pod restart
re-registered. Use one `daemon_id` per Sandbox (project-scoped, which T4 supports), or add a managed mode that does not deregister on shutdown.

**F4 — Daemon cannot start from an env token.** `daemon start` ignores `MULTICA_TOKEN`; it needs `multica login --token`
(writes `config.json`, which is what F2 reads). Managed mode should read the credential from env or a file the agent cannot read.
I found `mdt_` tokens minted only in the remote-MCP claim path (`remoteMCPDaemonTokenForClaim`); I did not find an issuance path for daemon bootstrap.

**F5 — Runtime details are visible to every member.** Member and admin see the same keys: `daemon_id`, `device_info`
(contains the pod name), `metadata` (`cli_version`, capabilities, `launched_by`), `launch_header`, `owner_id`. Private runtimes are
hidden from non-owners, so a shared managed runtime must be `public`, which exposes all of it. Task objects expose
`work_dir` / `relative_work_dir` (absolute pod paths). Redaction must be server-side: the UI hides some of these fields, but the API returns all of them.

**F6 — Scale-in loses in-flight work.** No drain, no retry. Needs a drain signal (stop claiming, finish, exit) and a
`terminationGracePeriodSeconds` sized for agent tasks, or one-task-one-pod.

**F7 — Warm pool and per-claim identity are mutually exclusive.** Env injection forces a fresh pod, so identity
(`daemon_id`, workspace, credential) must be assigned after adoption (file/ConfigMap/API polled by the entrypoint).

**F8 — Nothing resumes a suspended Sandbox.** A task queued for an `offline` managed runtime waits forever unless a controller
flips `operatingMode` to `Running`. A suspended managed runtime also shows as `offline` to users.

## What this means for the design

1. Keep the v0.6.1 control plane. Add a thin controller that creates/resumes one Sandbox (+ PVC) per project, watches queued tasks on offline managed runtimes, and suspends idle ones.
2. Server changes (small, security-critical): daemon-class credential bound to `(workspace, daemon_id)`; daemon routes check runtime ownership by daemon; server-side redaction for managed runtimes; an admin-only detail endpoint for the permission-gated admin page.
3. Daemon changes: `--managed` mode (credential from env/file, no `login`, no deregister on SIGTERM, drain, optional one-shot, no auto-update/probe of local CLIs).
4. Pod template: separate uid for agent and daemon, no SA token, no service links, egress allow-list, gVisor/Kata RuntimeClass.

## Limits of this PoC

kind only (no gVisor/Kata, so isolation strength is untested); fake agent (no real Claude/Codex behavior, e.g. real `--resume`); images preloaded
(cold start is a lower bound); single node; v0.6.1 only (v0.6.2 has 50 more commits touching daemon/handler/migrations, not re-run); task-cancel actor in T5 inferred.
The F1 claim race against a live daemon was inconclusive, so F1 was proven on a runtime with no live daemon.

## Files

`setup.sh` · `bootstrap.sh` · `lib.sh` · `base.yaml` · `sandbox-t4.yaml` (PVC-backed daemon) · `warmpool.yaml` · `Dockerfile.daemon` ·
`fake-claude` · `probe.sh` · `results/` (raw notes, T6/T7/T8 outputs). `state.env` and `vendor/` are git-ignored.
