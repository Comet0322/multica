#!/bin/sh
# Becomes pod-supervisor (PID 1) with the pod layout taken from the image's
# environment. POD_ROLE=runner starts the agent container's runner; otherwise
# this is the daemon container, and everything after "--" is the daemon command.
# The supervisor passes the environment on to the daemon, so the daemon sees the
# image's MULTICA_* settings.
set -eu

case "${POD_ROLE:-supervisor}" in
runner)
	exec /usr/local/bin/pod-supervisor -role runner \
		--agent-socket "$POD_AGENT_SOCKET" \
		--daemon-uid "$POD_DAEMON_UID" \
		--work-root "$POD_WORK_ROOT" \
		--tmpdir "$POD_TMPDIR" \
		--shim-dir "$POD_SHIM_DIR" \
		--home "$POD_HOME" \
		--home-template "$POD_HOME_TEMPLATE"
	;;
supervisor)
	: "${POD_CONTROL_URL:?set by the image}"
	exec /usr/local/bin/pod-supervisor \
		--control-url "$POD_CONTROL_URL" \
		--agent-socket "$POD_AGENT_SOCKET" \
		--work-root "$POD_WORK_ROOT" \
		--tmpdir "$POD_TMPDIR" \
		--shim-dir "$POD_SHIM_DIR" \
		--daemon-home "$POD_DAEMON_HOME" \
		--daemon-home-template "$POD_DAEMON_HOME_TEMPLATE" \
		${POD_POLL_INTERVAL:+--poll-interval "$POD_POLL_INTERVAL"} \
		-- /usr/local/bin/multica daemon start --foreground
	;;
*)
	echo "unknown POD_ROLE: $POD_ROLE" >&2
	exit 2
	;;
esac
