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

## Operating sandboxes separately

When another team runs the sandboxes, the broker talks to their service over
the v1 HTTP API in [CONTRACT.md](CONTRACT.md) (`SANDBOX_PROVIDER=http`). The
contract also covers the runtime, network, image and operations agreements.
Their service is accepted when `go run ./cmd/sandbox-conformance` passes
against it with the probe image from [`conformance/`](conformance/); see
section 9 of the contract.

## Try it locally

[`compose.yaml`](compose.yaml) runs the whole stack: its own Postgres, the
backend and web images, the broker and Docker sandboxes. It builds the broker,
sandbox and egress images from this checkout and uses its own project name,
network and ports.

Create `deploy/sandbox/.env` (git-ignored) with:

| Variable | Value |
| --- | --- |
| `MULTICA_BACKEND_IMAGE`, `MULTICA_WEB_IMAGE` | Backend and web images built from this commit |
| `SANDBOX_COMPOSE_DB_PASSWORD`, `SANDBOX_COMPOSE_JWT_SECRET` | Random values, e.g. from `openssl rand -hex 32` |
| `SANDBOX_COMPOSE_DEV_CODE` | Any six digits: the fixed sign-in code for this local stack |

```bash
docker compose -f deploy/sandbox/compose.yaml up -d --build
```

Open http://localhost:3310 and sign in as `admin@localhost` with
`SANDBOX_COMPOSE_DEV_CODE`. On first start the broker signs in as that user, creates the
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
| `SANDBOX_PROVIDER` | `docker` (default) or `http` for a separately operated sandbox service |
| `SANDBOX_SERVICE_URL`, `SANDBOX_SERVICE_TOKEN` | The sandbox service with `SANDBOX_PROVIDER=http`; see [CONTRACT.md](CONTRACT.md) |
| `DOCKER_HOST` (`tcp://…`) or `DOCKER_SOCKET` | Docker API: a socket proxy (recommended) or the socket path |
| `SANDBOX_DOCKER_INSTANCE`, `SANDBOX_DOCKER_NETWORK`, `SANDBOX_DOCKER_EXTRA_HOSTS` | Resource names and the network sandboxes join |
| `SANDBOX_DOCKER_RUNTIME` | OCI runtime, e.g. `runsc` for gVisor |
| `SANDBOX_MEMORY_MB`, `SANDBOX_CPUS`, `SANDBOX_PIDS_LIMIT` | Per-sandbox limits; default `4096`, `2`, `1024`, negative removes the limit |
| `SANDBOX_WRITABLE_ROOTFS` | `true` for images that must write outside `/data` and `/tmp` |

Put runtime-private skills, MCP servers and CLI settings under
[`home/`](home/README.md).

## Security notes

Every sandbox container runs as a non-root user with all capabilities
dropped, `no-new-privileges`, a read-only root filesystem (writable `/data`
volume and a 1 GB `/tmp` tmpfs), and memory, CPU and process limits. HOME is
`/data/home`; files under `home/` are copied into it at every start. A change
to any of these settings, the network or the environment replaces a
sandbox's container on its next start and keeps its volume.

The compose stack adds two network boundaries:

- **Docker API proxy.** The broker reaches Docker only through
  `tecnativa/docker-socket-proxy`, which allows containers, volumes and image
  pulls and refuses exec, build, info and everything else. It cannot inspect
  request bodies, so a compromised broker could still ask for a privileged
  container: treat the broker as trusted, or run it against rootless Docker.
- **Egress allowlist.** Sandboxes sit on an `internal` network with no route
  out. They reach the broker directly and everything else through the
  `egress` proxy, which allows only the domains in `SANDBOX_EGRESS_ALLOW`
  (and their subdomains) and only port 443 for HTTPS. Add your LLM gateway
  when you set `ANTHROPIC_BASE_URL`. Git over SSH is not available; use
  HTTPS remotes.

Also:

- The broker's PAT never enters a sandbox. A sandbox gets a broker-local
  credential that can only act on that sandbox's own tasks. The agent can read
  this credential because it runs as the same user as the daemon.
- The journal holds claim payloads (with task tokens) and sandbox
  credentials. Keep `SANDBOX_BROKER_STATE_DIR` private.

## Verify

With the compose stack running, assign an issue to an agent on the Managed
Sandbox runtime and check that it completes, that the agent can name the
hidden `sandbox-environment` skill, and that the broker log shows no
`refused sandbox request` (which would mean the daemon used an endpoint the
broker does not support). Repeat after every upgrade to a new Multica version.

## Not yet covered

Kubernetes provider, warm pools, per-sandbox LLM keys, and serving more than
one workspace from one broker.
