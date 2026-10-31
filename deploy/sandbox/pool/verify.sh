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
[ "$(dx id -u node)" = "$daemon_uid" ] && pass "daemon uid is the image's node user" || fail "daemon uid is the image's node user"
daemon_caps="$(dx sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm)" = multica ] && awk "/^CapEff:/{print \$2}" $p/status; done' | head -1)"
[ "$daemon_caps" = "0000000000000000" ] && pass "daemon has no effective capabilities" || fail "daemon capabilities: $daemon_caps"
pid1_caps="$(dx awk '/^CapBnd:/{print $2}' /proc/1/status)"
# CHOWN(0) DAC_OVERRIDE(1) FOWNER(3) KILL(5) SETGID(6) SETUID(7) = 0xeb
[ "$pid1_caps" = "00000000000000eb" ] && pass "supervisor capability bounding set is only the six it needs" || fail "supervisor CapBnd: $pid1_caps"
dx sh -c 'cat /proc/1/status' | grep -q '^NoNewPrivs:\s*1' && pass "no-new-privileges set" || fail "no-new-privileges set"

echo "== daemon settings"
dx sh -c 'cat /home/agent/.multica/config.json' | grep -q "$mgr" && pass "HOME template config points at the manager" || fail "HOME template config points at the manager"
dx sh -c 'cat /home/agent/.multica/config.json' | grep -q '"token":"pool-placeholder"' && pass "HOME template config has the placeholder token" || fail "placeholder token"
# The daemon logs its effective config at startup.
dx cat /home/agent/.multica/daemon.log | grep -F 'gc: disabled' >/dev/null && pass "daemon logs gc: disabled" || fail "daemon logs gc: disabled"

echo "== system tooling"
git_rules="$(dx git config --system --get-regexp 'url\..*\.insteadof')"
echo "$git_rules" | grep -q 'git-gateway' && pass "system gitconfig has insteadOf to the gateway: $git_rules" || fail "insteadOf"
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
check "HOME rebuilt from the template" dx test -f /home/agent/.multica/config.json
regs="$(state | python3 -c 'import json,sys; print(len(json.load(sys.stdin)["registrations"]))')"
[ "$regs" -ge 2 ] && pass "new daemon registered again ($regs registrations)" || fail "new daemon registered again ($regs)"

echo
[ "$failed" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$failed"
