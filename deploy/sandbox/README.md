# Managed sandbox runtime

Members see one runtime per workspace. Behind it, a fixed pool of pods runs the
agents' tasks, and every pod runs the stock `multica daemon`. A task borrows a
pod, runs, and the pod is reset before anyone else uses it. Agents therefore
have the same features as on a local runtime.

```
Multica server ──/api/daemon/*── sandbox-broker (pool manager) ── pods (multica daemon + agent CLIs)
                 (bot PAT)                ▲                              │
                                          └── daemon and agent traffic ──┘
```

Pods register themselves; the manager creates and deletes no sandboxes and does
not call the Kubernetes API, so the pool can sit on Kubernetes, Docker or VMs.
LLM calls go through LiteLLM with a key per task, and git and merge requests go
through the git gateway.

Everything else (settings, pod reset, authorization, lease handling, LiteLLM,
git gateway, security notes, known limits) is in [POOL.md](POOL.md). Manifests
and the manual smoke test are in [pool/k8s](pool/k8s).

| File | What |
|---|---|
| `Dockerfile.pool` | The pod image (daemon, agent CLIs, pod supervisor) |
| `Dockerfile.broker` | The pool manager |
| `Dockerfile.gateway` | The git gateway |
| `home/` | Files copied into the agent's home at every pod reset |
| `fake/` | A stand-in agent CLI for tests without a model |
