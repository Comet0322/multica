# Sandbox service contract

This is the agreement between the **sandbox broker** (part of Multica) and the
**sandbox service** that runs sandboxes on Docker, Kubernetes or anything
else. The broker decides *when* a sandbox runs; the service decides *how*.
Version: `v1`.

```
Multica server ── sandbox broker ──(this API)──▶ sandbox service ──▶ sandboxes
                       ▲                                                │
                       └──────── sandbox daemons connect out ──────────┘
```

The broker never connects into a sandbox. Each sandbox runs the stock Multica
daemon, which connects out to the broker. The service needs no ingress to
sandboxes, no exec, and no log streaming.

## 1. Responsibilities

| Concern | Broker (Multica) | Sandbox service |
| --- | --- | --- |
| Which sandboxes exist, and when they start, stop and are deleted | Owns | Executes; never acts on its own |
| Sandbox image contents and version | Builds it from the broker's commit | Pulls and runs it |
| Compute, volumes, scheduling, limits, isolation | — | Owns |
| Network boundaries and the egress allowlist | States requirements (section 4) | Implements |
| LLM and git credentials | Passes them as sandbox env | Never logs or persists them |
| Sandbox logs and metrics | — | Collects and gives Multica maintainers access |

## 2. API

Base URL: agreed per environment, e.g. `https://sandboxes.internal/`. All
paths are under `/v1`. Request and response bodies are JSON.

**Authentication.** Every request carries `Authorization: Bearer <token>`. A
token identifies one broker and scopes everything it can see: two brokers
never see or touch each other's sandboxes, even with equal keys. Wrong or
missing token: `401`.

**Keys.** A key names one sandbox: 1–63 characters of `[a-z0-9-]`, not
starting or ending with `-` (a DNS-1123 label). Invalid key: `400`.

**Sandbox object.**

```json
{"key": "issue-1a2b3c", "status": "running", "reason": "", "image": "registry/multica-sandbox:abc123"}
```

| `status` | Meaning |
| --- | --- |
| `pending` | Requested, not running yet (scheduling, pulling, starting) |
| `running` | The container is running |
| `stopped` | No compute is running; the volume is kept |
| `failed` | It cannot run; `reason` says why (image pull failed, OOM, crash) |
| `missing` | No compute exists for this key; a volume may still exist |

`reason` is human-readable and may be empty. `image` is the image the current
compute runs, or empty when there is none. Clients treat an unknown `status`
as `pending`.

### `PUT /v1/sandboxes/{key}` — ensure running

```json
{"image": "registry/multica-sandbox:abc123", "env": {"NAME": "value"}, "labels": {"ai.multica.workspace": "…"}}
```

Make the sandbox run this spec, then return the sandbox object with `200`.

