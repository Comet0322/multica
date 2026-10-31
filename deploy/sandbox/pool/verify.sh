#!/bin/bash
# Builds the pool pod image (fake agent target) and checks the isolation rules
# against a local Docker with a stand-in manager. Run from the repository root:
#   deploy/sandbox/pool/verify.sh
# Only creates its own uniquely named image and container, and removes them.
set -u

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../../.." && pwd)"
tag="$$"
image="multica-pod-image-verify:$tag"
name="multica-pod-image-verify-$tag"
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
dx() { docker exec "$name" "$@"; }
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
	docker rm -f "$name" >/dev/null 2>&1
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

docker run -d --name "$name" --read-only \
	--tmpfs /work --tmpfs /home/agent --tmpfs /tmp \
	--cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
	--cap-add KILL --cap-add SETUID --cap-add SETGID \
	--security-opt no-new-privileges \
	-e POD_POLL_INTERVAL=500ms "$image" >/dev/null || { fail "container starts"; exit 1; }

echo "== daemon registers with the manager"
registered() { state | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if s["registrations"] and s["heartbeats"] else 1)'; }
if wait_for 60 registered; then pass "daemon registered and sends heartbeats"; else
	fail "daemon registered and sends heartbeats"
	docker logs "$name" 2>&1 | tail -20
fi

echo "== read-only root, three writable areas"
check "write to / fails" bash -c "! docker exec $name sh -c 'touch /rootfile'"
check "write to /etc fails" bash -c "! docker exec $name sh -c 'touch /etc/x'"
check "write to /usr/local fails" bash -c "! docker exec $name sh -c 'touch /usr/local/x'"
check "write to /work succeeds" dx sh -c 'touch /work/x'
check "write to /home/agent succeeds" dx sh -c 'touch /home/agent/x'
check "write to /tmp succeeds" dx sh -c 'touch /tmp/x'

echo "== identities and capabilities"
[ "$(dx cat /proc/1/comm)" = "pod-supervisor" ] && pass "PID 1 is pod-supervisor" || fail "PID 1 is pod-supervisor"
daemon_uid="$(dx sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm)" = multica ] && awk "/^Uid:/{print \$2}" $p/status; done' | head -1)"
[ -n "$daemon_uid" ] && [ "$daemon_uid" != 0 ] && pass "daemon runs as non-root (uid $daemon_uid)" || fail "daemon runs as non-root (uid '$daemon_uid')"
[ "$(dx id -u sandboxd)" = "$daemon_uid" ] && pass "daemon uid is the image's sandboxd user" || fail "daemon uid is the image's sandboxd user"
[ "$daemon_uid" != "$(dx id -u node)" ] && pass "daemon and agent users differ" || fail "daemon and agent users differ"
daemon_caps="$(dx sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm)" = multica ] && awk "/^CapEff:/{print \$2}" $p/status; done' | head -1)"
[ "$daemon_caps" = "0000000000000000" ] && pass "daemon has no effective capabilities" || fail "daemon capabilities: $daemon_caps"
pid1_caps="$(dx awk '/^CapBnd:/{print $2}' /proc/1/status)"
# CHOWN(0) DAC_OVERRIDE(1) FOWNER(3) KILL(5) SETGID(6) SETUID(7) = 0xeb
[ "$pid1_caps" = "00000000000000eb" ] && pass "supervisor capability bounding set is only the six it needs" || fail "supervisor CapBnd: $pid1_caps"
dx sh -c 'cat /proc/1/status' | grep -q '^NoNewPrivs:\s*1' && pass "no-new-privileges set" || fail "no-new-privileges set"

echo "== daemon settings"
dcfg=/tmp/.multica-daemon/.multica
dx sh -c "cat $dcfg/config.json" | grep -q "$mgr" && pass "daemon config points at the manager" || fail "daemon config points at the manager"
dx sh -c "cat $dcfg/config.json" | grep -q '"token":"pool-placeholder"' && pass "daemon config has the placeholder token" || fail "placeholder token"
# The daemon logs its effective config at startup.
dx cat $dcfg/daemon.log | grep -F 'gc: disabled' >/dev/null && pass "daemon logs gc: disabled" || fail "daemon logs gc: disabled"

echo "== daemon and agent are different users"
daemon_pid="$(dx pgrep -x multica | head -1)"
# Not readable even by root once capabilities are dropped, which is the point;
# the abstract socket's name is in the socket table instead.
sock="$(dx sh -c "grep -o '@multica-pod-agents-[0-9a-f]*' /proc/net/unix | head -1 | tr -d @")"
dpath="/usr/local/libexec/pod-agents:$(dx sh -c 'echo $PATH')"
[ -n "$sock" ] && pass "the supervisor listens for agent launches" || fail "the launcher socket is not listening"
dx test -x /usr/local/libexec/pod-agents/claude && pass "the claude shim exists" || fail "the claude shim exists"
node_uid="$(dx id -u node)"
# Plays what the daemon does: start the agent CLI by name, in a task directory
# it created, with a task variable.
dx sh -c 'rm -rf /work/t && mkdir -m 755 /work/t && : > /work/t/f'
dx chown sandboxd:node /work/t /work/t/f
docker exec -u sandboxd:node -w /work/t -e POD_AGENT_SOCKET="$sock" -e PATH="$dpath" -e HOME=/tmp/.multica-daemon \
	-e FAKE_CLAUDE_REPORT=/tmp/agent-report "$name" sh -c 'echo "" | claude' >/dev/null 2>&1
