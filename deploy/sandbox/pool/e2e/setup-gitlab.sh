#!/bin/bash
# Prepares the local GitLab CE of the pool end-to-end run: an administrator
# token (made with gitlab-rails, written to $TOKEN_FILE mode 0600, never
# printed), users alice and bob with the same emails as their Multica users,
# and the project root-owned `demo/app` where alice and bob are Developers
# (bob is a Reporter in `demo/readonly`). Usage:
#   CONTAINER=pool21-gitlab-1 GITLAB=http://10.77.21.30 TOKEN_FILE=<file> setup-gitlab.sh
set -euo pipefail
: "${TOKEN_FILE:?}"
CONTAINER=${CONTAINER:-pool21-gitlab-1}
GITLAB=${GITLAB:-http://10.77.21.30}
umask 077

tok=$(openssl rand -hex 16)
printf 'glpat-%s' "$tok" >"$TOKEN_FILE"
docker exec "$CONTAINER" gitlab-rails runner "
u = User.find_by_username('root')
u.update!(password: '$(openssl rand -hex 12)Aa1!', password_confirmation: nil) rescue nil
t = u.personal_access_tokens.create!(name: 'pool21-gateway', scopes: [:api, :read_user, :sudo, :read_repository, :write_repository], expires_at: 30.days.from_now)
t.set_token('glpat-$tok'); t.save!
puts 'token ok'
"

g() { curl -fsS -H "PRIVATE-TOKEN: $(cat "$TOKEN_FILE")" -H 'Content-Type: application/json' "$@"; }
mkuser() { # username email -> id
	g -X POST "$GITLAB/api/v4/users" -d "{\"username\":\"$1\",\"email\":\"$2\",\"name\":\"$1\",\"password\":\"$(openssl rand -hex 12)Aa1!\",\"skip_confirmation\":true}" | jq -r .id
}
alice=$(mkuser alice alice@pool21.test)
bob=$(mkuser bob bob@pool21.test)
group=$(g -X POST "$GITLAB/api/v4/groups" -d '{"name":"demo","path":"demo"}' | jq -r .id)
app=$(g -X POST "$GITLAB/api/v4/projects" -d "{\"name\":\"app\",\"namespace_id\":$group,\"initialize_with_readme\":true,\"default_branch\":\"main\"}" | jq -r .id)
ro=$(g -X POST "$GITLAB/api/v4/projects" -d "{\"name\":\"readonly\",\"namespace_id\":$group,\"initialize_with_readme\":true,\"default_branch\":\"main\"}" | jq -r .id)
g -X POST "$GITLAB/api/v4/projects/$app/members" -d "{\"user_id\":$alice,\"access_level\":30}" >/dev/null
g -X POST "$GITLAB/api/v4/projects/$app/members" -d "{\"user_id\":$bob,\"access_level\":30}" >/dev/null
g -X POST "$GITLAB/api/v4/projects/$ro/members" -d "{\"user_id\":$bob,\"access_level\":20}" >/dev/null
g -X POST "$GITLAB/api/v4/projects/$app/repository/files/hello.py" \
	-d '{"branch":"main","content":"def greet(name):\n    return \"hello \" + name\n","commit_message":"add hello.py"}' >/dev/null
echo "gitlab ready: demo/app=$app demo/readonly=$ro alice=$alice bob=$bob"
