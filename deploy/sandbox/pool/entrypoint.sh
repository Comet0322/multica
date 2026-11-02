#!/bin/sh
# Becomes pod-supervisor (PID 1) with the pod layout taken from the image's
# environment. Everything after "--" is the daemon command. The supervisor
# passes the environment on to the daemon, so the daemon sees the image's
# MULTICA_* settings.
set -eu

: "${POD_CONTROL_URL:?set by the image}"
exec /usr/local/bin/pod-supervisor --clean-ipc \
	--control-url "$POD_CONTROL_URL" \
	--work-root "$POD_WORK_ROOT" \
	--tmpdir "$POD_TMPDIR" \
	--daemon-home "$POD_SINGLE_HOME" \
	--daemon-home-template "$POD_SINGLE_HOME_TEMPLATE" \
	${POD_POLL_INTERVAL:+--poll-interval "$POD_POLL_INTERVAL"} \
	-- /usr/local/bin/multica daemon start --foreground