report="$(dx cat /tmp/agent-report 2>/dev/null)"
echo "$report" | grep -qx "uid=$node_uid" && pass "an agent started by the daemon runs as uid $node_uid" || fail "agent uid: $report"
echo "$report" | grep -qx "home=/home/agent" && pass "the agent gets the agent HOME" || fail "agent HOME: $report"
echo "$report" | grep -qx 'daemon-config=hidden' && pass "the agent cannot read the daemon's CLI config" || fail "daemon config visible to the agent: $report"
echo "$report" | grep -qx 'cwd-writable=yes' && pass "the agent can write in the task directory the daemon made" || fail "agent cannot write in the task directory: $report"
check "the agent user cannot list the daemon's HOME" bash -c "! docker exec -u node $name ls /tmp/.multica-daemon"
check "the agent user cannot read the daemon's process environment" bash -c "! docker exec -u node $name cat /proc/$daemon_pid/environ"
check "the agent user cannot signal the daemon" bash -c "! docker exec -u node $name kill -0 $daemon_pid"
check "the agent user cannot ptrace the daemon" bash -c "! docker exec -u node $name python3 -c 'import ctypes,sys; l=ctypes.CDLL(None,use_errno=True); sys.exit(0 if l.ptrace(16,int(sys.argv[1]),0,0)==0 else 1)' $daemon_pid"
check "the agent user cannot rename the daemon's HOME" bash -c "! docker exec -u node $name mv /tmp/.multica-daemon /tmp/x"
dx rm -f /tmp/agent-report2
docker exec -u node:node -w /tmp -e POD_AGENT_SOCKET="$sock" -e PATH="$dpath" -e FAKE_CLAUDE_REPORT=/tmp/agent-report2 \
	"$name" sh -c 'echo "" | claude' >/dev/null 2>&1
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
check "python venv works as the daemon user" dx su node -s /bin/sh -c 'python3 -m venv /tmp/venv && /tmp/venv/bin/python -c pass'

echo "== daemon crash restarts cleanly"
beats() { state | python3 -c 'import json,sys; print(json.load(sys.stdin)["heartbeats"])'; }
regs() { state | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["registrations"]))'; }
r0="$(regs)"
dx pkill -9 -x multica
registered_again() { [ "$(regs)" -gt "$r0" ]; }
wait_for 30 registered_again && pass "daemon killed, restarted by the supervisor and registered again" || fail "daemon crash restart"

echo "== reset leaves only PID 1 and a new daemon"
old_daemon="$(dx pgrep -x multica | head -1)"
# An orphan outside the daemon's process group, as an agent could create.
dx sh -c 'setsid sleep 3000 >/dev/null 2>&1 & setsid sh -c "setsid sleep 3000 & sleep 3000" >/dev/null 2>&1 &'
dx touch /work/leftover /home/agent/leftover /tmp/leftover
dx sh -c 'pgrep -x sleep | wc -l' | grep -qv '^0$' && pass "orphan processes exist before reset" || fail "orphan processes exist before reset"
curl -fsS -X POST "http://127.0.0.1:$port/_admin/reset?id=r-1" >/dev/null
reset_done() { state | python3 -c 'import json,sys; s=json.load(sys.stdin); sys.exit(0 if any(r["id"]=="r-1" and r["status"]=="done" for r in s["reports"]) else 1)'; }
wait_for 30 reset_done && pass "reset reported done" || fail "reset reported done"
wait_for 30 dx pgrep -x multica
new_daemon="$(dx pgrep -x multica | head -1)"
[ -n "$new_daemon" ] && [ "$new_daemon" != "$old_daemon" ] && pass "daemon restarted (pid $old_daemon -> $new_daemon)" || fail "daemon restarted"
procs="$(dx ps -eo pid,comm --no-headers | grep -vE ' (ps|sh)$' | awk '{print $2}' | sort | tr '\n' ' ')"
echo "processes after reset: $procs"
[ "$procs" = "multica pod-supervisor " ] && pass "only PID 1 and the new daemon remain" || fail "only PID 1 and the new daemon remain: $procs"
check "leftover files removed from all three areas" dx sh -c '! ls /work/leftover /home/agent/leftover /tmp/leftover'
check "daemon HOME rebuilt from its template" dx test -f /tmp/.multica-daemon/.multica/config.json
check "agent HOME rebuilt from its template" dx test -d /home/agent/.claude
regs="$(state | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["registrations"]))')"
[ "$regs" -ge 2 ] && pass "new daemon registered again ($regs registrations)" || fail "new daemon registered again ($regs)"

echo
[ "$failed" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$failed"
