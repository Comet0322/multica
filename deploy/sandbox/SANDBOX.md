# Sandbox (pod image and pod supervisor)

The sandbox is what runs a task: a pod of two non-root containers of one image.
In the `daemon` container PID 1 is `pod-supervisor`, which starts the stock
`multica daemon` as uid 1001 and resets the pod when the sandbox manager asks;
in the `agent-runner` container PID 1 is `pod-supervisor -role runner`, which
starts every agent CLI as uid 1000. Nothing runs as root or with a capability. The sandbox manager and
the git gateway are described in [POOL.md](POOL.md).

| File | What |
|---|---|
| `Dockerfile.pool` | The pod image (daemon, agent CLIs, pod supervisor) |
| `pool/entrypoint.sh`, `pool/pip.conf`, `pool/git-credential-gateway` | Image contents: supervisor entry point, package mirror settings, git credential helper for the gateway |
| `home/` | Files copied into the agent's home at every pod reset |
| `fake/` | A stand-in agent CLI for tests without a model |
| `pool/k8s/sandbox/` | The pods' Deployment and kustomization |

## Daemon and agent containers

A pool pod has two containers of one image. Nothing in the pod runs as root:
no process, no capability (all dropped, `no-new-privileges`), no setuid
binary used. The cluster needs no Kubernetes API for the pod and the image
needs no root at run time.

| | `daemon` container | `agent-runner` container |
|---|---|---|
| User | `sandboxd`, uid 1001 | `node`, uid 1000 |
| PID 1 | `pod-supervisor` (polls the manager, runs the daemon) | `pod-supervisor -role runner` (starts agent CLIs) |
| Own volume | `/home/daemon` (the daemon's `HOME`, with its `tmp`) | `/home/agent` (the agent's `HOME`) |
| Shared volumes | `/work`, `/tmp`, `/dev/shm` | the same |
| Processes visible | its own | its own |

Both run as group 2000 and the pod's `fsGroup` is 2000, so files in the shared
volumes are group 2000 and the group can use them (umask 007, set by both PID
1s). The two users have no other file in common.

The stock daemon starts each agent as its own user and cannot be told
otherwise, so the split is done around it:

- The daemon's `PATH` starts with `/usr/local/libexec/pod-agents`, which holds
  a hard link of `pod-supervisor` named like each installed agent CLI
  (`AGENT_COMMANDS` build argument, default `claude`). The daemon finds and
  runs that "CLI". Run under an agent's name the binary is a shim: it sends its
  arguments, working directory, environment and stdin/stdout/stderr
  (`SCM_RIGHTS`) to the runner over an abstract unix socket (a pod has one
  network namespace, so both containers see it), and the runner runs the real
  CLI, found on the `PATH` without the shim directory, in the agent container in
  its own process group. The shim relays signals and exits with the agent's
  status. If the shim dies (the daemon kills its process group) the agent and
  its process tree are killed.
- A file that the daemon names in an agent's arguments and keeps in its private
  `TMPDIR` (the task's MCP configuration) is read by the shim and written by the
  runner as a copy in the shared `/tmp`, and the argument is rewritten to the
  copy; the copy is removed when the agent ends.
- The runner answers only a peer with uid 1001 (`SO_PEERCRED`), on both its
  sockets: the launch socket and the control socket the supervisor uses
  (`close`, `open`, `status`). The agent, uid 1000, is turned away. The names
  are fixed in the image (`POD_AGENT_SOCKET`), so the check, not secrecy of the
  name, is the protection.
- A reset is the supervisor's: it stops the daemon, has the runner **close**
  (refuse launches, kill every process in the agent container, empty its `HOME`
  and what it can of the shared volumes, restore the `HOME` template), kills the
  daemon's remaining processes, empties the shared volumes and the daemon's
  `HOME`, restores the daemon's template, starts the daemon and has the runner
  **open**. A runner that restarted on its own comes back closed and the
  supervisor opens it again within five seconds while the daemon runs. If the
  runner cannot be reached the reset fails and is retried like any failed
  reset; at start the supervisor waits up to two minutes for the runner, then
  exits so the kubelet restarts the container.
- `git` trusts both users (`safe.directory *`, `core.sharedRepository group`).
  Before launching, the shim opens the files the daemon created in the task
  directory to the shared group.

What the agent cannot do: see the daemon's processes (separate process
namespaces: no `/proc/<daemon>`, no signals, no `ptrace`), read the daemon's
`HOME` or config (a volume the agent container does not have), or use the
runner (uid check). Checked against the cluster by the pool e2e scenarios.

