#!/bin/bash
# Builds the pool pod image (fake agent target) and checks the isolation rules
# against a local Docker with a stand-in manager. A pod is two containers: the
# daemon container, and the agent runner container joined to its network
# namespace; neither runs as root. Run from the repository root:
#   deploy/sandbox/pool/verify.sh
# Only creates its own uniquely named image, containers and temporary volumes,
# and removes them.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
tag="$$"
image="multica-pod-image-verify:$tag"
name="multica-pod-image-verify-$tag"
runner="$name-runner"
vol="$(mktemp -d)"
sock=multica-pod-agents
port="${VERIFY_PORT:-18$((RANDOM % 900 + 100))}"
host_ip="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')"
mgr="http://$host_ip:$port"
failed=0

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; failed=1; }
check() { # check <description> <command...>
	local d="$1"
	shift
	if "$@" >/dev/null 2>&1; then pass "$d"; else fail "$d"; fi
}
dx() { docker exec "$name" "$@"; }   # the daemon container
dr() { docker exec "$runner" "$@"; } # the agent runner container
state() { curl -fsS "http://127.0.0.1:$port/_admin/state"; }
wait_for() { # wait_for <seconds> <command...>
	local n="$1"
	shift
	for _ in $(seq "$n"); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 1
	done
	return 1
}
cleanup() {
	docker rm -f "$runner" "$name" >/dev/null 2>&1
	# The volumes hold files of the container users; remove them as that user.
	docker run --rm -v "$vol:/vol" --user 1001:2000 --entrypoint sh "$image" -c 'rm -rf /vol/* 2>/dev/null' >/dev/null 2>&1
	rm -rf "$vol" 2>/dev/null
	docker rmi "$image" >/dev/null 2>&1
	[ -n "${mgr_pid:-}" ] && kill "$mgr_pid" 2>/dev/null
}
trap cleanup EXIT

echo "== build $image"
docker build -q -f "$root/deploy/sandbox/Dockerfile.pool" --target fake \
	--build-arg MANAGER_URL="$mgr" -t "$image" "$root" || { fail "image builds"; exit 1; }
pass "image builds"

python3 "$here/fake_manager.py" "$port" &
mgr_pid=$!
wait_for 10 curl -fsS "http://127.0.0.1:$port/_admin/state" || { fail "stand-in manager starts"; exit 1; }

# The volumes the pod gets from the kubelet: work, tmp and shm shared, one home each.
for d in work tmp shm dhome ahome; do mkdir "$vol/$d" && chmod 2777 "$vol/$d"; done
hardening=(--read-only --cap-drop ALL --security-opt no-new-privileges)
shared=(-v "$vol/work:/work" -v "$vol/tmp:/tmp" -v "$vol/shm:/dev/shm" -e POD_SHM_DIR=/dev/shm)
docker run -d --name "$name" --user 1001:2000 "${hardening[@]}" "${shared[@]}" \
	-v "$vol/dhome:/home/daemon" -e POD_POLL_INTERVAL=500ms "$image" >/dev/null || { fail "daemon container starts"; exit 1; }
docker run -d --name "$runner" --network "container:$name" --user 1000:2000 "${hardening[@]}" "${shared[@]}" \
	-v "$vol/ahome:/home/agent" -e POD_ROLE=runner "$image" >/dev/null || { fail "agent runner container starts"; exit 1; }

echo "== daemon registers with the manager"
registered() { state | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if s["registrations"] and s["heartbeats"] else 1)'; }
if wait_for 60 registered; then pass "daemon registered and sends heartbeats"; else
	fail "daemon registered and sends heartbeats"
	docker logs "$name" 2>&1 | tail -20
fi

echo "== read-only root, writable volumes"
check "write to / fails" bash -c "! docker exec $name sh -c 'touch /rootfile'"
check "write to /etc fails" bash -c "! docker exec $name sh -c 'touch /etc/x'"
check "write to /usr/local fails" bash -c "! docker exec $name sh -c 'touch /usr/local/x'"
check "write to /work succeeds" dx sh -c 'touch /work/x'
check "write to /home/daemon succeeds in the daemon container" dx sh -c 'touch /home/daemon/x'
check "write to /home/agent succeeds in the runner container" dr sh -c 'touch /home/agent/x'
check "write to /tmp succeeds" dx sh -c 'touch /tmp/x'

