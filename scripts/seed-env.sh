#!/usr/bin/env bash
# Fill the credentials a freshly copied .env.example leaves empty: the database
# password (and the password inside DATABASE_URL) and JWT_SECRET. Values that are
# already set are kept. POSTGRES_PASSWORD from the environment wins over a
# generated one, so a worktree can reuse the password of the shared Postgres.
set -euo pipefail

file="${1:?usage: seed-env.sh ENV_FILE}"

current() { sed -n "s/^$1=//p" "$file" | tail -1; }

set_value() {
  if grep -q "^[# ]*$1=" "$file"; then
    sed -i.bak "s|^[# ]*$1=.*|$1=$2|" "$file" && rm -f "$file.bak"
  else
    printf '%s=%s\n' "$1" "$2" >> "$file"
  fi
}

pgpass="$(current POSTGRES_PASSWORD)"
pgpass="${pgpass:-${POSTGRES_PASSWORD:-$(openssl rand -hex 24)}}"
set_value POSTGRES_PASSWORD "$pgpass"

if grep -q '^DATABASE_URL=postgres' "$file"; then
  sed -i.bak -E "s#^(DATABASE_URL=postgres(ql)?://[^:@/]+)(:[^@]*)?@#\1:${pgpass}@#" "$file" && rm -f "$file.bak"
fi

if [ -z "$(current JWT_SECRET)" ]; then
  set_value JWT_SECRET "$(openssl rand -hex 32)"
fi
