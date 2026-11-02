# Pool end-to-end run (ticket 21)

Scripts used for the real-service run (Multica, LiteLLM with the real haiku model, GitLab CE, minikube/Calico).
Nothing here holds a secret: `lib.sh` reads `$P21/{env,state,gitlab.token,anthropic.key}`, files that live outside the repository (mode 0600).

- `compose.yaml`, `litellm-config.yaml`: the external services (project `pool21`).
- `setup-gitlab.sh`, `setup-multica.sh`: seed GitLab (demo/app, demo/readonly, alice, bob) and Multica (two workspaces, bot PAT, users).
- `secscan.sh <pod-ip>`: while a task runs, scans every process's environment/arguments (read on the node) and the pod's files for the long-lived secrets; prints names and counts only.
- `gitpolicy.sh <pod-ip> <task-id>`: from a leased pod with the task's gateway token, tries pushes to main, to another task's namespace and to the task's own namespace.
- `agent-isolation.py`: piped into `kubectl exec -i <pod> -c agent-runner -- python3 -`, runs as the agent and tries to read the daemon's files and environment.

## Single-container pod

- `single-pod-scenarios.sh`: scenarios `s_hostile_task`, `s_hostile_killall`, `s_hostile_sigterm` for the dev e2e harness (source it from `e2e.sh` after its helpers). A hostile task tries to ptrace and signal PID 1, trace the daemon, overwrite the supervisor, and leaves files, locked and read-only trees, links and respawning processes everywhere; the next tasks on the pool must complete and the pod must be clean.
- `single-evil.py`: the hostile script the scenarios run inside the pod as the agent's user.
- The other scenarios of `e2e.sh` need `-c agent-runner` loops reduced to the one `daemon` container.
