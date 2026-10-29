# Managed sandbox runtime

Members see one cloud runtime. Behind it, `sandbox-broker` runs each agent's
tasks in its own sandbox, and each sandbox runs the stock `multica daemon`.
Agents therefore have the same features as on a local runtime, while the
image's own skills and MCP servers stay hidden from the Multica UI.

```
Multica server ──/api/daemon/*── sandbox-broker ──provider API── sandbox (multica daemon + agent CLIs)
                 (broker PAT)        ▲                                │
                                     └──── daemon and agent traffic ──┘
```

- Upstream, the broker is a daemon: it registers `runtime_mode: "cloud"`
  runtimes, heartbeats, claims tasks and extends their prepare lease until a
  sandbox starts them.
- Downstream, it acts as the Multica server for sandbox daemons. Register,
  heartbeat and claim are answered locally. Task calls are forwarded with the
  broker's token after checking that the task belongs to that sandbox. Other
  `/api/` calls (agents using their `mat_` task token) pass through
  unchanged. Endpoints the broker does not know are refused and logged as
  `refused sandbox request`, which is how a newer daemon's protocol changes
  show up.
- The first time, and after every image change, the broker starts a `control`
  sandbox to learn the daemon's registration and capability headers, then
  replays them upstream. The control sandbox also answers model-list requests.
- `SANDBOX_ISOLATION` decides which tasks share a sandbox, and with it a
  filesystem, processes and agent session store:

  | Value | Sandbox per | Resume | Deleted when |
  | --- | --- | --- | --- |
  | `issue` (default) | agent and issue, chat session or autopilot run | Within that issue or chat | It is done, cancelled, archived or deleted, after `SANDBOX_CLEANUP_GRACE` |
  | `task` | task | None | The task ends |
  | `agent` | agent | Across all of the agent's tasks | Unused for `SANDBOX_VOLUME_TTL` |

  Tasks without an issue, chat or autopilot run get their own sandbox in
  `issue` mode. Compute stops after `SANDBOX_IDLE_TIMEOUT` without tasks and
  the volume is kept until the rule above deletes both. Any sandbox unused
  for `SANDBOX_VOLUME_TTL` is deleted regardless of mode, and a sandbox with
  an unfinished task is never deleted.

## Try it locally

[`compose.yaml`](compose.yaml) runs the whole stack from this checkout: its
own Postgres, backend, web, the broker and Docker sandboxes. It uses its own
project name, network and ports, so it does not touch `make up` environments.

```bash
docker compose -f deploy/sandbox/compose.yaml up -d --build
```

Open http://localhost:3310 and sign in as `admin@localhost` with code
`888888`. On first start the broker signs in as that user, creates the
`Sandbox Demo` workspace and a PAT, and registers the **Managed Sandbox**
runtime. Create an agent on that runtime and assign it an issue. Each issue
gets its own `multica-sbx-compose-issue-…` container (`docker ps`), which
stops after 2 minutes idle and is deleted 1 minute after the issue is done.

By default the sandbox runs a fake Claude Code that comments on the issue and
names the hidden image skill it can see, so no LLM credentials are needed. For
real Claude Code, put one credential in `deploy/sandbox/.env` (git-ignored):
`ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`), or
`ANTHROPIC_BASE_URL` with `ANTHROPIC_AUTH_TOKEN` for a gateway. Then:

```bash
SANDBOX_AGENT=agents docker compose -f deploy/sandbox/compose.yaml up -d --build
```

The image ships the example hidden skill `sandbox-environment`; ask an agent
which environment it runs in to see it used. `SANDBOX_ISOLATION`,
`SANDBOX_IDLE_TIMEOUT` and `SANDBOX_CLEANUP_GRACE` are also read from the
environment. Watch the broker with
`docker compose -f deploy/sandbox/compose.yaml logs -f broker`.

To remove everything, delete the sandboxes first (the broker creates them
outside the compose project), then the stack:

```bash
docker ps -aq --filter label=ai.multica.sandbox.instance=compose | xargs -r docker rm -f
```

