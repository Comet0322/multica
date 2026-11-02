# The pool on a Docker host (no Kubernetes)

`stack.yaml` (manager and git gateway) and `gen-pods.sh` (the pods) run the
pool on one Docker host, for example a VM. They build each pod the way
`../k8s/pool.yaml` does, with the same images.

```bash
cp sandbox.env.example sandbox.env        # fill in the values (see stack.yaml)
mkdir secrets                             # pool-token, litellm-master-key, gitlab-token;
                                          # mode 0440, group or owner 65532
./gen-pods.sh 4 > pods.yaml               # IMAGE=... NETWORK=... override the defaults
docker compose --env-file sandbox.env -f stack.yaml -f pods.yaml up -d
docker compose exec manager wget -qO- http://127.0.0.1:8791/v1/inventory
```

## What Kubernetes does for a pod that Docker does not

The manager and the pod programs need no Kubernetes API. What a straight port
of the manifest loses is behaviour of the pod object itself:

| Kubernetes gives the pod | On plain Docker | Here |
| --- | --- | --- |
| One network namespace that outlives its containers | A container joined with `network_mode: service:X` loses the network when X restarts, and the daemon's supervisor exits on purpose to be restarted | `podN-net` holds the namespace and never restarts; the daemon and the runner join it |
| `fsGroup: 2000`: the volumes are group 2000 and setgid | A new named volume copies the image's ownership, which was root's | The image's `/work`, `/tmp` and the homes are owned `1001:2000` (`1000:2000` for the agent home), mode 2770 |
| `emptyDir` of medium Memory for `/dev/shm`, shared by both containers | Each container has its own `/dev/shm`, sticky and world-writable, so the daemon cannot empty what the agent wrote | One `tmpfs` volume per pod, group-writable, mounted by both |
| Cluster DNS names baked into the image | Not resolvable | Network aliases with the same names on the manager and the gateway |
| `replicas` | `--scale` cannot share a network namespace per replica | `gen-pods.sh N` writes N pods |
| kubelet restarts an exited container | `restart: always` | Set on every pod container |
| NetworkPolicy | None | See below |

Without these, the daemon container restarts in a loop on the first reset
(`prepare directories: removeat ...: permission denied`, from a root-owned
`/tmp`) and the runner fails to start (`cannot join network namespace of
container ... is restarting`).

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

## Operating

- Restarting a pod's `podN-net` container changes its network namespace; also
  restart that pod's two other containers: `docker compose restart podN-daemon podN-runner`.
- Limits come from the `deploy.resources` values; Docker applies them without Swarm.
- A pod's volumes live until `docker compose down -v`.

## Checked

Run on Docker 28 / Compose 2.39 with three pods, against a real Multica, LiteLLM
and GitLab (the same dev stack the Kubernetes version is tested on). Of the 24
end-to-end scenarios, 19 apply to Compose and all 19 pass: tasks from a
non-owner, queueing, cancel, deleted task, daemon killed, repository access
control, `/dev/shm` clearing, orphan processes after a reset, no root and no
capabilities, no secrets in a pod, model list, opencode, a real clone, push and
merge request through the gateway with the initiator's identity, task keys
removed from LiteLLM, and budget exhaustion. Not run, because they need cluster
features (rolling a Deployment, revoking the bot PAT, a second Multica
workspace): bad PAT at startup, manager restart in the middle of a task, drain
with a running task, PAT revoked while running, two workspaces.

The run found one defect, in the pod supervisor and not in Compose: after the
daemon container restarted, a file the agent had left in `/tmp` (mode 0700,
agent uid) made the supervisor exit while preparing directories, and the
container restarted forever. The supervisor now closes the agent runner, which
removes what the agents own, before it cleans the shared areas.
