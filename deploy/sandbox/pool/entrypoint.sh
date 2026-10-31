#!/bin/sh
# Becomes pod-supervisor (PID 1) with the pod layout taken from the image's
# environment. Everything after "--" is the daemon command. The supervisor
# passes the environment on to the daemon, so the daemon sees the image's
# MULTICA_* settings.
set -eu

: "${POD_CONTROL_URL:?set by the image}"
exec /usr/local/bin/pod-supervisor \
	--control-url "$POD_CONTROL_URL" \
	--work-root "$POD_WORK_ROOT" \
	--home "$POD_HOME" \
	--tmpdir "$POD_TMPDIR" \
	--home-template "$POD_HOME_TEMPLATE" \
	--uid "$POD_UID" --gid "$POD_GID" \
	--agent-uid "$POD_AGENT_UID" --agent-gid "$POD_AGENT_GID" \
	--shim-dir "$POD_SHIM_DIR" \
	--daemon-home "$POD_DAEMON_HOME" \
	--daemon-home-template "$POD_DAEMON_HOME_TEMPLATE" \
	${POD_POLL_INTERVAL:+--poll-interval "$POD_POLL_INTERVAL"} \
	-- /usr/local/bin/multica daemon start --foreground
