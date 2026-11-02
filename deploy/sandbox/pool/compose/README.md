# The pool on a Docker host (no Kubernetes)

`stack.yaml` (manager and git gateway) and `pool.yaml` (the pods) run the pool
on one Docker host, for example a VM. A pod is one container of the same image
the Kubernetes pool uses; the replicas of one service are the pods.

```bash
cp sandbox.env.example sandbox.env        # fill in the values (see stack.yaml)
mkdir secrets                             # pool-token, litellm-master-key, gitlab-token;
                                          # mode 0440, group or owner 65532
docker compose --env-file sandbox.env -f stack.yaml -f pool.yaml up -d --scale pod=4
docker compose exec manager wget -qO- http://127.0.0.1:8791/v1/inventory
```

Keep `POOL_SIZE` in `sandbox.env` at the same number as `--scale`. Each replica
has its own network namespace, address, anonymous `/work` and `/home/pod`
volumes and tmpfs `/tmp` and `/dev/shm`, so nothing is shared between
replicas. Change the size with the same command and a new `--scale`. What the
single user costs is in `../../POOL.md` ("The pod").

## What Kubernetes does for a pod that Docker does not

| Kubernetes gives the pod | On plain Docker | Here |
| --- | --- | --- |
| `fsGroup: 2000`: the volumes are group 2000 | A new named volume copies the image's ownership, which was root's | The image's `/work`, `/tmp` and `/home/pod` are owned `1001:2000`; tmpfs is mounted with `uid=1001,gid=2000` |
| `emptyDir` of medium Memory for `/dev/shm` | A container's `/dev/shm` is sticky and world-writable | `tmpfs` with the pod's user and group |
| Cluster DNS names baked into the image | Not resolvable | Network aliases with the same names on the manager and the gateway |
| `replicas` | `--scale` | `--scale pod=N` |
| kubelet restarts an exited container | `restart: always` | Set on the pod |
| `podPidsLimit` of the kubelet | None unless set | `deploy.resources.limits.pids: 1024` in `pool.yaml` |
| NetworkPolicy | None | See below |

## Network isolation

- The manager's gateway listener (8792) answers only on the `control` network,
  which holds the manager and the gateway; pods cannot reach it. (Kubernetes
  does this with a NetworkPolicy; the endpoints are unauthenticated.)
- Pods can reach the manager (8080) and the gateway (8081) by name.
- The pods' network allows egress by default. Set `POD_NETWORK_INTERNAL=true`
  to cut pods off from everything else; LiteLLM and the package mirror must
  then be reachable on that network (attach them to `multica-sandbox`).
- Pods on one bridge can reach each other; there is no per-pod policy as on a
  cluster with NetworkPolicy. For tasks that must not meet, use one host or
  one network per trust group.
- A pod is identified by its source address. Reach the manager through the
  network alias, never through a published host port: the userland proxy
  rewrites the source and every pod then looks like one.

## Process limit

`pool.yaml` limits a pod to 1024 processes and threads. Without a limit a task
that forks without end fills the host's process table. The supervisor (PID 1)
starts 32 threads at launch and keeps them: when the limit is reached it could
not start a new thread, and the Go runtime would abort the supervisor and with
it the container.

## Operating

- Limits come from the `deploy.resources` values; Docker applies them without Swarm.
- A pod's volumes live until `docker compose down -v`.

## Checked

Run on Docker 28 / Compose 2.39 with three pods, against a real Multica, LiteLLM
and GitLab (the same dev stack the Kubernetes version is tested on). All 19
end-to-end scenarios that apply to Compose pass: tasks from a non-owner,
workspace add and remove, SIGHUP, queueing, cancel, deleted task, daemon
killed, repository access control, `/dev/shm` clearing, orphan processes after
a reset, no root and no capabilities, a real clone, push and merge request
through the gateway with the initiator's identity, budget exhaustion, model
list, opencode (with and without git), no long-lived secrets in a pod, runtime
deleted, and quiet logs. The four hostile-task scenarios pass as well: ptrace
and signals at PID 1, `kill(-1)`, `SIGTERM`, a fork bomb, and planted files and
processes, each followed by tasks that complete on the same pool.

Not run on Compose, because they need cluster features (rolling a Deployment,
revoking the bot PAT, a second Multica workspace): bad PAT at startup, manager
restart in the middle of a task, drain with a running task, PAT revoked while
running, two workspaces.

The run found two defects. Without a process limit a fork bomb fills the host's
process table (`pids` in `pool.yaml`). With the limit, the supervisor aborted
when it could not start a thread (the reserved threads).
