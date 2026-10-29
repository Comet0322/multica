#!/bin/sh
# Starts the stock Multica daemon against the sandbox broker. The root
# filesystem may be read-only: HOME and workdirs live on the sandbox volume
# ($MULTICA_SANDBOX_DATA), so agent sessions and settings survive restarts.
set -eu

: "${MULTICA_SANDBOX_BROKER_URL:?set by the sandbox broker}"
: "${MULTICA_SANDBOX_TOKEN:?set by the sandbox broker}"
data="${MULTICA_SANDBOX_DATA:-/data}"
skeleton=/etc/multica-sandbox/home

export HOME="$data/home"
mkdir -p "$HOME/.multica" "$data/workspaces"
# Image-provided files win over earlier copies so a new image's skills apply.
if [ -d "$skeleton" ]; then
	cp -R "$skeleton/." "$HOME/"
fi

umask 077
printf '{"server_url":"%s","token":"%s"}\n' "$MULTICA_SANDBOX_BROKER_URL" "$MULTICA_SANDBOX_TOKEN" >"$HOME/.multica/config.json"
unset MULTICA_SANDBOX_TOKEN MULTICA_SANDBOX_BROKER_URL

export MULTICA_WORKSPACES_ROOT="${MULTICA_WORKSPACES_ROOT:-$data/workspaces}"
# The image is the unit of rollout.
export MULTICA_DAEMON_AUTO_UPDATE=false
exec multica daemon start --foreground