echo "== identities and capabilities"
[ "$(dx cat /proc/1/comm)" = "pod-supervisor" ] && pass "PID 1 of the daemon container is pod-supervisor" || fail "daemon container PID 1"
[ "$(dr cat /proc/1/comm)" = "pod-supervisor" ] && pass "PID 1 of the runner container is pod-supervisor" || fail "runner container PID 1"
daemon_uid="$(dx sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm)" = multica ] && awk "/^Uid:/{print \$2}" $p/status; done' | head -1)"
[ "$daemon_uid" = 1001 ] && [ "$(dx id -u sandboxd)" = "$daemon_uid" ] && pass "daemon runs as sandboxd (uid $daemon_uid)" || fail "daemon uid '$daemon_uid'"
[ "$(dr awk '/^Uid:/{print $2}' /proc/1/status)" = "$(dx id -u node)" ] && pass "the runner runs as the node user" || fail "runner user"
for c in "$name" "$runner"; do
	bad="$(docker exec "$c" sh -c 'for p in /proc/[0-9]*; do awk "/^Uid:/{u=\$2} /^CapEff:/{c=\$2} /^CapBnd:/{b=\$2} /^NoNewPrivs:/{n=\$2} END{print u, c, b, n}" $p/status; done' | sort -u | grep -v '^[1-9][0-9]* 0000000000000000 0000000000000000 1$')"
	[ -z "$bad" ] && pass "$c: no process runs as root or holds a capability, no-new-privileges set" || fail "$c: $bad"
done

echo "== daemon settings"
dcfg=/home/daemon/.multica
dx sh -c "cat $dcfg/config.json" | grep -q "$mgr" && pass "daemon config points at the manager" || fail "daemon config points at the manager"
dx sh -c "cat $dcfg/config.json" | grep -q '"token":"pool-placeholder"' && pass "daemon config has the placeholder token" || fail "placeholder token"
# The daemon logs its effective config at startup.
dx cat $dcfg/daemon.log | grep -F 'gc: disabled' >/dev/null && pass "daemon logs gc: disabled" || fail "daemon logs gc: disabled"

echo "== daemon and agent are different users in different containers"
dpath="/usr/local/libexec/pod-agents:$(dx sh -c 'echo $PATH')"
dx test -x /usr/local/libexec/pod-agents/claude && pass "the claude shim exists" || fail "the claude shim exists"
node_uid="$(dx id -u node)"
# Plays what the daemon does: start the agent CLI by name, in a task directory
# it created, with a task variable.
dx sh -c 'rm -rf /work/t && mkdir -m 755 /work/t && : > /work/t/f'
docker exec -w /work/t -e POD_AGENT_SOCKET="$sock" -e POD_DAEMON_TMP=/home/daemon/tmp -e PATH="$dpath" -e HOME=/home/daemon \
	-e FAKE_CLAUDE_REPORT=/tmp/agent-report "$name" sh -c 'echo "" | claude' >/dev/null 2>&1
report="$(dx cat /tmp/agent-report 2>/dev/null)"
echo "$report" | grep -qx "uid=$node_uid" && pass "an agent started by the daemon runs as uid $node_uid" || fail "agent uid: $report"
echo "$report" | grep -qx "home=/home/agent" && pass "the agent gets the agent HOME" || fail "agent HOME: $report"
echo "$report" | grep -qx 'daemon-config=hidden' && pass "the agent cannot read the daemon's CLI config" || fail "daemon config visible to the agent: $report"
echo "$report" | grep -qx 'cwd-writable=yes' && pass "the agent can write in the task directory the daemon made" || fail "agent cannot write in the task directory: $report"
check "the agent container does not see the daemon's process" bash -c "! docker exec $runner pgrep -x multica"
check "the agent container has none of the daemon's files" bash -c "[ -z \"\$(docker exec $runner ls -A /home/daemon)\" ]"
check "the runner turns the agent user away on its control socket" bash -c "[ \"\$(docker exec $runner python3 -c \"
import socket
s = socket.socket(socket.AF_UNIX); s.settimeout(5)
s.connect('\\0$sock-ctl')
try:
    s.sendall(b'{\\\"op\\\":\\\"close\\\"}'); print(repr(s.recv(100)))
except ConnectionError:
    print(repr(b''))\")\" = \"b''\" ]"
dx rm -f /tmp/agent-report2
docker exec -w /tmp -e POD_AGENT_SOCKET="$sock" -e PATH="$dpath" -e FAKE_CLAUDE_REPORT=/tmp/agent-report2 \
	"$runner" sh -c 'echo "" | claude' >/dev/null 2>&1
check "the agent user cannot use the launcher" bash -c "! docker exec $name test -e /tmp/agent-report2"

echo "== system tooling"
# Claude Code must not try its telemetry or update endpoints (egress denies
# them); the daemon hands the pod's environment on to the agent CLI.
[ "$(dx printenv CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC)" = 1 ] && pass "image disables Claude Code nonessential traffic" || fail "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"
[ "$(dx printenv DISABLE_AUTOUPDATER)" = 1 ] && pass "image disables Claude Code auto-update" || fail "DISABLE_AUTOUPDATER"
git_rules="$(dx git config --system --get-regexp 'url\..*\.insteadof')"
echo "$git_rules" | grep -q 'git-gateway' && pass "system gitconfig has insteadOf to the gateway: $git_rules" || fail "insteadOf"
# An agent push needs the task token: git asks the credential helper, which
# must answer for the gateway's host only.
gateway_url="$(echo "$git_rules" | awk '{print $1}' | sed -E 's/^url\.//; s/\.insteadof$//')"
cred_fill() { # cred_fill <url> [env assignment]
	dx env GIT_TERMINAL_PROMPT=0 ${2:+"$2"} sh -c 'printf "url=%s\n\n" "$1" | git credential fill' sh "$1"
}
filled="$(cred_fill "${gateway_url}group/proj.git" GIT_GATEWAY_TOKEN=gwt_verify_token 2>&1)"
echo "$filled" | grep -qx 'username=agent' && echo "$filled" | grep -qx 'password=gwt_verify_token' \
	&& pass "git credential fill supplies agent and the task token for the gateway" || fail "git credential fill for the gateway: $filled"
