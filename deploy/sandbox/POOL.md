# Pool manager configuration

`sandbox-broker` runs as the pool manager when `SANDBOX_WORKSPACES_FILE` is
set (SIGHUP reloads that file). The settings are read once at startup; a bad
value stops the process with a message naming the variable. Durations use Go
syntax (`45s`, `10m`, `2h`). Unset or `0` means the default shown.

Secrets accept `NAME` or `NAME_FILE` (a path; surrounding whitespace is
trimmed). Setting both is refused. Secret values are never logged or echoed in
errors. Prefer the `_FILE` form with a mounted secret.

## Core

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_WORKSPACES_FILE` | none | Workspace list (mode 0600). Selects pool mode. |
| `SANDBOX_WORKSPACES_FILE_GROUP_READ` | `false` | `true` also accepts a workspace file its group can read (mode 0440/0640), for a Kubernetes Secret volume with `fsGroup`; group write and any access by others stay refused. |
| `MULTICA_SERVER_URL` | none | Multica server base URL. |
| `SANDBOX_BROKER_LISTEN` | `:8790` | Pod API listener. |
| `SANDBOX_POOL_ADMIN_LISTEN` | `127.0.0.1:8791` | Admin listener (inventory, leases, git verify). |
| `SANDBOX_BROKER_NAME` | `Managed Sandbox` | Machine and default runtime name. |
| `SANDBOX_POOL_SIZE` | 0 | Declared pod count, shown next to live counts. |
| `SANDBOX_PROVIDERS` | all | Comma list of agent CLIs exposed as runtimes. |
| `SANDBOX_POOL_DRAIN_TIMEOUT` | `10m` | How long a removed workspace keeps its leases. |
| `SANDBOX_POOL_HEARTBEAT_TIMEOUT` | `45s` | Silence after which a pod is listed lost. |
| `SANDBOX_POOL_RESET_TIMEOUT` | `2m` | Longest a pod may stay resetting. |
| `SANDBOX_POOL_GATEWAY_LISTEN` | off | Listener that serves only the git gateway's two verify endpoints (needs `SANDBOX_GIT_GATEWAY_URL`). |

## How a pod is reset and rejoins

The stock daemon sends no boot nonce and no `pod_ip`, so the manager needs
nothing from it to tell a reset has finished:

1. A reset starts when a lease ends (and for idle timeout, maximum lifetime, or
   a waiting task). The pod is `resetting` and the supervisor, polling
   `/pod/control`, is told to reset.
2. The supervisor kills every process, empties the three writable areas,
   rebuilds HOME, **starts the new daemon, then reports `done`**. The report
   is the acknowledgement.
3. The next registration from that IP after the acknowledgement is the new
   daemon's. It completes the reset and the pod is `idle`. A registration
   before the acknowledgement is the old daemon's and changes nothing.

The manager answers a heartbeat from a pod it does not know (or has marked lost)
with `404 runtime not found`, which is what makes the stock daemon register again;
this is how the pool rebuilds itself after a manager restart.

A pod the manager has never seen is treated the same way: its registry entry
is created `resetting`, and it is reset before it is lent. After a manager
restart every pod, a busy one included, goes through this once, because the
registry is in memory.

A pod that is lost while it holds leases has those leases ended (task failed
upstream, LiteLLM key and git token revoked, release audited). If it
registers again it is reset first.

`boot_nonce` and `pod_ip` in the registration are optional extras for daemons
that send them (a different nonce on a known IP is a new instance and replaces
the old state at once). Without `pod_ip` the manager cannot detect a rewritten
source address (SNAT, a mesh): that check is then the manual step of the
cluster smoke test (`pool/k8s/README.md`, ticket 20), which compares the pod
IPs in `/v1/inventory` with `kubectl get pod -o wide`.

Reset events are audited (`action: reset`): start, completion, timeout and
failure, with the task whose end caused it when there was one. `/v1/leases`
and the pods in `/v1/inventory` name the initiators.

## Placement and pod lifetime

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_PLACEMENT` | `task` | `task`, `issue` or `chat`: which tasks share a pod. |
| `SANDBOX_POD_CONCURRENCY` | 1 (`task`), 20 otherwise | Tasks a shared pod runs at once. |
| `SANDBOX_PLACEMENT_WAIT` | `30s` | How long a claimed task waits for a pod. |
| `SANDBOX_POD_IDLE_TIMEOUT` | `15m` | Release a shared pod with no task for this long. |
| `SANDBOX_POD_MAX_LIFETIME` | `2h` | Stop giving a shared pod new tasks after this. |
| `SANDBOX_POD_DRAIN_GRACE` | `10m` | Time running tasks get after max lifetime. |

