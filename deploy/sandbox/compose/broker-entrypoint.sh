#!/bin/sh
# Starts the sandbox broker. Without MULTICA_TOKEN it bootstraps a demo setup
# once: signs in as SANDBOX_BOOTSTRAP_EMAIL with the server's dev verification
# code, creates the workspace and a PAT, and keeps them in the state volume.
set -eu

state="${SANDBOX_BROKER_STATE_DIR:?}"
credentials="$state/bootstrap.env"
mkdir -p "$state"

if [ -z "${MULTICA_TOKEN:-}" ] && [ ! -s "$credentials" ]; then
	: "${SANDBOX_BOOTSTRAP_EMAIL:?set MULTICA_TOKEN or SANDBOX_BOOTSTRAP_EMAIL}"
	: "${SANDBOX_BOOTSTRAP_CODE:?set the server's MULTICA_DEV_VERIFICATION_CODE}"
	api="${MULTICA_SERVER_URL:?}"
	until curl -fsS "$api/health" >/dev/null 2>&1; do
		echo "waiting for $api"
		sleep 2
	done
	curl -fsS -X POST "$api/auth/send-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"$SANDBOX_BOOTSTRAP_EMAIL\"}" >/dev/null
	jwt="$(curl -fsS -X POST "$api/auth/verify-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"$SANDBOX_BOOTSTRAP_EMAIL\",\"code\":\"$SANDBOX_BOOTSTRAP_CODE\"}" | jq -r .token)"
	auth="Authorization: Bearer $jwt"
	workspace="$(curl -fsS "$api/api/workspaces" -H "$auth" | jq -r '.[0].id // empty')"
	if [ -z "$workspace" ]; then
		workspace="$(curl -fsS -X POST "$api/api/workspaces" -H "$auth" -H 'Content-Type: application/json' \
			-d "{\"name\":\"${SANDBOX_BOOTSTRAP_WORKSPACE:-Sandbox Demo}\",\"slug\":\"sandbox-demo\"}" | jq -r .id)"
	fi
	token="$(curl -fsS -X POST "$api/api/tokens" -H "$auth" -H 'Content-Type: application/json' \
		-d '{"name":"sandbox-broker"}' | jq -r .token)"
	umask 077
	printf 'MULTICA_TOKEN=%s\nMULTICA_WORKSPACE_ID=%s\n' "$token" "$workspace" >"$credentials"
	echo "bootstrapped workspace $workspace for $SANDBOX_BOOTSTRAP_EMAIL"
fi

if [ -z "${MULTICA_TOKEN:-}" ]; then
	set -a
	. "$credentials"
	set +a
fi
exec sandbox-broker
