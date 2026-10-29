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
- Each agent gets its own sandbox and volume, so its next task reuses
  workdirs and agent sessions. Compute stops after `SANDBOX_IDLE_TIMEOUT`
  without tasks, and the volume is kept.

## Build and run

Build the image and the broker from the same commit:

```bash
docker build -f deploy/sandbox/Dockerfile -t registry.example/multica-sandbox:1 .
```

```bash
cd server && go build -o bin/sandbox-broker ./cmd/sandbox-broker
```

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
through the broker and saw the hidden skill, that idle sandboxes stop, and
that the daemon called no endpoint the broker does not support. Rerun it
whenever you merge a new upstream version.

## Not yet covered

Kubernetes provider, warm pools, per-sandbox LLM keys, and serving more than
one workspace from one broker.
