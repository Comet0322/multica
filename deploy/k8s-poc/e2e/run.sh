#!/usr/bin/env bash
# Acceptance run for managed runtimes on a live kind cluster (after ./setup.sh).
# Prints PASS/FAIL per check and a timing summary; exits non-zero on any FAIL.
set -uo pipefail
cd "$(dirname "$0")"
export KCTX=kind-multica-mr-e2e API=http://localhost:18901
. ../lib.sh
NS=mr-e2e
CSECRET=e2e-controller-secret-0123456789
PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "PASS  $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL  $1  -- $2"; }
check(){ local name=$1; shift; if "$@"; then ok "$name"; else bad "$name" "$*"; fi; }
sql()  { k -n $NS exec deploy/postgres -- psql -U multica -d multica -tAc "$1"; }
export -f k sql; export NS KCTX
now()  { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }
ctl()  { curl -s -X "$1" "$API/internal/v1$2" -H "Authorization: Bearer $CSECRET" -H 'content-type: application/json' ${3:+-d "$3"}; }
task_status() { sql "select status from agent_task_queue where id='$1'"; }
wait_task() { # id timeout -> echoes final status
  local end=$((SECONDS+$2)) s
  while [ $SECONDS -lt $end ]; do
    s=$(task_status "$1"); case $s in completed|failed|cancelled) echo "$s"; return;; esac; sleep 1
  done; echo "timeout:$s"
}
new_issue() { # token ws agent title desc -> issue id
  call "$1" "$2" POST /api/issues "{\"title\":\"$4\",\"description\":\"$5\",\"assignee_type\":\"agent\",\"assignee_id\":\"$3\"}" | jq -r .id
}
task_of_issue() { sql "select id from agent_task_queue where issue_id='$1' order by created_at limit 1"; }
sandbox_mode() { k -n $NS get sandbox "$1" -o jsonpath='{.spec.operatingMode}' 2>/dev/null; }

pkill -f "port-forward svc/server 18901" 2>/dev/null || true
(kubectl --context "$KCTX" -n $NS port-forward svc/server 18901:8080 >/tmp/mr-e2e-pf.log 2>&1 &)
sleep 3

echo "=== fixtures"
OPS=$(login ops@e2e.test); OWNER=$(login owner@e2e.test); MEMBER=$(login member@e2e.test)
WS=$(call "$OWNER" - POST /api/workspaces "{\"name\":\"E2E\",\"slug\":\"e2e-$RANDOM\"}" | jq -r .id)
OWNER_ID=$(call "$OWNER" - GET /api/me | jq -r .id)
MEMBER_ID=$(call "$MEMBER" - GET /api/me | jq -r .id)
sql "insert into member (workspace_id,user_id,role) values ('$WS','$MEMBER_ID','member')" >/dev/null
MEMBER_PAT=$(call "$MEMBER" - POST /api/tokens '{"name":"e2e-member"}' | jq -r .token)

echo "=== A. platform admin gate"
check "member gets 404 on admin list" test "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $MEMBER" $API/api/admin/managed-runtimes)" = "404"
check "member gets 404 on admin create" test "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $MEMBER" -H 'content-type: application/json' -d '{}' $API/api/admin/managed-runtimes)" = "404"
check "member status probe says not admin" test "$(call "$MEMBER" - GET /api/admin/status | jq -r .is_admin)" = "false"
check "ops status probe says admin" test "$(call "$OPS" - GET /api/admin/status | jq -r .is_admin)" = "true"

echo "=== B. admin creates managed runtime"
T0=$(now)
RT=$(call "$OPS" - POST /api/admin/managed-runtimes "{\"workspace_id\":\"$WS\",\"daemon_id\":\"e2e-d1\",\"provider\":\"claude\",\"display_name\":\"Company Claude\",\"owner_id\":\"$OWNER_ID\"}" | jq -r .id)
check "create returns runtime id" test -n "$RT" -a "$RT" != null
RT=$(sql "select id from agent_runtime where daemon_id='e2e-d1'")
check "admin list shows daemon id" test "$(curl -s -H "Authorization: Bearer $OPS" $API/api/admin/managed-runtimes | jq -r '.runtimes[0].daemon_id')" = "e2e-d1"