```bash
docker volume ls -q --filter label=ai.multica.sandbox.instance=compose | xargs -r docker volume rm
```

```bash
docker compose -f deploy/sandbox/compose.yaml down -v
```

## Build and run

Build the image and the broker from the same commit:

```bash
docker build -f deploy/sandbox/Dockerfile -t registry.example/multica-sandbox:1 .
```

```bash
docker build -f deploy/sandbox/Dockerfile.broker -t registry.example/multica-sandbox-broker:1 .
```

The broker image's entrypoint only bootstraps credentials when
`MULTICA_TOKEN` is unset and `SANDBOX_BOOTSTRAP_EMAIL` is set (the compose
demo); in production pass `MULTICA_TOKEN` and `MULTICA_WORKSPACE_ID`.

| Variable | Meaning |
| --- | --- |
| `MULTICA_SERVER_URL`, `MULTICA_TOKEN`, `MULTICA_WORKSPACE_ID` | Server, PAT of the account that owns the runtime, workspace |
| `SANDBOX_IMAGE` | Sandbox image (changing it rolls sandboxes over between runs) |
| `SANDBOX_BROKER_URL` | URL sandboxes use to reach the broker |
| `SANDBOX_BROKER_LISTEN` | Sandbox-facing listen address (default `:8790`) |
| `SANDBOX_BROKER_STATE_DIR` | Private persistent directory for the journal |
| `SANDBOX_BROKER_NAME`, `SANDBOX_BROKER_ID` | Runtime name; stable upstream daemon ID (derived by default) |
| `SANDBOX_PROVIDERS` | Comma-separated providers to expose, e.g. `claude` (default: all detected) |
| `SANDBOX_MAX_CONCURRENCY` | Tasks in flight across sandboxes (default 4) |
| `SANDBOX_IDLE_TIMEOUT`, `SANDBOX_START_TIMEOUT` | Default `10m` each |
| `SANDBOX_ISOLATION` | `issue` (default), `task` or `agent` |
| `SANDBOX_CLEANUP_GRACE`, `SANDBOX_VOLUME_TTL` | Default `1h` and `168h` |
| `SANDBOX_ENV_<NAME>` | Passed to every sandbox as `<NAME>`, e.g. LLM gateway settings |
| `SANDBOX_PROVIDER` | `docker` (default) |
| `DOCKER_SOCKET`, `SANDBOX_DOCKER_INSTANCE`, `SANDBOX_DOCKER_NETWORK`, `SANDBOX_DOCKER_EXTRA_HOSTS`, `SANDBOX_DOCKER_RUNTIME`, `SANDBOX_MEMORY_MB`, `SANDBOX_CPUS` | Docker provider settings; set `SANDBOX_DOCKER_RUNTIME=runsc` for gVisor |

Put runtime-private skills, MCP servers and CLI settings under
[`home/`](home/README.md).

## Security notes

- The broker's PAT never enters a sandbox. A sandbox gets a broker-local
  credential that can only act on that sandbox's own tasks. The agent can read
  this credential because it runs as the same user as the daemon.
- The journal holds claim payloads (with task tokens) and sandbox
  credentials. Keep `SANDBOX_BROKER_STATE_DIR` private.
- Restrict sandbox egress to the broker, git hosts and the LLM gateway.

## Verify

With `make up C=api` running in this checkout:

```bash
bash deploy/sandbox/e2e/run.sh
```

The test starts a real broker and real Docker sandboxes, with a fake Claude
Code. It checks that the runtime appears as `cloud`, that image skills are
reported unsupported, that a task completes, that the agent reached Multica
through the broker and saw the hidden skill, that the idle issue sandbox
stops and is deleted with its volume once the issue is done, and that the
daemon called no endpoint the broker does not support. Rerun it
whenever you merge a new upstream version.

## Not yet covered

Kubernetes provider, warm pools, per-sandbox LLM keys, and serving more than
one workspace from one broker.
