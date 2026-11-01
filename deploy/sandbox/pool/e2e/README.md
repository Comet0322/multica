# Pool end-to-end run (ticket 21)

Scripts used for the real-service run (Multica, LiteLLM with the real haiku model, GitLab CE, minikube/Calico).
Nothing here holds a secret: `lib.sh` reads `$P21/{env,state,gitlab.token,anthropic.key}`, files that live outside the repository (mode 0600).

- `compose.yaml`, `litellm-config.yaml`: the external services (project `pool21`).
- `setup-gitlab.sh`, `setup-multica.sh`: seed GitLab (demo/app, demo/readonly, alice, bob) and Multica (two workspaces, bot PAT, users).
- `secscan.sh <pod-ip>`: while a task runs, scans every process's environment/arguments (read on the node) and the pod's files for the long-lived secrets; prints names and counts only.
- `gitpolicy.sh <pod-ip> <task-id>`: from a leased pod with the task's gateway token, tries pushes to main, to another task's namespace and to the task's own namespace.
- `agent-isolation.py`: piped into `kubectl exec -i <pod> -c agent-runner -- python3 -`, runs as the agent and tries to read the daemon's files and environment.