echo "=== C. controller provisions a Sandbox"
SB=""
for i in $(seq 1 30); do SB=$(k -n $NS get sandbox -o name 2>/dev/null | head -1); [ -n "$SB" ] && break; sleep 1; done
check "sandbox object created by controller" test -n "$SB"
SBN=${SB#*/}
echo "sandbox=$SBN mode=$(sandbox_mode "$SBN")"

echo "=== D. member view is redacted but runtime is usable"
MV=$(call "$MEMBER" "$WS" GET /api/runtimes)
check "member sees the runtime" test "$(echo "$MV" | jq -r --arg id "$RT" '[.[]|select(.id==$id)]|length')" = "1"
check "daemon_id hidden" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.daemon_id')" = "null"
check "device_info/metadata/owner hidden" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|[.device_info, (.metadata|length|tostring), (.owner_id|tostring)]|join("|")')" = "|0|null"
check "managed flag set" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.managed')" = "true"
check "projected status online while suspended/waking" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.status')" = "online"
check "owner cannot rename" test "$(curl -s -o /dev/null -w '%{http_code}' -X PATCH -H "Authorization: Bearer $OWNER" -H "X-Workspace-ID: $WS" -H 'content-type: application/json' -d '{"custom_name":"x"}' $API/api/runtimes/$RT)" = "404"
check "owner cannot delete via user API" test "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $OWNER" -H "X-Workspace-ID: $WS" $API/api/runtimes/$RT)" = "404"

echo "=== E. cold start: agent + issue -> task completes"
AG=$(call "$OWNER" "$WS" POST /api/agents "{\"name\":\"e2e-agent\",\"runtime_id\":\"$RT\",\"visibility\":\"workspace\",\"max_concurrent_tasks\":3}" | jq -r .id)
check "agent created on managed runtime" test -n "$AG" -a "$AG" != null
T1=$(now)
I1=$(new_issue "$OWNER" "$WS" "$AG" "first" "hello")
sleep 1; TK1=$(task_of_issue "$I1")
S=$(wait_task "$TK1" 180); T2=$(now)
check "first task completes through a controller-provisioned Sandbox" test "$S" = completed
COLD=$(elapsed "$T1" "$T2"); echo "TIMING cold (suspended/absent -> completed, incl. 1s fake agent): ${COLD}s"
POD=$(k -n $NS get pod -l agents.x-k8s.io/sandbox-name-hash -o name 2>/dev/null | head -1)
[ -z "$POD" ] && POD=$(k -n $NS get pod -o name | grep -v -E 'postgres|server|litellm|runtime-controller' | head -1)
echo "pod=$POD"

echo "=== F. secrets are not reachable from the agent"
OUT=$(call "$OWNER" "$WS" GET "/api/issues/$I1/tasks" 2>/dev/null)
check "no work_dir paths in member task list" test "$(call "$MEMBER" "$WS" GET /api/agents/$AG/tasks | jq -r '[.[]|select((.work_dir//"")!="" or (.relative_work_dir//"")!="")]|length')" = "0"
check "no config.json credential in pod home" bash -c "! k -n $NS exec ${POD#pod/} -- sh -c 'cat /data/home/.multica/config.json 2>/dev/null' | grep -q 'mdt_\|mul_'"
check "agent env has no daemon token (mdt_) or user PAT (mul_)" bash -c "! k -n $NS exec ${POD#pod/} -- sh -c 'find /data/workspaces -name .fake-last-env.txt -exec cat {} +' | grep -E 'mdt_|mul_|MULTICA_DAEMON_TOKEN'"
check "agent env carries only the task-scoped token (mat_)" bash -c "k -n $NS exec ${POD#pod/} -- sh -c 'find /data/workspaces -name .fake-last-env.txt -exec cat {} +' | grep -q 'MULTICA_TOKEN=mat_'"
check "agent-visible env carries the per-Sandbox LLM key (accepted exposure)" bash -c "k -n $NS exec ${POD#pod/} -- sh -c 'find /data/workspaces -name .fake-last-env.txt -exec cat {} +' | grep -q 'sk-'"

echo "=== G. LiteLLM key lifecycle"
LL=$(k -n $NS exec deploy/litellm -- wget -qO- localhost:4000/state 2>/dev/null || k -n $NS get --raw "/api/v1/namespaces/$NS/services/litellm:4000/proxy/state")
echo "$LL" | head -c 300; echo
check "one live LLM key minted" test "$(echo "$LL" | jq -r '.live|length')" = "1"

