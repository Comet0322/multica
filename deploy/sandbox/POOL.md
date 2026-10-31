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

## Daemon and agent users

Inside a pool pod PID 1 is root (capabilities CHOWN, DAC_OVERRIDE, FOWNER,
KILL, SETUID, SETGID, no-new-privileges). The Multica daemon runs as
`sandboxd` (uid 1001) and every agent as `node` (uid 1000); both have primary
group 1000.

The stock daemon starts each agent as its own user and cannot be told
otherwise, so the split is done around it:

- The daemon's `PATH` starts with `/usr/local/libexec/pod-agents`, which holds
  a hard link of `pod-supervisor` named like each installed agent CLI
  (`AGENT_COMMANDS` build argument, default `claude`). The daemon finds and
  runs that "CLI". Run under an agent's name the binary is a shim: it sends its
  arguments, working directory, environment and stdin/stdout/stderr
  (`SCM_RIGHTS`) to the supervisor over an abstract unix socket, and the
  supervisor runs the real CLI, found on the `PATH` without the shim directory,
  as uid 1000 in its own process group. The shim relays signals and exits with
  the agent's status. If the shim dies (the daemon kills its process group) the
  agent and its process tree are killed.
- Only a peer with uid 1001 (`SO_PEERCRED`) may use the socket, and only to run
  a CLI name found on the real `PATH`. No agent can start while a reset is in
  progress.
- The daemon's `HOME` is `/tmp/.multica-daemon` (mode 0700, uid 1001) and holds
  its CLI config (manager URL, placeholder token). `/tmp` is sticky, so the
  agent user cannot rename or remove it. `/home/agent` is the agent's `HOME`.
  Both are rebuilt from their templates on reset.
- The supervisor runs with umask 002. Before launching, the shim opens the
  files the daemon created in the task directory to the shared group. `git`
  trusts both users (`safe.directory *`, `core.sharedRepository group`).

What the agent user cannot do, checked by `pool/verify.sh` against a container:
read the daemon's config or `HOME`, read `/proc/<daemon>/environ`, signal or
`ptrace` the daemon, rename its `HOME`, or use the launcher.

### Residual risks

The split is not a full isolation of the daemon, because the daemon is unchanged:

1. **The daemon runs code that the agent can influence.** The daemon and the
   agent share the task files and the bare repository cache (`.repos`), which
   the agent must be able to write for commits to work. The daemon runs `git`
   there as uid 1001. An agent can plant a repository config (`core.fsmonitor`,
   a filter driver, an alias, `core.sshCommand`) or a hook there and have it
   executed as the daemon the next time the daemon runs git in that repository,
   for example in `multica repo checkout`. That gives the agent the daemon's
   user and so its `HOME` and process. What it reaches this way is the
   placeholder token and the manager address, neither of which is a secret
   (anything in the pod can already call the manager from the pod's IP), plus
   the ability to signal or ptrace the daemon. It does not reach the
   supervisor, root, or any credential of the manager, LiteLLM or GitLab. A fix
   needs the daemon to run git as the agent user or to keep the cache out of
   the agent's reach, i.e. a daemon change.
2. **Daemon network surface.** The agent shares the pod's network namespace,
   so it can reach the daemon's local HTTP listener (the `multica repo
   checkout` endpoint, authorised by the task token) and the manager. The
   manager already treats the pod's IP, not a secret, as the identity.
3. **Files created later by the daemon.** Files that the daemon creates itself
   (not through `git`) in a task directory after the agent was started get
   mode 0755 and are not writable by the agent. Directories and files created
   before launch, and everything `git` creates (umask 002), are.
4. **Workspace-level boundary.** All task directories in a shared pod are
   readable by both users (decision B in the spec); the user split does not
   separate tasks from each other.
5. **Agent CLIs not shimmed.** Only the names in `AGENT_COMMANDS` run as the
   agent user. Anything else the daemon starts by itself (its own `git`,
   helper processes) runs as the daemon user, and an agent CLI that is missing
   from `AGENT_COMMANDS` but installed would run as the daemon. Keep the list
   equal to the installed CLIs.
6. **Same-user processes.** Processes of one task can still signal and ptrace
   each other and those of other tasks in a shared pod, as before.

## Claude Code in the pod

Checked against Claude Code 2.1.314 with a fake Anthropic-style endpoint, the
daemon's argument list (`--strict-mcp-config --mcp-config <file>`, stream-json)
and an empty, even read-only, HOME:

- `ANTHROPIC_AUTH_TOKEN` + `ANTHROPIC_BASE_URL` (the `DefaultLiteLLMEnv`
  pair) are honoured: requests go to `<base>/v1/messages?beta=true` with
  `Authorization: Bearer <key>` and `anthropic-version: 2023-06-01`. No login
  or onboarding is needed. The model is the agent's `--model`, else Claude
  Code's own default (`claude-opus-5-5` for this version): the task key's
  `Models` must list it, and the small fast model
  (`ANTHROPIC_DEFAULT_HAIKU_MODEL`) if the deployment maps it elsewhere.
- The gateway's MCP entry (`type: http`, `headers.Authorization`) connects,
  lists and calls its tools; the Bearer header is sent on every POST.
- The image sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1` and
  `DISABLE_AUTOUPDATER=1`; without them Claude Code opens a connection to
  `api.anthropic.com` at start (telemetry). It is not fatal when blocked.
- Claude Code needs a writable HOME only to keep session state; it still runs
  with a read-only one, but `--resume` then has nothing to resume.
- If the LiteLLM models are not served by Anthropic's own API (Bedrock,
  Vertex, other vendors), add `ENABLE_TOOL_SEARCH=false` and
  `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` to the agent's `custom_env` or
  `LiteLLMConfig.Env`: by default Claude Code sends the `advanced-tool-use`,
  `context-management` and other beta headers and defers MCP tools behind a
  `ToolSearch` tool, none of which such backends are guaranteed to accept.
