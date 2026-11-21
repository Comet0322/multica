# Upgrade the fork to a new upstream release

Stage 1 of the upgrade flow, run **outside the company** in the fork's clone (`upstream` = upstream remote, `origin` = the fork). Result: `security/$NEW` (upstream tag + fix commits) and `security/$NEW-clean` (one parentless commit, same tree), both scanned and pushed.

```bash
OLD=v0.11.0      # the fork's current version
NEW=v0.12.0      # the upstream tag to move to
SCAN=~/.claude/skills/security-scan   # scanner
FORK=~/.claude/skills/multica-fork    # this skill
```

Load the **security-scan** skill before starting: steps 2 and 4 use its scanner and its triage rules.

The fix commits carry forward from `security/$OLD`; every finding fixed in an earlier version stays fixed by replaying them. Keep mobile, Helm and every upstream test: fix findings in them instead of deleting them.

## 1. Branch and rename

```bash
git fetch upstream --tags
git switch -c security/$NEW $NEW
RENAME=$(git log --format=%H -1 --grep='rename credential-named string identifiers' security/$OLD)
git checkout $RENAME -- scripts/credscan
git show $RENAME -- Makefile | git apply     # make sqlc also runs credscan; on failure add the two lines by hand
(cd scripts/credscan && go run . -dir ../../server -rename ./...)
(cd server && gofmt -w $(git diff --name-only --relative -- '*.go') && go build ./... && go vet ./...)
git add -A && git commit -C $RENAME
```

A build failure after the rename is usually a **collision**: one struct with both `token` and `ticket`, the first renamed onto the second. Rename one of them to another neutral word by hand.

Done when: `go build ./...` and `go vet ./...` pass.

## 2. Replay the fix commits

```bash
git cherry-pick $(git rev-list --reverse $RENAME..security/$OLD)
```

At each conflict, resolve, `git add`, `git cherry-pick --continue`; git carries on with the rest.

- `pnpm-lock.yaml`: `git checkout --ours pnpm-lock.yaml && bash $FORK/scripts/fixlock.sh`.
- `package.json`: keep both sides (upstream's new dependencies plus our `pnpm.overrides`).
- A fixed file that upstream deleted or already fixed: `git cherry-pick --skip`.

The fix commits also carry what the company's closed network needs, because the snapshot must build there as imported (PIPELINE.md):
- **Offline fonts**: `apps/web` loads its fonts from `@fontsource-variable/*` packages instead of `next/font/google`, which downloads them during `next build`. The first version of the change is `patches/offline-fonts-v0.11.0.patch` (made on v0.11.0: web layout, `globals.css`, `font-stacks.test.ts`, the packages moved into the pnpm catalog shared with desktop); add it once as a fix commit, then it replays like the others. When `apps/web/app/layout.tsx` or `font-stacks.test.ts` changed upstream, redo the commit on the new files rather than forcing the old diff.
- **Company npm pins**: the overrides in the latest `company/npm-pins.txt` brought out of the company (PIPELINE.md, Blocked npm versions), kept in `pnpm.overrides` while the blocked version is still in the lockfile.

Then look for what the replay cannot cover, code that is new in `$NEW`:

```bash
(cd scripts/credscan && go run . -dir ../../server ./...)
git grep -n 'multica:multica@\|:5432/multica' -- '*.go'          # built-in DB URLs in new tests
```

Fix new hits with the patterns in the security-scan skill (Triage order); a new test DSN default becomes a skip when `DATABASE_URL` is unset.

Done when: every commit applied or skipped with a reason, credscan reports 0 findings, `bash $FORK/scripts/fixlock.sh` ends with `Done`.

## 3. Test

```bash
PW=$(openssl rand -hex 12)
docker run -d --rm --name upgrade-test-pg -e POSTGRES_USER=multica -e POSTGRES_PASSWORD=$PW \
  -e POSTGRES_DB=multica -p 127.0.0.1:55432:5432 pgvector/pgvector:pg17
export DATABASE_URL="postgres://multica:$PW@127.0.0.1:55432/multica?sslmode=disable"
(cd server && go run ./cmd/migrate up && go test -p 4 ./...)
docker stop upgrade-test-pg

pnpm install --frozen-lockfile && pnpm typecheck && pnpm lint
for p in core ui web views; do pnpm --filter @multica/$p test; done
(cd apps/mobile && pnpm typecheck && pnpm lint && pnpm test)

helm() { docker run --rm -u $(id -u):$(id -g) -e HOME=/tmp -v "$PWD":"$PWD" -v /tmp:/tmp -w "$PWD" alpine/helm:3.18.4 "$@"; }
export -f helm
for f in scripts/*.test.sh; do bash "$f" >/dev/null 2>&1 && echo "ok $f" || echo "FAIL $f"; done
```

Timing-sensitive tests fail under load (`server/pkg/agent` ACP tests, `packages/views` issue-detail and filter tests). Rerun each failure alone (`go test ./pkg/agent -run '<name>' -count=5`, `pnpm exec vitest run <file>`); only a failure that repeats alone is real. Desktop is out of scope.

If `deploy/helm/` changed since `$OLD` (`git diff --stat $OLD $NEW -- deploy/`), also install the chart on a local kind cluster and check all pods Ready and `/healthz` ok.

Done when: every failure is fixed or passes when rerun alone.

## 4. Company gates: offline build, then scan

Two gates the snapshot must pass before it goes in: the company's closed network (Multica-specific, below) and the company's scanners (the security-scan skill, which is generic). Any change made for either gate is committed before the final scan, so the scan covers everything that goes in.

First the offline build, because whatever it needs is code and dependencies the scan must cover (the offline-fonts fix adds `@fontsource-variable/*` packages). Build the web image with Google Fonts unreachable and expect it to succeed; it fails when `next/font/google` is back (a new upstream font, or the offline-fonts commit lost in the replay):

```bash
docker build --add-host fonts.googleapis.com:127.0.0.1 --add-host fonts.gstatic.com:127.0.0.1 \
  -f Dockerfile.web -t web-offline-fonts-check . 2>&1 | tail -3
```

When it fails, fix it (usually by redoing the offline-fonts commit) and commit before the scan.

Then scan the committed branch with the security-scan skill's `scan.sh`, then compare against the accepted rules:

```bash
OUT=~/.cache/security-scan/runs/multica-$NEW
$SCAN/scripts/scan.sh . --branch security/$NEW --out $OUT
python3 $SCAN/scripts/new-rules.py $OUT $FORK/accepted-rules/multica.txt
```

For each **new rule**, read every hit: fix it in code, or, when it is by design, add it to `accepted-rules/multica.txt` with the reason. Dependency advisories follow the security-scan skill (Triage order); the ones that need a major upgrade are reported, not forced. Commit fixes and rescan with ClamAV and YARA included for the final run.

Done when, on the final commit: the web image builds with Google Fonts unreachable (rebuild if a scan fix touched the web app or its dependencies), `new-rules.py` exits 0, credscan and Gitleaks (working tree) are clean, and no fixable dependency advisory is left.

## 5. Clean snapshot and push

```bash
bash $FORK/scripts/clean-snapshot.sh security/$NEW
git push origin security/$NEW security/$NEW-clean      # branches only, never tags
```

Push only when the user has asked for it in this task.

Done when: `clean-snapshot.sh` prints `gitleaks history: clean` and both branches are on `origin`.

## Report

Reply with: the commit list on `security/$NEW` beyond the tag, the final scan table, the offline web build result, new rules and what was done with each, remaining dependency advisories with the reason each stays, and tests that failed only under load.