echo "=== H. daemon API authorization"
claim() { curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $1" -H "X-Workspace-ID: $WS" -H 'content-type: application/json' -d '{}' "$API/api/daemon/runtimes/$RT/tasks/claim"; }
I2=$(new_issue "$OWNER" "$WS" "$AG" "bait" "@@SLEEP@@25@@END@@"); sleep 1
check "member JWT cannot claim on managed runtime" test "$(claim "$MEMBER")" = "404"
check "member PAT cannot claim on managed runtime" test "$(claim "$MEMBER_PAT")" = "404"
ctl POST /managed-runtimes "{\"workspace_id\":\"$WS\",\"daemon_id\":\"e2e-d2\",\"provider\":\"claude\",\"display_name\":\"Second\",\"owner_id\":\"$OWNER_ID\"}" >/dev/null
OTHER=$(ctl POST /daemon-tokens "{\"workspace_id\":\"$WS\",\"daemon_id\":\"e2e-d2\",\"ttl_seconds\":600}" | jq -r .token)
check "minted a real mdt_ for a second managed daemon" test "${OTHER#mdt_}" != "$OTHER"
check "mdt_ of another daemon cannot claim" test "$(claim "$OTHER")" = "404"
check "internal API rejects a missing controller secret" test "$(curl -s -o /dev/null -w '%{http_code}' $API/internal/v1/managed-runtimes/state)" = "401"
check "internal API rejects a user token" test "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $OPS" $API/internal/v1/managed-runtimes/state)" = "401"
RT2=$(sql "select id from agent_runtime where daemon_id='e2e-d2'")
curl -s -X DELETE -H "Authorization: Bearer $OPS" $API/api/admin/managed-runtimes/$RT2 >/dev/null
for i in $(seq 1 40); do [ -z "$(k -n $NS get sandbox e2e-d2 -o name 2>/dev/null)" ] && break; sleep 2; done
TK2=$(task_of_issue "$I2")
S=$(wait_task "$TK2" 90); check "bait task still completes for the real daemon" test "$S" = completed

echo "=== I. continuity: workdir reused after idle suspend"
echo "waiting for idle suspend (IDLE_SUSPEND_AFTER=40s)..."
for i in $(seq 1 120); do [ "$(sandbox_mode "$SBN")" = Suspended ] && break; sleep 2; done
check "idle Sandbox is suspended" test "$(sandbox_mode "$SBN")" = Suspended
sleep 3
check "pod is gone while suspended" test "$(k -n $NS get pod -o name | grep -v -E 'postgres|server|litellm|runtime-controller' | wc -l)" = "0"
check "PVC retained while suspended" test "$(k -n $NS get pvc -o name | wc -l)" -ge 1
MV=$(call "$MEMBER" "$WS" GET /api/runtimes)
check "member still sees runtime online while suspended" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.status')" = "online"
T3=$(now)
call "$OWNER" "$WS" POST /api/issues/$I1/comments '{"content":"follow-up on the same issue"}' >/dev/null
sleep 1; TK3=$(sql "select id from agent_task_queue where issue_id='$I1' and id<>'$TK1' order by created_at desc limit 1")
check "follow-up comment queued a second task on the same issue" test -n "$TK3"
S=$(wait_task "$TK3" 180); T4=$(now)
check "queued task wakes the Sandbox and completes" test "$S" = completed
echo "TIMING wake (suspended -> completed, incl. 1s fake agent): $(elapsed "$T3" "$T4")s"
check "mode is Running again" test "$(sandbox_mode "$SBN")" = Running
RES=$(sql "select result from agent_task_queue where id='$TK3'")
echo "wake result: $RES"
check "same workdir reused (workdir_uses>=2 or session resumed)" bash -c "echo '$RES' | grep -qE 'workdir_uses=([2-9]|[1-9][0-9])'"

