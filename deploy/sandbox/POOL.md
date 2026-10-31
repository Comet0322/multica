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
| `MULTICA_SERVER_URL` | none | Multica server base URL. |
| `SANDBOX_BROKER_LISTEN` | `:8790` | Pod API listener. |
| `SANDBOX_POOL_ADMIN_LISTEN` | `127.0.0.1:8791` | Admin listener (inventory, leases, git verify). |
| `SANDBOX_BROKER_NAME` | `Managed Sandbox` | Machine and default runtime name. |
| `SANDBOX_POOL_SIZE` | 0 | Declared pod count, shown next to live counts. |
| `SANDBOX_PROVIDERS` | all | Comma list of agent CLIs exposed as runtimes. |
| `SANDBOX_POOL_DRAIN_TIMEOUT` | `10m` | How long a removed workspace keeps its leases. |
| `SANDBOX_POOL_HEARTBEAT_TIMEOUT` | `45s` | Silence after which a pod is listed lost. |
| `SANDBOX_POOL_RESET_TIMEOUT` | `2m` | Longest a pod may stay resetting. |

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
| `SANDBOX_LITELLM_MAX_BUDGET` | no cap | Budget per task key. |
| `SANDBOX_LITELLM_RPM_LIMIT` | no cap | Requests per minute per task key. |
| `SANDBOX_LITELLM_TASK_TIMEOUT` | `2h` | Longest task; key expires after this plus margin. |
| `SANDBOX_LITELLM_MARGIN` | `15m` | Slack after the task timeout. |
| `SANDBOX_LITELLM_ENV` | Anthropic/OpenAI base URL and key | `NAME=VALUE,...` forced into the agent env. `${LITELLM_URL}` and `${LITELLM_KEY}` are replaced. Names must not start with `MULTICA_`. |

## Git gateway

Enabled by `SANDBOX_GIT_GATEWAY_URL`, the gateway address as pods reach it.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SANDBOX_GIT_GATEWAY_URL` | off | Gateway base URL for pods. |
| `SANDBOX_GIT_GATEWAY_ENV` | `GIT_GATEWAY_TOKEN=${GIT_TOKEN}` | `NAME=VALUE,...` forced into the agent env. `${GIT_TOKEN}` and `${GIT_GATEWAY_URL}` are replaced. Names must not start with `MULTICA_`. |
| `SANDBOX_GIT_GATEWAY_MCP_URL` | off | The gateway's MCP endpoint as agents reach it, always `<SANDBOX_GIT_GATEWAY_URL>/mcp` (for example `http://git-gateway:8081/mcp`). Each task then gets a `gitlab-gateway` MCP server carrying its token. Must be http(s); only valid with `SANDBOX_GIT_GATEWAY_URL`. |

The gateway (`cmd/git-gateway`) is a separate process, but the token registry
lives in the manager. The manager serves the gateway's verification endpoints
on the **admin** listener, so run the gateway with:

```
GIT_GATEWAY_VERIFY_URL=http://<manager>:8791/v1/git/verify-token
GIT_GATEWAY_POD_VERIFY_URL=http://<manager>:8791/v1/git/verify-pod
```

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

Because the default admin listener is loopback-only, set
`SANDBOX_POOL_ADMIN_LISTEN` to an address the gateway can reach (for example
the manager's private network IP) and keep that address unreachable from pods
with network policy: the endpoints are unauthenticated.
