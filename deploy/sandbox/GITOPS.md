# Deploying the sandbox with Azure DevOps and GitOps

A short guide to running the sandbox manager, the git gateway and the pod pool
through an Azure DevOps pipeline and a GitOps controller. It assumes Kubernetes
(for example AKS), a container registry (for example ACR) and Argo CD or Flux
in the cluster. Replace those with whatever the company uses; the flow is the
same.

## What gets deployed

| Piece | Image | Built from | Manifests |
|---|---|---|---|
| Sandbox manager | `multica-sandbox-manager` | `deploy/sandbox/Dockerfile.manager` | `deploy/sandbox/pool/k8s/manager.yaml` |
| Git gateway | `multica-git-gateway` | `deploy/sandbox/Dockerfile.gateway` | `deploy/sandbox/pool/k8s/git-gateway.yaml` |
| Sandbox pods | `multica-sandbox-pool` | `deploy/sandbox/Dockerfile.pool` | `deploy/sandbox/pool/k8s/pool.yaml` |

`namespace.yaml` and `networkpolicy.yaml` sit next to the manager's manifests.
The manager and gateway images come from the manager branch, the pod image from
the sandbox branch; build all three from the same commit when both branches are
merged.

## Two repositories

- **App repository** (this one): code, Dockerfiles, base manifests. The CI
  pipeline lives here.
- **GitOps repository**: one overlay per environment (dev, test, prod) that
  points at the base manifests and holds the image tags, the pool size, hosts
  and network ranges. The GitOps controller watches this repository only. Nobody
  runs `kubectl apply` by hand.

Overlay (in the GitOps repository), one per environment:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base/pool/k8s            # the manager, gateway, namespace, policies, pods
images:
  - name: multica-sandbox-manager
    newName: <registry>/multica/sandbox-manager
    newTag: <git sha>
  - name: multica-git-gateway
    newName: <registry>/multica/git-gateway
    newTag: <git sha>
  - name: multica-sandbox-pool
    newName: <registry>/multica/sandbox-pool
    newTag: <git sha>
replicas:
  - name: sandbox-pool
    count: 4
patches: []   # every PLACEHOLDER in the manifests: Multica URL, LiteLLM URL and models, CIDRs
```

Every `PLACEHOLDER` in the base manifests is replaced here (use `patches`). The
base stays free of company values. The image names in `images:` must match the
names in the base manifests (`multica-sandbox-manager` and so on), not the short
names used in the Deployments.

## Secrets

No secret goes into either repository. Create these three in the cluster by
way of Key Vault (CSI driver or External Secrets) or Sealed Secrets:

| Secret | Keys | Content |
|---|---|---|
| `sandbox-manager-config` | `pool-token`, `litellm-master-key` | the bot account's Multica PAT; the LiteLLM master key |
| `git-gateway-gitlab-token` | `token` | the GitLab administrator PAT |

The GitOps controller applies only the reference to them (the Secret names).

## The pipeline (CI)

One pipeline in the app repository, triggered on merge to the main branch:

1. Build the three images with the commit SHA as the tag and push them to the
   registry.
2. Open a pull request in the GitOps repository that changes the three `newTag`
   values in the environment's overlay (`kustomize edit set image ...`). Dev can
   merge automatically; test and prod need approval, which is the release gate.
3. The GitOps controller sees the merge and syncs the cluster.

```yaml
# azure-pipelines.yml (outline)
trigger: { branches: { include: [main] } }
pool: { name: <build agents> }
variables:
  tag: $(Build.SourceVersion)
steps:
  - script: |
      docker build -f deploy/sandbox/Dockerfile.manager -t $(registry)/multica/sandbox-manager:$(tag) .
      docker build -f deploy/sandbox/Dockerfile.gateway -t $(registry)/multica/git-gateway:$(tag) .
      docker build -f deploy/sandbox/Dockerfile.pool \
        --build-arg VERSION=$(tag) --build-arg COMMIT=$(tag) \
        --build-arg MANAGER_URL=http://sandbox-manager.multica-sandbox.svc.cluster.local:8080 \
        --build-arg GIT_GATEWAY_URL=http://git-gateway.multica-sandbox.svc.cluster.local:8081/ \
        --build-arg GITLAB_URL=$(gitlabUrl) \
        --build-arg PACKAGE_MIRROR_URL=$(mirrorUrl) \
        -t $(registry)/multica/sandbox-pool:$(tag) .
    displayName: Build
  - script: docker push ... # all three, or use the registry task
    displayName: Push
  - script: |
      git clone <gitops repo> && cd <gitops repo>/overlays/dev
      kustomize edit set image multica-sandbox-manager=$(registry)/multica/sandbox-manager:$(tag) \
        multica-git-gateway=$(registry)/multica/git-gateway:$(tag) \
        multica-sandbox-pool=$(registry)/multica/sandbox-pool:$(tag)
      git commit -am "sandbox $(tag)" && git push origin HEAD:refs/heads/bump-$(tag)  # then open a PR
    displayName: Propose the new tags
```

The pod image bakes in some addresses at build time (`MANAGER_URL`,
`GIT_GATEWAY_URL`, `GITLAB_URL`, `PACKAGE_MIRROR_URL`). If an environment uses
different addresses, build the pod image per environment, or keep the same
in-cluster names everywhere (the defaults).

## Rolling out

- The GitOps controller applies the namespace, the policies, the manager and the
  gateway, then the pods. Pods that start before the manager are fine: they keep
  retrying until it answers.
- Every pod passes through one reset after the manager starts; this is normal.
- Replacing pods ends the tasks they run; the manager reports those tasks failed to
  Multica. Roll out when the pool is quiet, or accept the retries.

## When the image gains or loses an agent CLI

Change `SANDBOX_PROVIDERS` in the manager's environment (overlay patch) in the
same pull request, for example `claude,opencode`. The manager only creates
runtimes for the CLIs in that list.

## Checking a release

After the sync, the checklist in `pool/k8s/README.md` applies. The short version:

```bash
kubectl -n multica-sandbox get deploy,pod
kubectl -n multica-sandbox port-forward deploy/sandbox-manager 8791:8791
curl -s localhost:8791/v1/inventory     # idle + leased + resetting + lost = pool size
```

Then run one task from the Multica UI on the sandbox runtime and open its
merge request.

## Rollback

Revert the bump commit in the GitOps repository. The controller returns the
cluster to the previous tags. Nothing is stored outside the manager's memory
(inventory, leases), so a rollback loses no data; tasks that were running on
pods being replaced are reported failed to Multica, which can retry them.

## Before the first deployment

1. The bot account exists in Multica and is a member of the workspaces to serve;
   its PAT is in Key Vault.
2. LiteLLM has the models listed in `SANDBOX_LITELLM_MODELS` and a user for
   every person who starts tasks (the user id is the person's email, the same
   address as in GitLab).
3. The cluster's CNI enforces NetworkPolicy and does not rewrite pod source
   addresses (the manager identifies a pod by its source IP).
4. The namespace allows Pod Security `restricted`, and the uid in the pod
   manifest (1001, group 2000) is allowed by the cluster's policy. The nodes' kubelet
   sets `podPidsLimit` (for example 1024): the pod manifest has no process limit
   of its own, and a task that forks without end would otherwise fill the node.
5. Egress from the pods to the package mirror and LiteLLM is open in
   `networkpolicy.yaml` (replace the CIDR placeholders in the overlay).
