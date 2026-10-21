# Shared helpers for the managed-runtime PoC. Source this file.
export API=${API:-http://localhost:18900}
export KCTX=${KCTX:-kind-multica-mr-test}
k() { kubectl --context "$KCTX" "$@"; }

# login <email> -> prints a JWT (dev verification code flow)
login() {
  curl -s -X POST "$API/auth/send-code" -H 'content-type: application/json' -d "{\"email\":\"$1\"}" >/dev/null
  curl -s -X POST "$API/auth/verify-code" -H 'content-type: application/json' \
    -d "{\"email\":\"$1\",\"code\":\"888888\"}" | jq -r .token
}

# call <token> <workspace-id|-> <method> <path> [json-body]
call() {
  local tok=$1 ws=$2 m=$3 p=$4 body=${5:-}
  local args=(-s -X "$m" "$API$p" -H "Authorization: Bearer $tok" -H 'content-type: application/json')
  [ "$ws" != "-" ] && args+=(-H "X-Workspace-ID: $ws")
  [ -n "$body" ] && args+=(-d "$body")
  curl "${args[@]}"
}

# create_issues <token> <ws> <agent> <n> <prefix> [description]  -> echoes issue ids
create_issues() {
  local tok=$1 ws=$2 ag=$3 n=$4 prefix=$5 desc=${6:-hello} i
  for i in $(seq 1 "$n"); do
    call "$tok" "$ws" POST /api/issues \
      "{\"title\":\"$prefix $i\",\"description\":\"$desc\",\"assignee_type\":\"agent\",\"assignee_id\":\"$ag\"}" | jq -r .id
  done
}

# task_counts <token> <ws> <agent>  -> status histogram of the agent's tasks
task_counts() {
  call "$1" "$2" GET "/api/agents/$3/tasks" | jq -c 'group_by(.status) | map({(.[0].status): length}) | add'
}

# wait_tasks_done <token> <ws> <agent> <expected-total> <timeout-s>
wait_tasks_done() {
  local end=$((SECONDS + $5)) c
  while [ $SECONDS -lt $end ]; do
    c=$(call "$1" "$2" GET "/api/agents/$3/tasks" | jq '[.[]|select(.status=="completed" or .status=="failed" or .status=="cancelled")]|length')
    [ "$c" -ge "$4" ] && return 0
    sleep 3
  done
  return 1
}
