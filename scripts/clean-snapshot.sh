#!/usr/bin/env bash
# Make <branch>-clean: one parentless commit with the same tree as <branch>,
# then check its history with Gitleaks in a --no-tags clone. Does not push.
# Usage: clean-snapshot.sh <branch> [repo]
set -euo pipefail
BR=${1:?usage: clean-snapshot.sh <branch> [repo]}
REPO=$(cd "${2:-.}" && pwd)
cd "$REPO"
CLEAN="$BR-clean"
VER=${BR##*/}

git rev-parse --verify -q "refs/heads/$CLEAN" >/dev/null && { echo "$CLEAN already exists"; exit 1; }
C=$(git commit-tree "refs/heads/$BR^{tree}" \
  -m "chore: import multica $VER with security fixes" \
  -m "Single-commit snapshot of $BR so the history no longer carries credential-looking strings from earlier commits.")
git branch "$CLEAN" "$C"
git diff --quiet "refs/heads/$BR" "refs/heads/$CLEAN" || { echo "tree differs from $BR"; exit 1; }
echo "created $CLEAN ($C), same tree as $BR"

# --no-tags matters: upstream tags would pull the old history back in.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
git clone -q --no-tags --single-branch -b "$CLEAN" "$REPO" "$TMP/repo"
[ "$(git -C "$TMP/repo" rev-list --count HEAD)" = 1 ] || { echo "clone has more than one commit"; exit 1; }
docker run --rm -v "$TMP/repo":/repo zricethezav/gitleaks:latest git /repo --no-banner
echo "gitleaks history: clean"
