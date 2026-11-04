# daemonbridge

Presents the Multica daemon protocol to a Multica server and runs the claimed
tasks on a pluggable `Backend` instead of a local agent CLI.

```
Multica server <-- daemon protocol --> Bridge <-- Backend interface --> engine
                                         |
                                         +-- multica CLI as a task-scoped tool
```

The bridge owns registration, heartbeats, claim, start, message streaming,
usage, cancellation, supplements, completion and failure. A backend only runs a
rendered brief and reports events.

## What the bridge reuses from the local daemon

- `daemon.Client` for every server call, so wire formats cannot drift.
- `daemon.RenderBrief` for the runtime brief and prompt, so agents see the
  same instructions, command reference and issue context as on a local daemon.
- The real `multica` CLI (`CLITool`), run with the task-scoped `mat_` token.
  Comments, status changes, issue/project/label/autopilot/squad commands and
  attachments therefore behave exactly as for a local agent, including the
  squad Leader wake-up that only an API comment triggers.

## Guarantees

- Journal: every task is persisted before each step; a restart resumes runs,
  redelivers outcomes and does not call recover-orphans over them.
- Capabilities: only supported client capabilities are advertised.
- Explicit failure: agent configuration the backend cannot honour (custom
  env/args, MCP config, skills, model, thinking, remote MCP, local directory)
  fails the task with a clear message instead of being ignored.
- The task credential never reaches the backend (`Job.Task` has it removed);
  the CLI tool refuses `daemon/auth/login/setup/config/update/runtime` and any
  flag that would change the credentials or server.

## Backends

`oma/` runs each task on Open Managed Agents: one session per task, the task's
MCP bearer in a per-task OMA vault, and a poll of the session event log. The
bridge serves the Multica tools to the session over MCP (`ToolServer`); the
tools are generated from `multica tools-schema`. Configure it with `OMA_URL`,
`OMA_API_KEY`, `OMA_ENVIRONMENT_ID` and `OMA_MODEL`; the OMA environment must
allow MCP servers.

## Not implemented

- Git inside the sandbox (`repo checkout` is unavailable to the run).
- Remote MCP credential resolution and plugin hook tools (`RemoteMCP` is rejected).
- Local directory resources and git worktrees (`LocalWorkDir` is rejected).
- WebSocket wake-up; the bridge polls (`PollInterval`).