### Residual risks

The split is not a full isolation of the daemon, because the daemon is unchanged:

1. **The daemon runs code that the agent can influence.** The daemon and the
   agent share the task files and the bare repository cache (`.repos`), which
   the agent must be able to write for commits to work. The daemon runs `git`
   there as uid 1001. An agent can plant a repository config (`core.fsmonitor`,
   a filter driver, an alias, `core.sshCommand`) or a hook there and have it
   executed as the daemon the next time the daemon runs git in that repository,
   for example in `multica repo checkout`. That gives the agent uid 1001 inside
   the daemon container: the daemon's `HOME` and process, the supervisor
   (same user), and with it the runner's control socket, so it can close the
   runner and kill its own pod's agents. What it reaches is the placeholder
   token and the manager address, neither of which is a secret (anything in the
   pod can already call the manager from the pod's IP). It has no root and no
   capability, and does not reach any credential of the manager, LiteLLM or
   GitLab. A fix needs the daemon to run git as the agent user or to keep the
   cache out of the agent's reach, i.e. a daemon change.
2. **Daemon network surface.** The containers share the pod's network
   namespace, so the agent can reach the daemon's local HTTP listener (the
   `multica repo checkout` endpoint, authorised by the task token) and the
   manager. The manager already treats the pod's IP, not a secret, as the
   identity. Any process in the pod can also open an abstract socket, but the
   runner's two are bound first and uid-checked.
3. **Files created later by the daemon.** Files that the daemon creates itself
   (not through `git`) in a task directory after the agent was started follow
   umask 007, so they are group 2000 and usable by the agent; ones whose mode
   the daemon sets explicitly (0755, 0644) are not writable by the agent.
   Directories and files created before launch are opened to the group by the
   shim.
4. **Workspace-level boundary.** All task directories in a shared pod are
   readable by both users (decision B in the spec); the split does not
   separate tasks from each other.
5. **Agent CLIs not shimmed.** Only the names in `AGENT_COMMANDS` run in the
   agent container. Anything else the daemon starts by itself (its own `git`,
   helper processes) runs in the daemon container as uid 1001, and an agent CLI
   that is missing from `AGENT_COMMANDS` but installed would run there. Keep
   the list equal to the installed CLIs.
6. **Same-user processes.** Processes of one task can still signal and ptrace
   each other and those of other tasks in a shared pod, as before.
7. **Shared volumes are group-writable.** Both users can delete each other's
   files in `/work`, `/tmp` and `/dev/shm` (no sticky bit: the volumes belong
   to the kubelet). The daemon's private files are in `/home/daemon`, not there.

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

## Agent CLIs in the image

The image carries Claude Code and opencode (1.19.5, the last 1.x release). Both
are listed in `AGENT_PACKAGES` and `AGENT_COMMANDS` of `Dockerfile.pool`; each
command gets a shim, so the daemon starts it in the agent container.

opencode reaches LiteLLM through the provider in
`deploy/sandbox/home/.config/opencode/opencode.json`, which reads
`OPENAI_BASE_URL` and `OPENAI_API_KEY` from the task's environment (the manager
sets them per task). Only that provider is enabled and it names only the models
LiteLLM serves, so the model picker of an opencode agent shows exactly those
(`litellm/<model>`). To offer another model, add it to the file and to
`SANDBOX_LITELLM_MODELS` of the manager.

Auto-update and the model catalog download are switched off
(`OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_MODELS_FETCH`): the pods'
network policy allows neither.