other="$(cred_fill 'https://gitlab.example.com/group/proj.git' GIT_GATEWAY_TOKEN=gwt_verify_token 2>&1)"
echo "$other" | grep -q 'gwt_verify_token' && fail "the task token is offered to a host other than the gateway" || pass "the task token is not offered to any other host"
none="$(cred_fill "${gateway_url}group/proj.git" 2>&1)"
echo "$none" | grep -q '^password=' && fail "a password is supplied although the task has no token: $none" || pass "no password is supplied without a task token"
dx pip config list | grep -q 'index-url=.*package-mirror' && pass "pip index points at the mirror" || fail "pip index"
dx npm config get registry | grep -q 'package-mirror' && pass "npm registry points at the mirror" || fail "npm registry"
dx sh -c 'echo $GOPROXY' | grep -q 'package-mirror' && pass "GOPROXY points at the mirror" || fail "GOPROXY"
for tool in git node npm pnpm python3 pip make gcc curl jq; do
	check "toolchain: $tool" dx sh -c "command -v $tool"
done
check "python venv works for the agent user" dr sh -c 'python3 -m venv /tmp/venv && /tmp/venv/bin/python -c pass'

echo "== daemon crash restarts cleanly"
beats() { state | python3 -c 'import json,sys; print(json.load(sys.stdin)["heartbeats"])'; }
regs() { state | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["registrations"]))'; }
r0="$(regs)"
dx pkill -9 -x multica
registered_again() { [ "$(regs)" -gt "$r0" ]; }
wait_for 30 registered_again && pass "daemon killed, restarted by the supervisor and registered again" || fail "daemon crash restart"

echo "== reset leaves only PID 1 and a new daemon"
old_daemon="$(dx pgrep -x multica | head -1)"
# Orphans outside the daemon's process group, as an agent could create, in both containers.
for c in "$name" "$runner"; do
	docker exec "$c" sh -c 'setsid sleep 3000 >/dev/null 2>&1 & setsid sh -c "setsid sleep 3000 & sleep 3000" >/dev/null 2>&1 &'
	docker exec "$c" sh -c 'pgrep -x sleep | wc -l' | grep -qv '^0$' && pass "$c: orphan processes exist before reset" || fail "$c: orphan processes exist before reset"
done
dx touch /work/leftover /tmp/leftover /home/daemon/leftover
dr touch /home/agent/leftover
curl -fsS -X POST "http://127.0.0.1:$port/_admin/reset?id=r-1" >/dev/null
reset_done() { state | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if any(r["id"]=="r-1" and r["status"]=="done" for r in s["reports"]) else 1)'; }
wait_for 30 reset_done && pass "reset reported done" || fail "reset reported done"
wait_for 30 dx pgrep -x multica
new_daemon="$(dx pgrep -x multica | head -1)"
[ -n "$new_daemon" ] && [ "$new_daemon" != "$old_daemon" ] && pass "daemon restarted (pid $old_daemon -> $new_daemon)" || fail "daemon restarted"
procs="$(dx ps -eo pid,comm --no-headers | grep -vE ' (ps|sh)$' | awk '{print $2}' | sort | tr '\n' ' ')"
echo "daemon container processes after reset: $procs"
[ "$procs" = "multica pod-supervisor " ] && pass "only PID 1 and the new daemon remain" || fail "only PID 1 and the new daemon remain: $procs"
procs="$(dr ps -eo pid,comm --no-headers | grep -vE ' (ps|sh)$' | awk '{print $2}' | sort | tr '\n' ' ')"
[ "$procs" = "pod-supervisor " ] && pass "only the runner remains in the agent container" || fail "only the runner remains in the agent container: $procs"
check "leftover files removed from all areas" bash -c "! docker exec $name ls /work/leftover /tmp/leftover /home/daemon/leftover && ! docker exec $runner ls /home/agent/leftover"
check "daemon HOME rebuilt from its template" dx test -f /home/daemon/.multica/config.json
check "agent HOME rebuilt from its template" dr test -d /home/agent/.claude
regs="$(state | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["registrations"]))')"
[ "$regs" -ge 2 ] && pass "new daemon registered again ($regs registrations)" || fail "new daemon registered again ($regs)"

echo
[ "$failed" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$failed"
