# Kubernetes sandbox runtime (design draft)

Status: MVP implemented (see [MVP implementation](#mvp-implementation)); the rest is design.
Evidence: file references are to the tree at commit `1c908ea52`. Lines that were
only sampled, not read end to end, are listed under [Open questions](#open-questions).

## Goal

Run each dispatched task in its own short-lived, isolated Pod so that many
tasks can be processed in parallel and untrusted agent-generated code never
shares a kernel or filesystem with other tasks.

Non-goals for the first version:

- `local_directory` and local worktree tasks (they are bound to a user machine).
- Changing the server protocol in a way that older desktop daemons cannot follow.
- Replacing the existing StatefulSet runtime (`deploy/helm/multica/templates/runtime.yaml`).

## Baseline: what already exists

`runtime.yaml` already deploys a **StatefulSet of standard daemons**
(`multica daemon start --foreground`, `MULTICA_DAEMON_ID=$(POD_NAME)`,
`MULTICA_DAEMON_MAX_CONCURRENT_TASKS`, a config Secret seeded into
`$HOME/.multica`). That is the "one long-lived daemon per Pod" model. It scales
throughput but gives no per-task isolation, and agents run as child processes
of the daemon. This design adds a second mode next to it, not a replacement.

## Constraints from the current protocol

These are facts the design has to respect.

| Area | Fact | Evidence |
| --- | --- | --- |
| Direction | The daemon connects out. The server never calls a daemon; it queues work and sends WS hints. | `router.go:1546`, `daemonws/hub.go` |
| Claim | Atomic DB claim. `dispatched_at` is the claim generation. `max_tasks=0` claims nothing; cap is 32. | `agent.sql:743`, `handler/daemon.go:1689` |
| Fence | Only `start` is generation-fenced (409 = stale claim). `complete`, `fail`, `cancel-ack`, `progress`, `messages` are keyed by task id only. | `service/task.go:4184`, `agent.sql:986` |
| Liveness | Heartbeat every 15s per runtime. Offline after 150s. Running tasks fail only after a 3h reconnect grace. **A dead Pod under a heartbeating controller is never noticed by the server.** | `runtime_sweeper.go:38-89` |
| Cancel | No push. The daemon polls `GET /tasks/{id}/status` every 5s; terminal status or 404 interrupts the agent. Other errors never interrupt. | `daemon.go:5637-5664, 5828-5835` |
| Auth | `DaemonAuth` accepts `mdt_`, `mul_` (PAT), `mcn_` (cloud PAT) and JWT. It does **not** accept the task token `mat_`. | `middleware/daemon_auth.go:99-260` |
| `mdt_` | Workspace + daemon_id scoped, 24h. Minted only per claim that has Remote MCP. It is not the daemon's normal credential. | `handler/daemon.go:2217`, `migrations/029` |
| Daemon credential | Read from `~/.multica/config.json`, never from `MULTICA_TOKEN`. Normally a PAT. | `daemon.go:2214`, `cmd_daemon.go:528` |
| Register | One workspace per call. A runtime is `(workspace, daemon_id, provider)` or `(workspace, daemon_id, profile_id)`. At least one runtime or failed profile is required. | `handler/daemon.go:196-221, 424` |
| Recovery | `recover-orphans` fails **every** dispatched/running task for the runtime with no ownership check. | `agent.sql:1314`, `task_lifecycle.go:27` |
| Agent callbacks | The agent CLI uses the task token `MULTICA_TOKEN` (`mat_`, bound to agent+task) against the normal user API. The daemon refuses to fall back to its own credential. | `types.go:173`, `daemon.go:8456` |
| Repo checkout | `multica repo checkout` posts to `http://127.0.0.1:$MULTICA_DAEMON_PORT/repo/checkout`, authenticated by the task token looked up in an in-memory map, with a workdir that must exist on that filesystem. The URL is hard-coded. | `cmd_repo.go:389`, `health.go:175, 264` |

## Architecture

```
                 outbound only
 multica server <──────────────── controller (1 leader, in cluster)
                                     │  owns: daemon credential, claim, lease,
                                     │  start, heartbeat, cancel poll,
                                     │  terminal reports, Pod lifecycle
                                     │
                     create / watch  │   in-cluster gRPC (Pod → controller)
                                     ▼
                              Sandbox Pod (per task)
                              runner + agent CLI + local /repo/checkout
                                     │
                                     └── task token (mat_) ──> multica server
                                         (agent CLI calls only)
```

Principle: **the daemon credential never enters a sandbox.** The Pod holds only
the task token, which is already the least-privilege credential the native
daemon gives its agent child process. This matches native behaviour and adds no
new permission.

### Components

1. **Controller** (new binary, `multica-k8s-controller`).
   Acts as a daemon. Registers runtimes, heartbeats, claims, and drives Pods.
2. **Runner** (new binary in the sandbox image).
   Runs exactly one task: prepares the workdir, starts the agent through
   `pkg/agent`, serves the local `/repo/checkout` endpoint, streams events to
   the controller, exits.
3. **Sandbox layer**: `kubernetes-sigs/agent-sandbox` (`SandboxTemplate`,
   `SandboxWarmPool`, `SandboxClaim`) with a `RuntimeClass` for gVisor or Kata.
   The controller uses these primitives only; it does not depend on a full
   sandbox platform. See [Sandbox layer](#sandbox-layer).

## Credential model

| Credential | Held by | Used for |
| --- | --- | --- |
| Daemon credential (PAT `mul_` or `mcn_`) | Controller only, k8s Secret | register, heartbeat, claim, start, prepare-lease, messages, progress, usage, complete, fail, cancel-ack, status, session pin |
| `mdt_` (per-claim, from `RemoteMCPDaemonToken`) | Controller only | Remote MCP credential and plugin-hook calls, proxied to the Pod (see [MCP](#local-mcp-servers)) |
| Task token `mat_` | Pod, injected as `MULTICA_TOKEN` | agent CLI calls, and the key for the Pod-local `/repo/checkout` |

Consequence: a PAT with workspace membership is what runs the controller.
Document that it should belong to a dedicated service user, and that the
controller registers one workspace per call (loop over the workspaces it
serves).

## Controller design

### Registration and capacity

- One `daemon_id` per controller **leader**. Standby replicas do not register.
  This avoids crossed `runtime_id` sets, which the claim rejects on `daemon_id`
  mismatch.
- Register one runtime per provider the sandbox image contains (built-in
  providers) and any custom profiles it can serve. Do not advertise
  `local-worktree-v1` or local directory support.
- `max_tasks` = free capacity, never more. Capacity is the smaller of the warm
  pool headroom and a configured cluster cap. Over-claiming strands tasks until
  the 90s reclaim or 300s fail.
- Claim through WS `tasks.claim` with HTTP fallback, as `wsrpc.go` does.

### Per-task state machine

```
claimed ──lease loop──> pod_requested ──pod ready──> start ──ok──> running
   │                          │                        │409          │
   │                          │timeout                 └─> drop Pod  │ events
   │                          └─> fail(timeout)                      ▼
   └── controller lost claim → server reclaims after 90s      terminal report
```

State is stored in **Pod labels and annotations**, not in controller memory:
`task_id`, `runtime_id`, `workspace_id`, `dispatched_at`, `daemon_id`.
Pod name is deterministic from `task_id`, so a duplicate create returns
AlreadyExists.

| Step | Server call | Notes |
| --- | --- | --- |
| Claim | `POST /api/daemon/tasks/claim` | persist `dispatched_at` verbatim |
| Prepare | `POST /runtimes/{rt}/tasks/{id}/prepare-lease` every ~15s | covers scheduling and image pull; lease is 45s |
| Skills | `POST .../skill-bundles/resolve` | resolved by the controller and shipped inside the Pod input |
| Start | `POST /tasks/{id}/start {runtime_id, dispatched_at, capabilities}` | after the Pod reports "env ready". 409 = not mine: delete Pod, send nothing |
| Run | `POST /tasks/{id}/messages`, `/progress`, `/usage` | proxied from Pod events, 500ms batching as in `executeAndDrain` |
| Terminal | `complete` / `fail` | re-check `GET /status` first; use a durable outbox like `reportTerminalTask` |

Start ordering matters: the native daemon calls `StartTask` only after the
workdir exists (`daemon.go:8392`, issue #3999). The Pod must therefore signal
"environment ready" and the controller must return the negotiated supplement
capability before the agent launches.

### Heartbeat

Heartbeat every 15s per registered runtime. Because heartbeating is the only
liveness signal, **Pod health must be tied to it**: the controller watches Pod
phase and fails the task itself when a Pod dies. Otherwise the server waits 3h.

### Cancellation

There is no server push, so the controller polls `GET /tasks/{id}/status` for
each running task every 5s, and immediately after a WS reconnect. On terminal
status or 404 it deletes the Pod (grace period, then kill), then sends
`cancel-ack` with branch and durable workdir, retried. Non-404 errors never
cancel, matching `shouldInterruptAgent`.

Before `complete` or `fail`, re-check status. `complete` requires `running`;
`fail` requires `dispatched`, `running` or `waiting_local_directory`.

### Failure matrix

| Event | Controller action | Server result |
| --- | --- | --- |
| Pod pending too long (image pull, no capacity) | delete Pod, `fail` with `timeout` before 300s dispatched limit | retryable |
| Pod OOM / crash / eviction | `fail` with `agent_error` for the agent's own crash, `runtime_recovery` or `timeout` where a retry is wanted | only `timeout`, `runtime_recovery`, `codex_semantic_inactivity` are auto-retried (`task.go:5216-5223`) |
| Agent silent > idle watchdog (default 2h, tool budget in flight) | kill Pod, report as the daemon does (`blocked` + `idle_watchdog`) | not retried |
| `start` returns 409 | delete Pod, no fail | task re-delivered by server |
| Cancel | poll, delete Pod, `cancel-ack` | already `cancelled` |
| Controller restart | rebuild from Pod annotations, see below | none if resumed within grace |
| Duplicate controller | leader election + deterministic Pod name + drop on 409 | DB claim already atomic |

### Controller restart

Do **not** call `recover-orphans` on start if Pods can outlive the controller;
it fails every in-flight task for the runtime with no ownership filter. Instead:

1. List task Pods, rebuild state from annotations.
2. Resume heartbeat, lease and status polling.
3. Server terminal or 404 and Pod exists: delete the Pod.
4. Server says `running` but no Pod: `fail` that single task with `runtime_recovery`.
5. Use `recover-orphans` only when all Pods are known lost.

A restart longer than 150s takes the runtime offline. Claims resume once
heartbeats are fresh; running tasks survive until the 3h grace.

## MVP implementation

The MVP deliberately differs from the "Reporter / event channel" cut described
under [Runner design](#runner-design). Instead of refactoring `daemon.go`, the
runner reuses `handleTask` **unchanged** and the controller relays its HTTP:

```
Pod (runner, task token mat_) ──> relay (in controller) ──> server
                                   checks token→task scope,
                                   swaps in the daemon credential
```

- `internal/k8sruntime/relay`: scoped reverse proxy. A Pod authenticates with
  its task token. For `/api/daemon/*` the relay allows only the endpoints the
  task pipeline needs, only for that token's task, runtime and workspace, and
  forwards with the daemon credential. Everything else (the agent CLI's user-API
  calls) is forwarded untouched with the task token. Unknown endpoints,
  `register`, `heartbeat`, `tasks/claim`, `recover-orphans` and other tasks are
  denied. The per-claim `mdt_` token stays in the relay; the Pod receives its
  own task token in that field so the Remote MCP callbacks still map back.
- `internal/daemon/single_task.go`: `RunSingleTask` seeds one runtime and
  workspace, serves the loopback `/repo/checkout`, runs `handleTask`, and gives
  an undelivered terminal report a bounded last chance. `TaskShouldStop` exports
  the native cancellation rule. `daemon.go` is untouched.
- `internal/k8sruntime/controller`: registers runtimes, heartbeats, claims at
  most free capacity, creates one Pod per task, extends the prepare lease while
  the Pod starts, fails tasks whose Pod dies or never starts, backstops
  cancellation, and adopts existing Pods on restart without `recover-orphans`.
- `internal/k8sruntime/kube`: stdlib-only Kubernetes REST driver. The task
  input (which contains the task token) goes through a per-task Secret, never
  the Pod spec. Pods run as uid 1000, no service account token, all capabilities
  dropped, optional `RuntimeClass`.
- `cmd/multica-k8s-controller`, `cmd/multica-k8s-runner`,
  `Dockerfile.k8s-sandbox`, `deploy/k8s-sandbox/controller.yaml` (RBAC,
  Deployment, relay Service, egress NetworkPolicy).

Answers to the open questions, as implemented:

1. **Server changes**: none. The relay is a task-scoped credential in practice,
   so no `DaemonAuth` change is needed.
2. **Per-task credential for daemon calls**: provided by the relay, not the server.
3. **Session continuity**: not supported; every Pod starts fresh.
4. **Remote MCP and supplements**: routed through the relay and allowed, but
   only exercised by unit tests. Adopted tasks (after a controller restart) lose
   the per-claim MCP token because it is not persisted.
5. **Git credentials**: static Secret via `MULTICA_K8S_ENV_SECRETS` (`GH_TOKEN`).
   Per-task, repo-scoped token exchange is not implemented.

Not in the MVP: leader election (single replica, `Recreate`), WS wake-ups
(the controller polls every 2s), autoscaling metrics, warm pools, and a
Helm chart (example manifests only). The runner refuses to start if the task's
provider CLI is missing from the image.

Verification actually run (Go 1.27.1 on the host, non-root): `go vet` and
`gofmt` are clean; unit tests for the relay, controller and Kubernetes driver,
plus an end-to-end test, pass (25 tests). The end-to-end test runs the real
`runTask` with a fake `claude` through the real relay against a fake server and
asserts that every daemon call reached the server with the daemon credential
the Pod never held. The whole `internal/daemon/...` suite also passes. In
`pkg/agent`, four cursor background tests fail, and they fail identically on an
untouched checkout of HEAD, so they are unrelated to this work.

Kubernetes test (kind v0.30 / Kubernetes 1.34, its own Postgres and a server
built from this tree, all on a private Docker network, with a fake `claude`
because no model key was used). Passed:

- A task assigned through the real server ran in a Pod as uid 1000, received an
  env var from the `envFrom` Secret, completed through the relay and the Pod and
  per-task Secret were removed. Queue to done was about 2.5s.
- 6 issues at once: all completed, none failed, nothing left behind.
- Pod force-deleted mid-run: the task was failed as `runtime_recovery` within 3s,
  the server retried it and attempt 2 completed.
- User cancel mid-run: the runner noticed, Pod and Secret were removed within
  6s, the task stayed `cancelled` and was not retried.
- Controller restarted mid-run: it adopted the Pod, the task completed in one
  attempt with no failure and no retry.

Bugs found only by running it, now fixed: the controller image used a
non-numeric `USER`, which the kubelet rejects under `runAsNonRoot`; and a
runner that received SIGTERM cancelled the pipeline and reported the task as
`cancelled`, which the server never retries. The runner now exits 143 without
reporting, and the controller reports a retryable `runtime_recovery`.

Known gap from the same test: a sandbox Pod could still open a TCP connection
to the kube-apiserver Service even though the example NetworkPolicy excludes
private ranges (see the comment in `deploy/k8s-sandbox/controller.yaml`). Not
investigated further.

Not run: gVisor/Kata, a real model call, a cluster other than kind, or the full
repo suite (`make test` needs the database). Pod startup time here is not
representative: the images were preloaded into the node.

## Running it in a restricted company environment

The controller is an operator: GitOps manages its static resources (Deployment,
Role, Service, NetworkPolicy); it creates Pods and Secrets at run time, which
ArgoCD does not track because they carry no `app.kubernetes.io/instance` label.
Nobody needs `kubectl`. What is missing for that setting today:

| Gap | Effect | Status |
| --- | --- | --- |
| No `imagePullSecrets` on sandbox Pods | private registry pulls fail | not implemented |
| No custom Pod labels, annotations, tolerations or priority class | admission policies or taints can reject or strand Pods | only `nodeSelector` and `RuntimeClass` |
| Dockerfile pulls public base images, Go modules and npm packages | fails behind a firewall | needs build args for mirrors |
| Secrets in the example were created with kubectl | use SealedSecrets or ExternalSecrets | documented in the manifest |
| Egress rules are only an example | needs a CNI-specific allow-list for the model gateway and git host | see the known gap above |
| Raw YAML, no Helm chart | ArgoCD works best with Helm or Kustomize | not implemented |

Platform prerequisites: the `RuntimeClass`, a Role that lets the controller
create Pods and Secrets in its namespace, and egress to the model API and git
host.

## Runner design

### Cut point

The native seam is the `taskRunner` interface (`daemon.go:198`) and
`TaskResult`. The runner covers what `runTask` does from identity validation
through the `TaskResult` build (`daemon.go:7662` to `9080`), **minus** four
calls that it replaces with events to the controller:

| Native call | Replacement |
| --- | --- |
| `StartTask` (`8392`) | event `env_ready`, then wait for `start_ack {supplement_capability}` |
| `ReportProgress` | event `progress` |
| `ReportTaskMessages` (drain goroutine, `9472`) | event `messages` |
| `PinTaskSession` (`9560`) | event `session` |

Introduce a `Reporter` interface used by `executeAndDrain`; the native daemon
implements it with `d.client`, the runner with the gRPC stream. This is a
refactor of `daemon.go` and should land first, behind unchanged behaviour and
existing tests.

### Runner inputs

The controller resolves everything that needed daemon state and passes a fully
resolved input: the `Task` (with `AuthToken`, `Agent.Skills` and MCP filled in),
agent entry path and version, resolved timeouts from config, `slot`, and the
workspace repo list and refs (today held in `d.workspaces[ws].taskRepoURLs`).

### Filesystem

`execenv.Prepare` builds `workdir/`, `output/`, `logs/`, `multica-config/` and
per-provider homes under `WorkspacesRoot`. In a Pod:

- `WorkspacesRoot` is an `emptyDir` (or ephemeral volume) per task.
- Call `execenv.Prepare` in-process (the nil `executionEnvironmentCommand` path)
  instead of re-exec'ing the daemon helper, or provide the helper entrypoint.
- GC bookkeeping (`markActiveEnvRoot`, `.gc_meta.json`, `ClaimEnvRoot`) is moot
  in an ephemeral Pod and is skipped.

### Environment

The runner sets the same variables as `taskMulticaEnvironment`
(`daemon.go:177`): `MULTICA_TOKEN` (task token), `MULTICA_SERVER_URL`,
`MULTICA_DAEMON_PORT` (the runner's local port), `MULTICA_WORKSPACE_ID`,
`MULTICA_AGENT_NAME`, `MULTICA_AGENT_ID`, `MULTICA_TASK_ID`,
`MULTICA_TASK_SLOT`, `TMPDIR`. Custom env is layered with the existing blocklist.
Provider config (`~/.codex`, `~/.hermes`, ...) is provided by mounted Secrets or
`CustomEnv`, not seeded from a user home.

### Repo checkout

The runner hosts its own `127.0.0.1` server on `MULTICA_DAEMON_PORT` exposing
`/repo/checkout` with the same rules as `health.go`: token in an in-process
map, `workspace_id` and `task_id` must match, `workdir` must be inside the
task workdir. It reuses `repocache` and the workspace repo list from the input.
Repo cache: shared read-only PVC or per-node cache to avoid full clones per Pod.
No CLI change is needed because the URL is loopback inside the Pod.

### Local MCP servers

- **Runtime MCP** (`runtime_mcp.go`): reads machine config files. Not applicable;
  MCP config comes from the agent's `mcp_config` in the input, or from the image.
- **Plugin-hook MCP** and **Remote MCP broker**: per-task loopback servers that
  call the server with `RemoteMCPDaemonToken` (`mdt_`). To keep `mdt_` out of the
  Pod, the runner keeps the loopback servers but the credential and hook calls
  go through a controller RPC (`ResolveRemoteMCPCredential`, `InvokeAgentPluginHook`).
  Phase 1 may simply not advertise Remote MCP support; setup failure already
  fails the task unless the connection is optional.

### Supplements

Advertise `task-supplement-v1` only in a later phase. It needs the controller to
proxy `supplements/claim` and `ack` and the Pod to expose a channel into
`session.Supplement()`. Without the capability, mid-run user input falls back to
the normal comment-triggered run.

## Git credentials for push and pull requests

Today git auth is ambient on the daemon host. `repocache.gitEnv` passes the full
daemon environment so credential helpers such as `gh` work
(`repocache/cache.go:25`), and agents open PRs themselves with `gh pr create`
(`execenv/runtime_config_sections.go:448`). The server never mints git
credentials; `integrations/vcs` and the webhook handlers only link PRs back to
issues. A Pod has no such ambient login, so credentials must be provided.

Decision: **short-lived, repo-scoped token injected per task.**

- The controller exchanges a GitHub App installation token (or a GitLab/Forgejo
  project or deploy token) for one scoped to the task's repositories only, with
  `contents:write` and `pull_requests:write`, expiring shortly after the task
  timeout. No admin or workflow permissions.
- Delivered as a per-task Secret mounted into the Pod. The runner exports
  `GH_TOKEN` (and a `GIT_ASKPASS` helper for plain git) so both `git` and
  `gh`/`glab` work unchanged.
- Never a shared long-lived PAT: the agent runs arbitrary code.
- Branch protection stays the guard against pushes to the default branch; the
  native flow already works on an isolated task branch.
- Egress policy must allow the git host, or clone and push fail.
- Deferred alternative: a git/API proxy in the controller so the Pod holds no
  credential at all. Stronger, but `gh pr create` calls the host API, so the
  proxy must cover that too.

The token source (which repos map to which App installation) is not defined in
the server today; the MVP takes a static credential from a Secret and leaves
the per-task exchange for a later phase.

## Sandbox layer

Use `kubernetes-sigs/agent-sandbox` primitives:

- `SandboxTemplate`: runner image (with agent CLIs), resources, `RuntimeClass`.
- `SandboxWarmPool`: pre-warmed Pods to cut cold start.
- `SandboxClaim`: what the controller creates per task.

Isolation: gVisor for development and CI; Kata (or Firecracker via Kata) for
production. Add `NetworkPolicy` for egress. Agent-sandbox does not provide
egress control, autoscaling or multi-cluster, so those are ours.
OpenSandbox is a reference for egress and credential injection, not a
dependency.

A warm Pod is claimed **before** its task is known to the sandbox layer, so the
task input must be delivered after claim (controller to runner over gRPC), not
baked into the Pod spec. That is why the runner waits for input.

## Scaling and metrics

- There is no queue-depth metric today. `ListPendingTasksByRuntime` returns full
  task lists, not counts, and requires runtime access. `multica_agent_task_in_progress`
  is per process.
- Add a queue-depth gauge (queued tasks per provider or runtime) or expose a
  count endpoint. Alternatively a KEDA Postgres scaler over `agent_task_queue`.
- The controller itself scales by warm pool size and its cluster cap, not by
  replicas (single leader).
- `METRICS_ADDR` is opt-in and not wired in the chart; add a Service and
  ServiceMonitor if used.

## Deployment

Add to `deploy/helm/multica/`, following existing conventions
(`images.<component>`, `existingSecret`, per-component `resources`,
`affinity`, `tolerations`):

- `controller.yaml`: Deployment (2 replicas, leader election), ServiceAccount,
  Role for Pods and agent-sandbox CRDs.
- `sandbox-template.yaml` and `sandbox-warmpool.yaml`, gated by values.
- A runner image build. The current `Dockerfile` bundles no agent CLIs, so a
  documented base plus agent CLI layer is needed.
- Disable daemon auto-update in this mode (`MULTICA_DAEMON_AUTO_UPDATE=false`);
  roll out by image tag.

## Rollout plan

1. **Refactor**: extract `Reporter` from `executeAndDrain` and `handleTask`.
   No behaviour change; existing daemon tests must pass.
2. **Runner binary** with local `/repo/checkout`, run locally against a fake
   controller to validate the seam.
3. **Controller MVP**: claim, lease, start, heartbeat, Pod create, terminal
   report, single provider, no Remote MCP, no supplements. Plain Pods first,
   no warm pool.
4. **Cancel, crash and restart handling** (the failure matrix above), with
   fault-injection tests.
5. **Sandbox layer**: agent-sandbox, `RuntimeClass`, warm pool, `NetworkPolicy`.
   Measure cold and warm latency.
6. **Metrics and autoscaling**.
7. Remote MCP proxying, then supplements.

## Testing

- Unit: `Reporter` implementations; controller state machine with a fake server
  and fake Pod API.
- Fault injection: kill Pod mid-run, kill controller mid-run, delay start past
  the 90s reclaim, cancel during prepare, duplicate controller.
- Integration: kind cluster with gVisor or a plain runtime; one task end to end.
- Follow repo rules: no test resolves user-installed agent CLIs; use fake
  executables. Real-agent smoke tests only when explicitly authorized.

## Open questions

1. **Server changes.** Complete, fail and cancel-ack are not generation-fenced.
   Should the server add `dispatched_at` fencing, or is leader election enough?
2. **Per-task credential for daemon calls.** Would a task-scoped credential for
   `messages`/`complete`/`fail` (so the Pod could report directly and the
   controller drop out of the data path) be acceptable? It removes the
   controller as a streaming bottleneck but changes `DaemonAuth`.
3. **Session continuity.** `PriorSessionID` and `PriorWorkDir` resume assumes a
   persistent local workdir. Do we need a per-issue or per-chat PVC, or accept
   fresh sessions in Pod mode?
4. **Workspace fan-out.** Registration is one workspace per call. One controller
   for many workspaces means many runtimes under one `daemon_id`; confirm there
   is no practical limit.
5. **Cold-start numbers** for the chosen isolation runtime are unmeasured.
   Published agent-sandbox material gives none; OpenSandbox's ~80ms figure is a
   vendor claim.
6. **Not fully read**: how `reportTaskResult` sends `blocked`/`idle_watchdog`,
   the daemon-side supplement crash-recovery path, the remainder of
   `repoCheckoutHandler` after authorization, and the request/response shapes of
   `cloudruntime.Client` (an alternative integration point through the Fleet
   node API, not evaluated here).
7. **Provider auth in Pods.** Which providers can be authenticated purely by
   API-key env and which need an interactive login state?

## Risks

- `daemon.go` is about 450KB; the `Reporter` extraction touches a large,
  heavily tested file.
- agent-sandbox is at v1beta1; CRD changes may break upgrades.
- Heartbeating from a healthy controller over dead Pods is the main correctness
  hazard; the Pod-watch to `fail` path is mandatory, not optional.
- The PAT used by the controller is a broad credential; scope and rotate it.
