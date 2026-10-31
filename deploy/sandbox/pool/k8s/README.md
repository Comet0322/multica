# Pool manifests and manual smoke test

Plain YAML for the sandbox pod pool (kustomize-friendly, no secrets).

| File | What |
|---|---|
| `kustomization.yaml` | Pool size (`replicas`) and image tags, version-controlled. Every `PLACEHOLDER` is replaced in an overlay |
| `namespace.yaml` | `multica-sandbox` namespace (Pod Security `baseline`: the pod's PID 1 is root with six capabilities) |
| `pool.yaml` | Pod pool Deployment: no `hostNetwork`, read-only root, three `emptyDir` areas (`/work`, `/home/agent`, `/tmp`), all capabilities dropped except CHOWN, DAC_OVERRIDE, FOWNER, KILL, SETUID, SETGID, no privilege escalation, no service account token. Matches `../verify.sh` |
| `manager.yaml` | Manager Deployment (1 replica) and a ClusterIP Service exposing the pod listener (`8080`) only. The admin listener (`8791`) is not in the Service |
| `networkpolicy.yaml` | Pod egress limited to manager, DNS, LiteLLM, git gateway, package mirror; manager pod listener reachable only from pool pods |

The pool image's manager address and control URL (`<manager>/pod/control`) are
baked in at build time (`--build-arg MANAGER_URL=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080`).
The manager's env settings come from `server/cmd/sandbox-broker/pool.go`; the
admin listener binds to loopback inside the manager pod (reach it with
`kubectl port-forward`), and the workspaces file is mounted from a Secret you
create out of band.

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
   - `curl -fsSI <git gateway URL>`
   - `curl -fsSI <package mirror URL>/pypi/simple/`
3. **Egress denied** (each must time out or be refused; use `--max-time 5`):
   - `curl --max-time 5 https://example.com` (public internet)
   - `curl --max-time 5 <GitLab URL>` (direct GitLab, bypassing the gateway)
   - `curl --max-time 5 http://<manager-ip>:8791/v1/inventory` (admin listener; must fail even if the policy were loosened, as it binds to loopback)
   - `curl --max-time 5 http://<another pool pod IP>:<any port>` (pod to pod)
4. **Manager only reachable from the pool.** From a throwaway pod WITHOUT the
   `multica.io/sandbox-role=pool-pod` label in the same namespace:
   `curl --max-time 5 http://$MGR/healthz` must fail.
5. **Source IP / SNAT check.** Pods register and the manager logs no
   "registration rejected: pod reports IP ... the source address is being rewritten" lines; `GET /v1/inventory` (via
   `kubectl port-forward deploy/sandbox-manager 8791:8791`) lists every pod
   with its own pod IP (`kubectl -n $NS get pod -o wide`), none showing a node
   or gateway IP. A mismatch means SNAT or a mesh rewrites addresses: fix the
   CNI/mesh before continuing.
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