- No sandbox: create the volume and compute, then start it.
- Stopped: start it with the same volume.
- Running with the same spec: do nothing. This must not restart it.
- Different spec (image, env, or the service's own settings): replace the
  compute and **keep the volume**.
- It may return before the sandbox runs (`pending`); the broker polls `GET`.
- Repeated calls are safe, including concurrent ones for the same key.

### `GET /v1/sandboxes/{key}` — status

`200` with the sandbox object. A key with no compute returns
`200 {"key": "…", "status": "missing"}`; `404` is accepted too.

### `POST /v1/sandboxes/{key}/stop` — release compute

Stop the compute and keep the volume. `204`. Idempotent: an already stopped or
missing sandbox also returns `204`. Afterwards `GET` reports `stopped` or
`missing`.

### `DELETE /v1/sandboxes/{key}` — delete everything

Delete the compute **and the volume**. `204`. Idempotent: an unknown key also
returns `204`.

### `GET /v1/sandboxes` — list

`200 {"sandboxes": [<sandbox object>, …]}` with every sandbox, compute or
volume, that this token owns. The broker uses it after restarts.

**Errors.** Non-2xx responses carry `{"error": "<message>"}`. `400` and `401`
are not retried. Other `4xx` and all `5xx` responses are retried on the next
reconcile loop, every few seconds. Each call returns within 30 seconds; long
work continues in the background behind `pending`.

## 3. Sandbox runtime

Each sandbox must provide:

1. **The broker's image and entrypoint.** Do not override the entrypoint or
   command.
2. **Every `env` entry, verbatim.** Pass the whole map without filtering by
   name: the broker adds variables over time, and adding one must not need a
   change to the service. The env contains a sandbox credential and LLM
   credentials, so never log it or show it in a UI that Multica members can
   reach.
3. **A persistent volume at `/data`**, one per key, writable by uid `1000`. It
   survives stop, start and spec replacement, and is removed only by
   `DELETE`. HOME, agent sessions and workdirs live there.
4. **A writable `/tmp`** (at least 1 GB). The root filesystem may be read-only.
5. **Graceful stop:** SIGTERM, then at least 10 seconds before SIGKILL.

Required security baseline: non-root (uid 1000), all capabilities dropped,
no privilege escalation (`no-new-privileges` or
`allowPrivilegeEscalation: false`), the default seccomp profile, no host paths,
no host network, not privileged, and no cloud or Kubernetes credentials
mounted. Recommended: gVisor or Kata isolation, and dedicated hosts or nodes
for sandboxes.

Default limits per sandbox, unless agreed otherwise: 2 CPUs, 4 GB memory,
1024 processes, a 20 GB volume.

## 4. Network

| From the sandbox to | Rule |
| --- | --- |
| The broker URL, e.g. `http://sandbox-broker:8790` | **Allowed** (required) |
| The domains on the agreed allowlist (LLM gateway, git hosts, package registries) | Allowed, directly or through an HTTP(S) proxy |
| Multica backend, databases, company networks, cloud metadata (`169.254.169.254`), the Kubernetes API, other sandboxes | **Denied** |

If egress goes through a proxy, tell the broker operator its URL; the broker
passes `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` (including the broker host)
to sandboxes. Git over SSH is not required.

The broker reaches the service API. Nothing needs to reach into sandboxes.

## 5. Images and rollout

- The broker and the sandbox image are built from the **same Multica commit**,
  because the daemon in the image and the broker share a protocol.
- Image references are **immutable**: a unique tag per build, or a digest. The
  broker detects a new version by the image string changing.
- Rollout: push the image, then restart the broker with the new
  `SANDBOX_IMAGE`. The broker replaces each sandbox's compute between tasks,
  never mid-task.
- The service must be able to pull from the agreed registry.

## 6. Capacity and timing

| Item | Value to agree |
| --- | --- |
| Sandboxes running at once | ≥ the broker's `SANDBOX_MAX_CONCURRENCY` + 1 |
| Stopped sandboxes and volumes kept | Bounded by the broker's cleanup: issue done or chat archived (`SANDBOX_CLEANUP_GRACE`), or unused for `SANDBOX_VOLUME_TTL` (default 7 days) |
| Time from `PUT` to `running` | ≤ 45 s p95. The broker treats longer as a crash and fails tasks not started after 10 minutes |
| Behavior when full | `PUT` returns `pending` (preferred) or `503`; never silently drops the request |

## 7. Lifecycle ownership

- Only the broker stops or deletes sandboxes. The service must not clean them
  up on its own (idle timers, TTLs, evictions).
- If compute disappears anyway (host failure, eviction), `GET` reports
  `missing` or `failed`. The broker re-queues or fails the affected tasks and
  starts the sandbox again.
- Losing a volume loses that issue's workdir and agent session, but no
  Multica data. Backups are optional and must be agreed explicitly.

## 8. Operations

- Every sandbox carries the `labels` from `PUT`. Use them, together with the
  key, in logs and metrics, so a sandbox can be matched to a broker log line.
- Multica maintainers get read access to sandbox logs and status.
- Triage order for a stuck task: broker log (`claimed`, `sandbox requested`,
  `refused sandbox request`), then the sandbox's status and logs in the
  service.
- Each side announces changes to this contract, the API version, or the
  network rules before deploying them.

## 9. Acceptance

The service passes the conformance suite against a real deployment:

```bash
cd server && go run ./cmd/sandbox-conformance \
  -url https://sandboxes.internal -token "$SANDBOX_SERVICE_TOKEN" \
  -image registry/multica-sandbox-probe:<tag> \
  -listen :8799 -callback-url http://<host reachable from sandboxes>:8799
```

Build the probe image with
`docker build -t registry/multica-sandbox-probe:<tag> deploy/sandbox/conformance`.
It stands in for the sandbox image, reports what it sees back to the suite,
and checks sections 2, 3 and part of 4. Rerun the suite after any change on
either side.
