# Pool manifests and manual smoke test

Plain YAML for the sandbox pod pool (kustomize-friendly, no secrets).

| File | What |
|---|---|
| `kustomization.yaml` | Pool size (`replicas`) and image tags, version-controlled. Every `PLACEHOLDER` is replaced in an overlay |
| `namespace.yaml` | `multica-sandbox` namespace (Pod Security `baseline`: the pod's PID 1 is root with six capabilities) |
| `pool.yaml` | Pod pool Deployment: no `hostNetwork`, read-only root, three `emptyDir` areas (`/work`, `/home/agent`, `/tmp`), all capabilities dropped except CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETUID, SETGID, no privilege escalation, no service account token. Matches `../verify.sh` |
| `manager.yaml` | Manager Deployment (1 replica) with its environment (Multica URL, LiteLLM, git gateway, audit to stdout), a ClusterIP Service exposing the pod listener (`8080`) only, and a second Service (`sandbox-manager-gateway`, `8792`) for the gateway listener that serves only the git verify endpoints. The admin listener (`8791`) is in neither Service |
| `git-gateway.yaml` | Git/MCP gateway Deployment (1 replica: it keeps the pushed-branch record in memory) and its ClusterIP Service (`8081`). It reaches the manager through `sandbox-manager-gateway` |
| `networkpolicy.yaml` | Pod egress limited to manager (`8080`), git gateway (`8081`), DNS, LiteLLM and the package mirror; in-cluster services are matched by pod labels, external ones by `ipBlock` placeholders. The manager's pod listener is reachable only from pool pods, its gateway listener only from the gateway; the gateway only from pool pods |

The pool image's manager address and control URL (`<manager>/pod/control`) are
baked in at build time (`--build-arg MANAGER_URL=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080`),
and so is the gateway root (`--build-arg GIT_GATEWAY_URL=http://git-gateway.multica-sandbox.svc.cluster.local:8081/`).
The manager's env settings are described in `../../POOL.md`; the
admin listener binds to loopback inside the manager pod (reach it with
`kubectl port-forward`). Two Secrets are created out of band and are not in
this repo: `sandbox-manager-config` (keys `workspaces.json`,
`litellm-master-key`) and `git-gateway-gitlab-token` (key `token`, the GitLab
administrator token). The images are `multica-sandbox-pool`
(`../../Dockerfile.pool`), `multica-sandbox-manager` (`../../Dockerfile.broker`)
and `multica-git-gateway` (`../../Dockerfile.gateway`).

Offline check (no cluster): `kubectl kustomize deploy/sandbox/pool/k8s`.

## Manual smoke checklist (real cluster)

Nothing here is automated. Record the results in ticket 20.
Set `NS=multica-sandbox`, `POD=$(kubectl -n $NS get pod -l multica.io/sandbox-role=pool-pod -o name | head -1)`,
`MGR=<manager pod-listener ClusterIP>:8080`.

1. **Deployed declaratively.** `kubectl apply -k` an overlay with real images, CIDRs and
   replicas. `kubectl -n $NS get deploy sandbox-pool` shows the replica count
   from git; `kubectl -n $NS get svc sandbox-manager` is `ClusterIP`, with port
   8080 only; `kubectl -n $NS get pod -o jsonpath='{..hostNetwork}'` prints nothing/false.
2. **Egress allowed** (from a pool pod, `kubectl -n $NS exec $POD -- ...`):
   - `curl -fsS http://$MGR/healthz`
   - `curl -fsSI <LiteLLM URL>`
   - `curl -fsSI http://git-gateway:8081/` (any answer, even 401, proves the path)
   - `curl -fsSI <package mirror URL>/pypi/simple/`
3. **Egress denied** (each must time out or be refused; use `--max-time 5`):
   - `curl --max-time 5 https://example.com` (public internet)
   - `curl --max-time 5 <GitLab URL>` (direct GitLab, bypassing the gateway)
   - `curl --max-time 5 http://<manager-ip>:8791/v1/inventory` (admin listener; must fail even if the policy were loosened, as it binds to loopback)
   - `curl --max-time 5 -X POST http://sandbox-manager-gateway:8792/v1/git/verify-pod -d '{"ip":"10.0.0.1"}'` (gateway listener: pods must not reach it)
   - `curl --max-time 5 http://<another pool pod IP>:<any port>` (pod to pod)
4. **Manager only reachable from the pool.** From a throwaway pod WITHOUT the
   `multica.io/sandbox-role=pool-pod` label in the same namespace:
   `curl --max-time 5 http://$MGR/healthz` must fail, and so must
   `curl --max-time 5 http://sandbox-manager-gateway:8792/v1/git/verify-pod`
   and `curl --max-time 5 http://git-gateway:8081/`. From a pod of the git
   gateway, the manager's gateway listener answers
   (`curl -X POST .../v1/git/verify-pod -d '{"ip":"10.0.0.1"}'` gives
   `{"identities":[]}`) and its pod listener (`8080`) does not.
5. **Source IP / SNAT check.** The stock daemon does not send `pod_ip`, so the
   manager cannot detect a rewritten source address itself: this manual step
   is the check. `GET /v1/inventory` (via
   `kubectl port-forward deploy/sandbox-manager 8791:8791`) lists every pod
   with its own pod IP (`kubectl -n $NS get pod -o wide`), none showing a node
   or gateway IP. A mismatch means SNAT or a mesh rewrites addresses: fix the
   CNI/mesh before continuing.
   (If a daemon that sends `pod_ip` is ever used, the manager logs and
   rejects mismatches with "registration rejected: pod reports IP ...".) Also
   note that each pod passes through `resetting` once after the manager
   starts, because a pod it has not seen is reset before it is lent.
6. **Fake task: lease, release, reset.** Without LLM or git, run a fake task
   (the `fake` image target's agent) through the manager. Watch
   `curl -s localhost:8791/v1/inventory` and `/v1/leases`: pod goes idle ->
   leased (task id and initiator visible) -> resetting -> idle. After the reset
   confirm in the pod: `kubectl -n $NS exec $POD -- ls /work /home/agent /tmp`
   shows only the rebuilt HOME template, and only `pod-supervisor` plus
   `multica` processes remain.
7. **Inventory matches the cluster.** Idle + leased + resetting + lost equals
   the expected size, and the pod IPs in `/v1/inventory` equal
   `kubectl -n $NS get pod -l multica.io/sandbox-role=pool-pod -o wide`. Delete one
   pool pod: inventory shows it lost, then the replacement registers under a
   new IP and the count recovers.
8. **Git through the gateway.** In a pool pod, with a leased task's
   `GIT_GATEWAY_TOKEN` in the agent environment, `git ls-remote <GitLab project
   URL>` succeeds (the system gitconfig rewrites it to the gateway), `git push`
   to `agent/<task id>/x` succeeds and any other ref is refused with the
   gateway's reason, and `git credential fill` for a host other than the
   gateway returns no password.

## Notes from the minikube smoke run (ticket 20)

- An overlay's `images:` entries must match the names the base kustomization
  already produced (`registry.example.com/multica/...`), not the short names in
  the Deployments.
- The manager reads `workspaces.json` from a Secret volume, which is
  group-readable under `fsGroup`; the manifest sets
  `SANDBOX_WORKSPACES_FILE_GROUP_READ=true` and `defaultMode: 0440` for that.
- `../smoke/` holds the stand-ins (fake Multica, LiteLLM, a destination that
  must stay unreachable, a package mirror) and an overlay used for a local
  minikube run with Calico. Fill in the stand-in pod IPs as the `LITELLM_IP`
  and `MIRROR_IP` markers before applying it.
- Lost pods stay in the inventory as `lost` for `SANDBOX_POOL_LOST_RETENTION`
  (default 10m) and are then pruned (`pruned_total` in the inventory counts
  them), so after pod replacement the count can exceed the declared size only
  until the retention runs out.
