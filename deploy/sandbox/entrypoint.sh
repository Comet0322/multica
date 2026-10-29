#!/bin/sh
# Starts the stock Multica daemon against the sandbox broker. Everything that
# must survive a restart lives on the sandbox volume ($MULTICA_SANDBOX_DATA).
set -eu

: "${MULTICA_SANDBOX_BROKER_URL:?set by the sandbox broker}"
: "${MULTICA_SANDBOX_TOKEN:?set by the sandbox broker}"
data="${MULTICA_SANDBOX_DATA:-/data}"

mkdir -p "$data/multica" "$data/workspaces"
ln -sfn "$data/multica" "$HOME/.multica"

# Agent session stores, so an agent's next task can resume its session.
for rel in ${MULTICA_SANDBOX_PERSIST:-.claude/projects .codex/sessions}; do
	mkdir -p "$data/home/$rel" "$(dirname "$HOME/$rel")"
	rm -rf "${HOME:?}/$rel"
	ln -s "$data/home/$rel" "$HOME/$rel"
done

umask 077
printf '{"server_url":"%s","token":"%s"}\n' "$MULTICA_SANDBOX_BROKER_URL" "$MULTICA_SANDBOX_TOKEN" >"$data/multica/config.json"
unset MULTICA_SANDBOX_TOKEN MULTICA_SANDBOX_BROKER_URL

export MULTICA_WORKSPACES_ROOT="${MULTICA_WORKSPACES_ROOT:-$data/workspaces}"
# The image is the unit of rollout.
export MULTICA_DAEMON_AUTO_UPDATE=false
exec multica daemon start --foreground
