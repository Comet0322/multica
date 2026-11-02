# Scenarios for the single-container pool pod (pool.yaml). Source this
# after the helpers of the dev e2e.sh (K, call, mk_agent, mk_issue, wait_idle,
# start_long_task, lease_pod_ip, pod_of_ip, ...) and run them by name.

EVIL=$(dirname "${BASH_SOURCE[0]}")/single-evil.py

pod_start() { "${K[@]}" exec "$1" -- sh -c 'cut -d" " -f22 /proc/1/stat'; }
pod_restarts() { "${K[@]}" get pod "$1" -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
run_evil() { # pod mode: starts the script detached in the pod, as the agent's user
	"${K[@]}" exec -i "$1" -- sh -c 'cat >/tmp/evil.py' <"$EVIL" || return 1
	"${K[@]}" exec "$1" -- sh -c "setsid python3 /tmp/evil.py $2 >/tmp/evil.out 2>&1 </dev/null &"
}

# followup_issues: six short tasks, which reach every pod of the pool. Fills
# the array issues of the caller.
followup_issues() {
	local agent n id
	wait_runtime "$WS_A" 90 || { fail "no pool runtime for the follow-up tasks"; return 1; }
	agent=$(mk_agent "$JWT_BOB" "$WS_A" 6); [ -n "$agent" ] || { fail "could not create the follow-up agent"; return 1; }
	issues=()
	for n in 1 2 3 4 5 6; do
		# allow_duplicate: Multica refuses a second active issue that looks like the first.
		id=$(call "$JWT_BOB" "$WS_A" POST /api/issues "$(jq -nc --arg t "e2e $RANDOM$n" --arg d "$SHORT_PROMPT" --arg a "$agent" '{title:$t,description:$d,assignee_type:"agent",assignee_id:$a,allow_duplicate:true}')" | jq -r '.id // empty'); [ -n "$id" ] || { fail "could not create follow-up issue $n"; return 1; }
		issues+=("$id")
	done
}

# s_hostile_task: a running task tries to ptrace and signal the supervisor,
# overwrite its binary, trace the daemon, and leaves files (locked, read-only,
# deeper than PATH_MAX, links, a FIFO) and processes that respawn or ignore
# signals in every writable area. After the task ends the pod must be exactly a
# fresh pod: the same PID 1 (not restarted), nothing left over, and the next
# tasks must complete on it.
s_hostile_task() {
	local ip pod start0 start1 rs rs0 out left procs n agent issues=()
	wait_runtime "$WS_A" 90 || { fail "no pool runtime (the pool re-registers one after a runtime was deleted)"; return; }
	start_long_task || { fail "task never reached running"; return; }
	ip=$(lease_pod_ip "$TASK"); pod=$(pod_of_ip "$ip"); [ -n "$pod" ] || { fail "no pod for lease ip '$ip'"; return; }
	start0=$(pod_start "$pod"); rs0=$(pod_restarts "$pod")
	run_evil "$pod" plant || { fail "could not start the hostile script in $pod"; return; }
	sleep 12
	out=$("${K[@]}" exec "$pod" -- cat /tmp/evil.out)
	echo "$out" | sed 's/^/  evil: /'
	[ -n "$out" ] || { echo "  evil.out is empty; supervisor log of $pod:"; "${K[@]}" logs "$pod" --since=2m 2>&1 | cut -c1-300; echo "  processes:"; "${K[@]}" exec "$pod" -- sh -c 'ls -la /tmp; for d in /proc/[0-9]*; do echo "${d#/proc/} $(tr "\\0" " " <$d/cmdline | cut -c1-80)"; done'; }
	# What the supervisor must refuse.
	echo "$out" | grep -q '^ptrace_pid1=Operation not permitted' || { fail "ptrace of PID 1 was not refused: $(echo "$out" | grep ptrace_pid1)"; return; }
	echo "$out" | grep -q '^read_proc1_mem=Permission denied' || { fail "/proc/1/mem readable"; return; }
	echo "$out" | grep -q '^read_proc1_environ=Permission denied' || { fail "/proc/1/environ readable"; return; }
	echo "$out" | grep -Eq '^write_supervisor_binary=(Read-only file system|Permission denied)' || { fail "supervisor binary writable"; return; }
	echo "$out" | grep -q '^pid1_alive_after_signals=True' || { fail "PID 1 died or stopped after signals"; return; }
	echo "$out" | grep -q '^planted=' || { fail "hostile script did not finish planting"; return; }
	call "$JWT_BOB" "$WS_A" POST "/api/tasks/$TASK/cancel" '{}' >/dev/null
	wait_idle 150 || { fail "pod not idle after the hostile task (idle $(idle_count))"; return; }
	sleep 5
	start1=$(pod_start "$pod"); rs=$(pod_restarts "$pod")
	[ "$start0" = "$start1" ] && [ "$rs" = "$rs0" ] || { fail "PID 1 was replaced (start $start0 -> $start1, restarts $rs0 -> $rs)"; return; }
	left=$("${K[@]}" exec "$pod" -- sh -c 'find /work /tmp /dev/shm /home/pod -name "canary*" -o -name "evil*" -o -name deep 2>/dev/null; ls -A /work/locked /tmp/ro 2>&1 | grep -v "No such" ')
	[ -z "$left" ] || { fail "files survived the reset in $pod: $(echo "$left" | head -5 | tr '\n' ' ')"; return; }
	procs=$("${K[@]}" exec "$pod" -- sh -c 'pgrep -fc "[s]leep 7777|[e]vil.py"; true')
	[ "${procs:-0}" = 0 ] || { fail "$procs hostile process(es) survived the reset in $pod"; return; }
	"${K[@]}" exec "$pod" -- sh -c 'grep -q task-1 /home/pod/.bashrc 2>/dev/null' && { fail "HOME kept the task's .bashrc"; return; }
	"${K[@]}" exec "$pod" -- sh -c 'grep -q evil.invalid /home/pod/.multica/config.json' && { fail "HOME kept the task's daemon config"; return; }
	# The next tasks, on every pod, including this one.
	followup_issues || return
	for n in "${issues[@]}"; do
		wait_status "$JWT_BOB" "$WS_A" "$n" 240 completed failed cancelled
		[ "$(task_status "$JWT_BOB" "$WS_A" "$n")" = completed ] || { fail "a task after the hostile one did not complete: $(call "$JWT_BOB" "$WS_A" GET "/api/issues/$n/task-runs" | jq -c '.[0]|{status,error}')"; return; }
	done
	wait_idle 120 || { fail "pool not idle after the follow-up tasks"; return; }
	echo "PASS $SCENARIO (pod $pod kept PID 1 (restarts $rs0 -> $rs), nothing left over, 6 follow-up tasks completed)"
}

# s_hostile_killall: the task kills every process its user can signal
# (kill(-1, SIGKILL): the daemon, the agent and itself). PID 1 survives, the
# pod resets, and the pool carries on.
s_hostile_killall() {
	local ip pod start0 start1 rs rs0 agent n issues=()
	wait_runtime "$WS_A" 90 || { fail "no pool runtime (the pool re-registers one after a runtime was deleted)"; return; }
	start_long_task || { fail "task never reached running"; return; }
	ip=$(lease_pod_ip "$TASK"); pod=$(pod_of_ip "$ip"); [ -n "$pod" ] || { fail "no pod for lease ip '$ip'"; return; }
	start0=$(pod_start "$pod"); rs0=$(pod_restarts "$pod")
	run_evil "$pod" killall || { fail "could not start the hostile script in $pod"; return; }
	wait_idle 150 || { fail "pod not idle after kill(-1) (idle $(idle_count))"; return; }
	start1=$(pod_start "$pod"); rs=$(pod_restarts "$pod")
	[ "$start0" = "$start1" ] && [ "$rs" = "$rs0" ] || { fail "PID 1 was replaced (start $start0 -> $start1, restarts $rs0 -> $rs)"; return; }
	followup_issues || return
	for n in "${issues[@]}"; do
		wait_status "$JWT_BOB" "$WS_A" "$n" 240 completed failed cancelled
		[ "$(task_status "$JWT_BOB" "$WS_A" "$n")" = completed ] || { fail "a task after kill(-1) did not complete: $(call "$JWT_BOB" "$WS_A" GET "/api/issues/$n/task-runs" | jq -c '.[0]|{status,error}')"; return; }
	done
	wait_idle 120 || { fail "pool not idle after the follow-up tasks"; return; }
	echo "PASS $SCENARIO (pod $pod kept PID 1 through kill(-1), 6 follow-up tasks completed)"
}

# s_hostile_sigterm: a task sends SIGTERM to PID 1. That restarts the container
# (an availability cost, not a leak): what the task left is gone when the pod
# is back, and the pool carries on.
s_hostile_sigterm() {
	local ip pod rs rs0 agent n issues=()
	wait_runtime "$WS_A" 90 || { fail "no pool runtime (the pool re-registers one after a runtime was deleted)"; return; }
	start_long_task || { fail "task never reached running"; return; }
	ip=$(lease_pod_ip "$TASK"); pod=$(pod_of_ip "$ip"); [ -n "$pod" ] || { fail "no pod for lease ip '$ip'"; return; }
	rs0=$(pod_restarts "$pod")
	"${K[@]}" exec "$pod" -- sh -c 'echo secret >/work/canary-file; echo secret >/dev/shm/canary-file; echo secret >/home/pod/canary-file; setsid sleep 7777 >/dev/null 2>&1 </dev/null & kill -TERM 1'
	wait_idle 180 || { fail "pod not idle after SIGTERM to PID 1 (idle $(idle_count))"; return; }
	rs=$(pod_restarts "$pod")
	"${K[@]}" exec "$pod" -- sh -c 'find /work /tmp /dev/shm /home/pod -name "canary*" | grep -q .' && { fail "files survived the restart"; return; }
	[ "$("${K[@]}" exec "$pod" -- sh -c 'pgrep -fc "[s]leep 7777"; true')" = 0 ] || { fail "process survived the restart"; return; }
	followup_issues || return
	for n in "${issues[@]}"; do
		wait_status "$JWT_BOB" "$WS_A" "$n" 240 completed failed cancelled
		[ "$(task_status "$JWT_BOB" "$WS_A" "$n")" = completed ] || { fail "a task after the restart did not complete: $(call "$JWT_BOB" "$WS_A" GET "/api/issues/$n/task-runs" | jq -c '.[0]|{status,error}')"; return; }
	done
	echo "PASS $SCENARIO (container restarts: $rs0 -> $rs, nothing left over, 6 follow-up tasks completed)"
}

# s_hostile_forkbomb: the task forks until the pod's pids limit is hit. That can
# take the pod's daemon down with it (the task fails and the pod is reset), but
# it must come back clean, with PID 1 intact, and the next tasks must complete.
s_hostile_forkbomb() {
	local ip pod start0 start1 rs rs0 issues=() n
	wait_runtime "$WS_A" 90 || { fail "no pool runtime (the pool re-registers one after a runtime was deleted)"; return; }
	start_long_task || { fail "task never reached running"; return; }
	ip=$(lease_pod_ip "$TASK"); pod=$(pod_of_ip "$ip"); [ -n "$pod" ] || { fail "no pod for lease ip '$ip'"; return; }
	start0=$(pod_start "$pod"); rs0=$(pod_restarts "$pod")
	run_evil "$pod" forkbomb || { fail "could not start the hostile script in $pod"; return; }
	wait_idle 240 || { fail "pod not idle after the fork bomb (idle $(idle_count))"; return; }
	sleep 5
	start1=$(pod_start "$pod"); rs=$(pod_restarts "$pod")
	[ "$start0" = "$start1" ] && [ "$rs" = "$rs0" ] || { fail "PID 1 was replaced (start $start0 -> $start1, restarts $rs0 -> $rs)"; return; }
	[ "$("${K[@]}" exec "$pod" -- sh -c 'pgrep -c "[s]leep 7777"; true')" = 0 ] || { fail "fork-bomb processes survived the reset"; return; }
	followup_issues || return
	for n in "${issues[@]}"; do
		wait_status "$JWT_BOB" "$WS_A" "$n" 240 completed failed cancelled
		[ "$(task_status "$JWT_BOB" "$WS_A" "$n")" = completed ] || { fail "a task after the fork bomb did not complete: $(call "$JWT_BOB" "$WS_A" GET "/api/issues/$n/task-runs" | jq -c '.[0]|{status,error}')"; return; }
	done
	wait_idle 120 || { fail "pool not idle after the follow-up tasks"; return; }
	echo "PASS $SCENARIO (pod $pod kept PID 1 (restarts $rs0 -> $rs), no fork-bomb process left, 6 follow-up tasks completed)"
}

# s_no_leftovers: whatever the agent's user can write is emptied by a reset.
# The scenario plants a marker in every directory the pod's user can write to
# (found by asking the filesystem, not from a list, so a new writable path or a
# volume nothing cleans shows up here) and creates System V and POSIX IPC
# objects, runs tasks through every pod, and then
# looks for the markers. A pod whose /work marker is still there was not reset
# and counts as a failure of the scenario, not as a pass.
s_no_leftovers() {
	local pods p dirs n left bad=0 issues=()
	pods=$("${K[@]}" get pods -l app.kubernetes.io/name=sandbox-pool -o name)
	for p in $pods; do
		dirs=$("${K[@]}" exec "$p" -- sh -c 'find / \( -path /proc -o -path /sys -o -path /dev/pts -o -path /dev/mqueue \) -prune -o -type d -writable -print 2>/dev/null')
		n=$(echo "$dirs" | grep -c .)
		[ "$n" -ge 3 ] || { fail "$p: found only $n writable directories"; return; }
		echo "$dirs" | "${K[@]}" exec -i "$p" -- sh -c 'while read -r d; do echo planted >"$d/.leftover-probe" 2>/dev/null; done'
		# System V and POSIX IPC objects live in the pod's IPC namespace, outside any directory.
		"${K[@]}" exec "$p" -- sh -c 'ipcmk -M 4096 >/dev/null && ipcmk -Q >/dev/null && ipcmk -S 1 >/dev/null && touch /dev/mqueue/leftover-probe' || { fail "$p: could not create IPC objects"; return; }
		echo "$p: marker planted in $n writable directories: $(echo "$dirs" | tr '\n' ' ' | cut -c1-200)"
	done
	followup_issues || return
	sleep 5; wait_idle 180 || { fail "pods did not return to idle"; return; }
	for p in $pods; do
		"${K[@]}" exec "$p" -- sh -c 'test ! -e /work/.leftover-probe' || { echo "  $p was never leased, so it was never reset"; bad=$((bad + 1)); continue; }
		left=$("${K[@]}" exec "$p" -- sh -c 'find / \( -path /proc -o -path /sys \) -prune -o -name .leftover-probe -print 2>/dev/null')
		[ -z "$left" ] || { echo "  $p kept: $(echo "$left" | tr '\n' ' ')"; bad=$((bad + 1)); }
		left=$("${K[@]}" exec "$p" -- sh -c 'ipcs | grep -c "^0x"; ls /dev/mqueue | wc -l' | tr '\n' ' ')
		[ "$left" = "0 0 " ] || { echo "  $p kept IPC objects (sysv, mqueue): $left"; bad=$((bad + 1)); }
	done
	[ $bad = 0 ] || { fail "$bad pod(s) kept a marker or were not reset"; return; }
	echo "PASS $SCENARIO"
}
