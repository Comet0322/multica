#!/usr/bin/env bash
# End-to-end contract test: a real Multica server, the sandbox broker, the
# Docker provider and the unmodified daemon inside a sandbox, with a fake
# agent CLI. Run from the repository root after `make up C=api`:
#
#   bash deploy/sandbox/e2e/run.sh
#
# KEEP=1 leaves the broker state, sandboxes and volumes for inspection.
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
here="$root/deploy/sandbox/e2e"
port="$(sed -n 's/^PORT=//p' "$root/.env.worktree" 2>/dev/null || true)"
api="${MULTICA_E2E_API:-http://localhost:${port:?run make up C=api first}}"
code="${MULTICA_E2E_CODE:-$(sed -n 's/^MULTICA_DEV_VERIFICATION_CODE=//p' "$root/.env.worktree")}"
email="${MULTICA_E2E_EMAIL:-dev@localhost}"
broker_port="${SANDBOX_E2E_BROKER_PORT:-18790}"
instance="e2e$(date +%s)"
work="$(mktemp -d)"
image="multica-sandbox-e2e:$instance"
broker_pid=""

log() { printf '==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
	[ -n "$broker_pid" ] && kill "$broker_pid" 2>/dev/null && wait "$broker_pid" 2>/dev/null || true
	if [ "${KEEP:-}" = 1 ]; then
		log "kept state in $work (instance $instance)"
		return
	fi
	docker ps -aq --filter "label=ai.multica.sandbox.instance=$instance" | xargs -r docker rm -f >/dev/null
	docker volume ls -q --filter "label=ai.multica.sandbox.instance=$instance" | xargs -r docker volume rm >/dev/null
	docker image rm "$image" >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT

req() { # method path [json]
	local args=(-fsS -X "$1" "$api$2" -H "Authorization: Bearer $jwt" -H "Content-Type: application/json")
	[ -n "${workspace:-}" ] && args+=(-H "X-Workspace-ID: $workspace")
	[ $# -ge 3 ] && args+=(-d "$3")
	curl "${args[@]}"
}

wait_for() { # seconds description command...
	local deadline=$((SECONDS + $1)) what="$2"
	shift 2
	until "$@"; do
		[ $SECONDS -lt $deadline ] || fail "timed out waiting for $what"
		sleep 2
	done
}

log "building multica, broker and the sandbox image"
(cd "$root/server" && CGO_ENABLED=0 go build -o "$work/multica" ./cmd/multica && go build -o "$work/sandbox-broker" ./cmd/sandbox-broker)
cp -r "$here/Dockerfile" "$here/fake-claude" "$here/skill" "$root/deploy/sandbox/entrypoint.sh" "$work/"
docker build -q -t "$image" "$work" >/dev/null

log "signing in to $api"
# Like the Playwright fixtures: clear earlier codes so reruns are not rate limited.
db="$(sed -n 's/^POSTGRES_DB=//p' "$root/.env.worktree")"
docker exec "${SANDBOX_E2E_PG_CONTAINER:-multica-postgres-1}" psql -qU multica -d "$db" -c "DELETE FROM verification_code WHERE email = '$email'" >/dev/null
curl -fsS -X POST "$api/auth/send-code" -H 'Content-Type: application/json' -d "{\"email\":\"$email\"}" >/dev/null
jwt="$(curl -fsS -X POST "$api/auth/verify-code" -H 'Content-Type: application/json' -d "{\"email\":\"$email\",\"code\":\"$code\"}" | jq -r .token)"
workspace="$(req GET /api/workspaces | jq -r '.[0].id // empty')"
if [ -z "$workspace" ]; then
	workspace="$(req POST /api/workspaces "{\"name\":\"Sandbox E2E\",\"slug\":\"sandbox-e2e-$instance\"}" | jq -r .id)"
fi
pat="$(req POST /api/tokens '{"name":"sandbox-broker-e2e","expires_in_days":1}' | jq -r .token)"

log "starting the broker (instance $instance)"
MULTICA_SERVER_URL="$api" MULTICA_TOKEN="$pat" MULTICA_WORKSPACE_ID="$workspace" \
	SANDBOX_BROKER_ID="sandbox-broker-$instance" SANDBOX_BROKER_NAME="Managed Sandbox $instance" \
	SANDBOX_BROKER_LISTEN="0.0.0.0:$broker_port" SANDBOX_BROKER_URL="http://host.docker.internal:$broker_port" \
	SANDBOX_DOCKER_EXTRA_HOSTS="host.docker.internal:host-gateway" SANDBOX_DOCKER_INSTANCE="$instance" \
	SANDBOX_IMAGE="$image" SANDBOX_PROVIDERS=claude SANDBOX_BROKER_STATE_DIR="$work/state" \
	SANDBOX_IDLE_TIMEOUT=20s SANDBOX_BROKER_POLL_INTERVAL=1s SANDBOX_CLEANUP_GRACE=1s \
	"$work/sandbox-broker" >"$work/broker.log" 2>&1 &
broker_pid=$!

runtime_ready() {
	runtime="$(req GET /api/runtimes | jq -r --arg d "sandbox-broker-$instance" '.[] | select(.daemon_id == $d and .status == "online") | .id' | head -n 1)"
	[ -n "$runtime" ]
}
wait_for 180 "the managed runtime to come online (broker log: $work/broker.log)" runtime_ready
mode="$(req GET /api/runtimes | jq -r --arg id "$runtime" '.[] | select(.id == $id) | "\(.runtime_mode) \(.provider)"')"
[ "$mode" = "cloud claude" ] || fail "runtime mode/provider = $mode"
log "runtime $runtime is online as $mode"

log "checking that image-local skills are hidden"
skills_request="$(req POST "/api/runtimes/$runtime/local-skills" '{}' | jq -r .id)"
skills_done() {
	skills="$(req GET "/api/runtimes/$runtime/local-skills/$skills_request")"
	[ "$(jq -r .status <<<"$skills")" = completed ]
}
wait_for 60 "the local skill list" skills_done
[ "$(jq -r '.supported, .mcp_supported, (.skills | length)' <<<"$skills" | tr '\n' ' ')" = "false false 0 " ] || fail "local skills leaked: $skills"

log "running a task in a sandbox"
agent="$(req POST /api/agents "{\"name\":\"sandbox-e2e-$instance\",\"runtime_id\":\"$runtime\",\"instructions\":\"e2e\",\"visibility\":\"workspace\"}" | jq -r .id)"
issue="$(req POST /api/issues "{\"title\":\"sandbox e2e $instance\",\"status\":\"todo\",\"assignee_type\":\"agent\",\"assignee_id\":\"$agent\"}" | jq -r .id)"
task_done() {
	status="$(req GET "/api/agents/$agent/tasks" | jq -r 'if type == "array" then . else .tasks end | .[0].status // empty')"
	case "$status" in completed) return 0 ;; failed | cancelled) fail "task $status (broker log: $work/broker.log)" ;; esac
	return 1
}
wait_for 240 "the task to complete (broker log: $work/broker.log)" task_done