echo "=== J. graceful stop drains the running task"
I4=$(new_issue "$OWNER" "$WS" "$AG" "drain" "@@SLEEP@@20@@END@@"); sleep 1; TK4=$(task_of_issue "$I4")
for i in $(seq 1 30); do [ "$(task_status "$TK4")" = running ] && break; sleep 1; done
POD=$(k -n $NS get pod -o name | grep -v -E 'postgres|server|litellm|runtime-controller' | head -1)
k -n $NS delete "$POD" --wait=false >/dev/null
S=$(wait_task "$TK4" 120)
check "SIGTERM during a task lets it finish (drain)" test "$S" = completed

echo "=== K. hard kill mid-task"
for i in $(seq 1 60); do [ "$(sql "select status from agent_runtime where id='$RT'")" = online ] && break; sleep 2; done
I5=$(new_issue "$OWNER" "$WS" "$AG" "kill" "@@SLEEP@@40@@END@@"); sleep 1; TK5=$(task_of_issue "$I5")
for i in $(seq 1 40); do [ "$(task_status "$TK5")" = running ] && break; sleep 1; done
POD=$(k -n $NS get pod -o name | grep -v -E 'postgres|server|litellm|runtime-controller' | head -1)
k -n $NS delete "$POD" --force --grace-period=0 >/dev/null 2>&1
sleep 5
echo "first task after kill: $(sql "select status||' '||coalesce(failure_reason,'')||' '||coalesce(error,'') from agent_task_queue where id='$TK5'")"
for i in $(seq 1 60); do
  CH=$(sql "select count(*) from agent_task_queue where parent_task_id='$TK5'"); [ "$CH" -ge 1 ] && break; sleep 3; done
echo "retry children: $CH"
if [ "${CH:-0}" -ge 1 ]; then
  CHID=$(sql "select id from agent_task_queue where parent_task_id='$TK5' limit 1")
  S=$(wait_task "$CHID" 180)
  check "killed task is retried on the recreated Sandbox and completes" test "$S" = completed
else
  bad "killed task is retried" "no retry child; original=$(task_status "$TK5") (see known gaps)"
fi

echo "=== M. controller down fails closed, recovery restores"
# Take the Sandbox out of the picture first so only the controller's report decides availability.
for i in $(seq 1 120); do [ "$(sandbox_mode "$SBN")" = Suspended ] && break; sleep 2; done
k -n $NS scale deploy/runtime-controller --replicas=0 >/dev/null
echo "controller stopped; waiting past the 2 min report TTL..."
sleep 135
MV=$(call "$MEMBER" "$WS" GET /api/runtimes)
check "stale controller report => runtime shown offline (no queueing behind a dead controller)" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.status')" = "offline"
k -n $NS scale deploy/runtime-controller --replicas=1 >/dev/null
k -n $NS wait --for=condition=available deploy/runtime-controller --timeout=60s >/dev/null
sleep 8
MV=$(call "$MEMBER" "$WS" GET /api/runtimes)
check "controller back => runtime shown online again" test "$(echo "$MV" | jq -r --arg id "$RT" '.[]|select(.id==$id)|.status')" = "online"

echo "=== L. admin delete tears everything down"
KEYS_BEFORE=$(k -n $NS exec deploy/litellm -- wget -qO- localhost:4000/state | jq -r '.live|length')
D=$(curl -s -X DELETE -H "Authorization: Bearer $OPS" $API/api/admin/managed-runtimes/$RT | jq -r .status)
check "admin delete ok" test "$D" = ok
for i in $(seq 1 60); do [ -z "$(k -n $NS get sandbox -o name 2>/dev/null)" ] && break; sleep 2; done
check "Sandbox removed by controller" test -z "$(k -n $NS get sandbox -o name 2>/dev/null)"
check "controller Secrets removed" test "$(k -n $NS get secret -o name | grep -c -E 'daemon-token|llm')" = "0"
LL=$(k -n $NS exec deploy/litellm -- wget -qO- localhost:4000/state)
echo "litellm: keys_before=$KEYS_BEFORE state=$(echo "$LL" | jq -c '{generated,deleted,live:(.live|length)}')"
check "LiteLLM key revoked" test "$(echo "$LL" | jq -r '.live|length')" = "0"
check "daemon tokens revoked" test "$(sql "select count(*) from daemon_token where daemon_id='e2e-d1'")" = "0"
check "agent kept but unbound" test "$(sql "select (runtime_id is null)::text from agent where id='$AG'")" = "true"

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