## Audit

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_AUDIT_FILE` | off | JSON Lines audit log, appended (0600). `-` writes to stdout. |

## LiteLLM (per-task keys)

Enabled by `SANDBOX_LITELLM_URL`; any other `SANDBOX_LITELLM_*` variable
without it is an error.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_LITELLM_URL` | off | LiteLLM base URL the manager calls. |
| `SANDBOX_LITELLM_POD_URL` | the URL | Base URL given to agents, if pods reach LiteLLM differently. |
| `SANDBOX_LITELLM_MASTER_KEY` / `_FILE` | required | Admin key; stays in the manager. |
| `SANDBOX_LITELLM_MODELS` | required | Comma list of models a task key may call. |
| `SANDBOX_LITELLM_MAX_BUDGET` | required, above 0 | Budget per task key. |
| `SANDBOX_LITELLM_RPM_LIMIT` | required, above 0 | Requests per minute per task key. |
| `SANDBOX_LITELLM_TASK_TIMEOUT` | `2h` | Longest task; key expires after this plus margin. |
| `SANDBOX_LITELLM_MARGIN` | `15m` | Slack after the task timeout. |
| `SANDBOX_LITELLM_ENV` | Anthropic/OpenAI base URL and key | `NAME=VALUE,...` forced into the agent env. `${LITELLM_URL}` and `${LITELLM_KEY}` are replaced. Names must not start with `MULTICA_`. |

The budget and rate caps are mandatory when LiteLLM is on: the manager refuses
to start without both (`SANDBOX_LITELLM_MAX_BUDGET` and
`SANDBOX_LITELLM_RPM_LIMIT` above zero). A key without its own caps would be
limited only by the initiator's personal budget, and a leaked key could spend
all of it at any rate. LiteLLM's error text is stripped of the master key and
the task key before it reaches logs, the audit trail or a task's failure
message.

## Git gateway

Enabled by `SANDBOX_GIT_GATEWAY_URL`, the gateway address as pods reach it.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_GIT_GATEWAY_URL` | off | Gateway base URL for pods. |
| `SANDBOX_GIT_GATEWAY_ENV` | `GIT_GATEWAY_TOKEN=${GIT_TOKEN}` | `NAME=VALUE,...` forced into the agent env. `${GIT_TOKEN}` and `${GIT_GATEWAY_URL}` are replaced. Names must not start with `MULTICA_`. |
| `SANDBOX_GIT_GATEWAY_MCP_URL` | off | The gateway's MCP endpoint as agents reach it, always `<SANDBOX_GIT_GATEWAY_URL>/mcp` (for example `http://git-gateway:8081/mcp`). Each task then gets a `gitlab-gateway` MCP server carrying its token. Must be http(s); only valid with `SANDBOX_GIT_GATEWAY_URL`. |

The gateway (`cmd/git-gateway`) is a separate process, but the token registry
lives in the manager. The manager serves the gateway's verification endpoints
on the **admin** listener (loopback by default) and, when
`SANDBOX_POOL_GATEWAY_LISTEN` is set, on a listener of their own that carries
nothing else. Prefer the latter whenever the gateway runs in another pod, so
run the gateway with:

```
GIT_GATEWAY_VERIFY_URL=http://<manager>:8792/v1/git/verify-token
GIT_GATEWAY_POD_VERIFY_URL=http://<manager>:8792/v1/git/verify-pod
```

Agents push with the task token: the pool image's system gitconfig has a
credential helper (`git-credential-gateway`) that answers `agent` /
`$GIT_GATEWAY_TOKEN` for the gateway's origin only. The image's
`GIT_GATEWAY_URL` build argument is the gateway root, with no path prefix.

The gateway serves git at `/` and its MCP tools at `/mcp` on the same
`GIT_GATEWAY_LISTEN` listener, so the MCP URL is the gateway URL plus `/mcp`.

Gateway policy (all optional; invalid values stop the gateway at startup with
an error that names the variable):

| Variable | Default | Meaning |
| --- | --- | --- |
| `GIT_GATEWAY_BRANCH_PREFIX` | `agent/` | A task may push only `refs/heads/<prefix><task id>/*`. |
| `GIT_GATEWAY_MIN_READ_LEVEL` | `20` | Lowest GitLab access level that may clone: 10, 20, 30, 40, 50 or guest, reporter, developer, maintainer, owner. |
| `GIT_GATEWAY_MIN_PUSH_LEVEL` | `30` | Lowest access level that may push or open a merge request; not below the read level. |
| `GIT_GATEWAY_MR_TARGETS` | project default branch | Comma list of branches a merge request may target. |
| `GIT_GATEWAY_MR_LABEL` | `created-by-agent` | Label on every merge request the gateway opens. |
| `GIT_GATEWAY_TASK_URL` | none | http(s) link template for the merge request description; `{task_id}` and `{workspace_id}` are replaced. |

The verify endpoints are unauthenticated: keep the gateway listener (or, if you
move the admin listener off loopback instead, that one) unreachable from pods
with network policy. The gateway keeps its record of pushed branches in
memory, so run one replica; the record is dropped when the task ends only if
the gateway shares the manager's token registry, and otherwise ages out
(`PushLogTTL`, 24 hours) or is capped at 10000 tasks.