comments="$(req GET "/api/issues/$issue/comments")"
jq -e '.. | strings | select(startswith("sandbox-e2e: agent ran in"))' <<<"$comments" >/dev/null ||
	fail "agent comment missing; the agent could not reach Multica through the broker: $comments"
jq -e '.. | strings | select(contains("hidden-runtime-skill"))' <<<"$comments" >/dev/null ||
	fail "agent did not see the image's runtime-private skill: $comments"
log "agent posted through the broker and saw the hidden skill"

key="$(jq -r --arg a "$agent" '.sandboxes[] | select(.agent_id == $a and .scope_kind == "issue") | .key' "$work/state/journal.json")"
[ -n "$key" ] || fail "no issue-scoped sandbox recorded for agent $agent"
container="multica-sbx-$instance-$key"
docker volume inspect "$container" >/dev/null || fail "sandbox volume $container missing"
stopped() { [ "$(docker inspect -f '{{.State.Status}}' "$container" 2>/dev/null)" = exited ]; }
wait_for 90 "the idle sandbox to stop" stopped
log "idle sandbox $key stopped, volume kept"

log "finishing the issue"
req PUT "/api/issues/$issue" '{"status":"done"}' >/dev/null
deleted() { ! docker volume inspect "$container" >/dev/null 2>&1 && ! docker inspect "$container" >/dev/null 2>&1; }
wait_for 150 "the finished issue's sandbox and volume to be deleted" deleted
log "finished issue's sandbox and volume deleted"

if grep -q '"refused sandbox request"' "$work/broker.log"; then
	grep '"refused sandbox request"' "$work/broker.log" >&2
	fail "the daemon called endpoints the broker does not support"
fi
log "PASS"
