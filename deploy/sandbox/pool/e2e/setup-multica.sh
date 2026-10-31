#!/bin/bash
# Creates the Multica side of the pool end-to-end run through the HTTP API:
# users alice, bob and the bot, two workspaces (pool-a, pool-b), the bot's PAT
# and both humans as members. Uses the dev verification code of the local
# stack (APP_ENV=development). Writes ids and tokens to $OUT (mode 0600), never
# to stdout. Usage: ENV_FILE=<env> OUT=<state file> setup-multica.sh
set -euo pipefail
: "${ENV_FILE:?}" "${OUT:?}"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
API="${API:-http://127.0.0.1:${POOL21_API_PORT:-18121}}"
umask 077

login() { # email -> JWT
	curl -fsS -X POST "$API/auth/send-code" -H 'Content-Type: application/json' -d "{\"email\":\"$1\"}" >/dev/null
	curl -fsS -X POST "$API/auth/verify-code" -H 'Content-Type: application/json' \
		-d "{\"email\":\"$1\",\"code\":\"$POOL21_DEV_CODE\"}" | jq -r .token
}
call() { # jwt method path [json]
	local jwt=$1 m=$2 p=$3 body=${4:-}
	if [ -n "$body" ]; then
		curl -fsS -X "$m" "$API$p" -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' -d "$body"
	else
		curl -fsS -X "$m" "$API$p" -H "Authorization: Bearer $jwt"
	fi
}

bot=$(login pool-bot@pool21.test)
alice=$(login alice@pool21.test)
bob=$(login bob@pool21.test)

ws_a=$(call "$bot" POST /api/workspaces '{"name":"Pool A","slug":"pool-a"}' | jq -r .id)
ws_b=$(call "$bot" POST /api/workspaces '{"name":"Pool B","slug":"pool-b"}' | jq -r .id)
pat=$(call "$bot" POST /api/tokens '{"name":"pool-manager"}' | jq -r .token)

invite() { # workspace email jwt
	call "$bot" POST "/api/workspaces/$1/members" "{\"email\":\"$2\",\"role\":\"admin\"}" >/dev/null
	local inv
	inv=$(call "$3" GET /api/invitations | jq -r --arg w "$1" '[.[]|select(.workspace_id==$w)][0].id')
	call "$3" POST "/api/invitations/$inv/accept" '{}' >/dev/null
}
invite "$ws_a" alice@pool21.test "$alice"
invite "$ws_a" bob@pool21.test "$bob"
invite "$ws_b" bob@pool21.test "$bob"

{
	echo "WS_A=$ws_a"
	echo "WS_B=$ws_b"
	echo "BOT_PAT=$pat"
	echo "JWT_BOT=$bot"
	echo "JWT_ALICE=$alice"
	echo "JWT_BOB=$bob"
} >"$OUT"
echo "workspaces and users created: A=$ws_a B=$ws_b (secrets in $OUT)"
