# Runtime controller

Runs one Kubernetes [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) per managed
runtime. It wakes a Sandbox when a task queues for it, suspends it after `IDLE_SUSPEND_AFTER`
without work, rotates its daemon token, mints a per-Sandbox LiteLLM key, and removes everything
when the runtime is deleted. It reaches the Multica server only through `/internal/v1`.

Design and rollout: `deploy/k8s-poc/CHANGE-PLAN.md` (Workstream E). Code: `server/internal/runtimecontroller`.

## Configure

| Variable | Default | Meaning |
|---|---|---|
| `MULTICA_SERVER_URL` | required | server base URL as the controller reaches it |
| `MULTICA_CONTROLLER_SECRET` | required | must equal the server's `MULTICA_CONTROLLER_SECRET` (the server's `/internal/v1` is disabled when unset) |
| `SANDBOX_IMAGE` | required | image with `/app/multica` and the agent CLIs |
| `SANDBOX_SERVER_URL` | `MULTICA_SERVER_URL` | server URL as Sandbox pods reach it |
| `CONTROLLER_NAMESPACE` | `default` | namespace for Sandboxes and Secrets |
| `IDLE_SUSPEND_AFTER` | `15m` | suspend a Sandbox with no queued or active task this long |
| `TOKEN_TTL` | `24h` | daemon token lifetime (server cap 24h); rotated at half |
| `STUCK_AFTER` | `5m` | Running but daemon not online this long => reported unavailable |
| `MULTICA_DAEMON_DRAIN_TIMEOUT` | `10m` | daemon drain window; pod grace period = this + 30s |
| `SANDBOX_RUNTIME_CLASS` | unset | e.g. `gvisor` / `kata` |
| `SANDBOX_PVC_SIZE` | `5Gi` | per-Sandbox volume (workdir, agent home, sessions) |
| `LITELLM_URL`, `LITELLM_ADMIN_KEY` | unset | enables per-Sandbox LLM keys; the admin key stays in the controller only |
| `LITELLM_MODELS`, `LITELLM_MAX_BUDGET`, `LITELLM_RPM` | unset | limits on each minted key |
| `CONTROLLER_LEADER_ELECT` | `true` | Lease-based leader election |
| `KUBECONFIG` + `KUBE_CONTEXT` | in-cluster | out-of-cluster only; the context must be named |

Server side: set `MULTICA_CONTROLLER_SECRET`, and leave `DAEMON_AUTHZ_MODE` at `log` (managed runtimes
enforce regardless).

## Deploy

```sh
kubectl -n <ns> create secret generic runtime-controller --from-literal=controller-secret=<secret>
kubectl -n <ns> apply -f controller.yaml -f sandbox-networkpolicy.yaml
```

## Operating notes

- A Sandbox's PVC is retained across suspend. Deleting a managed runtime removes the Sandbox, its
  Secrets, its tokens and its LiteLLM key.
- If the controller stops, the server stops showing managed runtimes as available within about two
  minutes (stale state fails closed); running tasks continue.
- The LLM key reaches the pod as environment (`ANTHROPIC_*`, `OPENAI_*`) of the daemon, so the agent can read
  it. Its blast radius is that one key's budget, model list and rate limit.
