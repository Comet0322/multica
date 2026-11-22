# Port analysis: company customizations onto a new fork snapshot

Stage 3 of the upgrade flow, run **inside the company** by a person in their own Claude Code session (SKILL.md), with the agent account for git and GitLab. Produce an **impact report** for a person to review. This stage analyses and trial-ports only: implementation is IMPLEMENT.md, after the review.

```bash
git fetch origin                         # compare the remote branches, not stale local ones
OLD_BASE=origin/security/v0.11.0-clean   # fork snapshot the company is on
NEW_BASE=origin/security/v0.12.0-clean   # new snapshot, already through stages 1 and 2
COMPANY=origin/company/main              # OLD_BASE plus the customizations
NEW=v0.12.0                       # names the report files and branches
```

## Terms

- **Inventory**: `company/customizations.yaml`, the single source of truth for customizations. One entry per feature:

  ```yaml
  features:
    - id: sso-login
      purpose: Sign in with company SSO instead of email codes
      owner: platform-team
      files: [server/internal/handler/sso.go, packages/views/auth/sso-button.tsx]
      seams:
        - server/internal/auth/jwt.go:IssueSession
        - "route: POST /auth/verify"
        - "table: users(email, auth_provider)"
        - "env: JWT_SECRET"
      acceptance: [company/tests/e2e/sso-login.spec.ts, server/internal/handler/sso_test.go]
  ```

- **Seam**: an upstream part a customization depends on (function, type, HTTP route, DB table/column, env var, component prop, event name). A changed seam can break a customization with no git conflict at all.
- **Verdict**: exactly one per feature:
  - `unaffected`: no seam changed between OLD_BASE and NEW_BASE, trial port clean.
  - `mechanical`: textual conflicts or renames only, behaviour unchanged.
  - `behavioral`: a seam's meaning changed (arguments, return values, call timing, schema).
  - `superseded`: upstream now ships the same or a similar feature.
  - `blocked`: a seam was removed or the trial port does not build; a person decides the direction.
- **Evidence**: `file:line` on both versions, or command output. A verdict without evidence is unfinished.
- **Frozen tests**: each feature's `acceptance`. This stage only proposes changes to them, as diffs in the report. The inventory itself is frozen too: proposed inventory changes go in the report. The guard hook (GUARDRAILS.md) blocks writes to frozen paths; a block means "propose it instead".

## Steps

1. **Baseline.** Find NEW_BASE's baseline pipeline in GitLab (PIPELINE.md) and confirm it is green; failures listed in `company/ci/flaky-tests.txt` do not count.
   Done when: you have the green pipeline URL. If it is red, stop and report the failing jobs.

2. **Map commits to features.**

   ```bash
   git log --reverse --format='%h %s' $OLD_BASE..$COMPANY
   git show --stat <commit>
   ```

   Map each commit to a feature by the inventory's `files`; split a commit that spans features by file. Two kinds of commit are not features:
   - **CI**: commits that touch only `company/` (pipeline, flaky list, inventory, fingerprints). They are carried by IMPLEMENT.md step 0; list them, and note any `company/ci/` change made after `baseline/$NEW` was cut, since the baseline has the older copy.
   - **Other company changes** outside any feature's `files` (docs, config): mark them `unmapped` and propose an inventory entry or a `carry` line for the report, so step 0 brings them across.
   Done when: every commit maps to a feature, is a CI commit, or is marked `unmapped` with a proposed inventory entry (id, files, seams) or `carry`.

3. **Check every seam.** Per feature, start from `seams`, then read the customization diff and add the seams the inventory missed:

   ```bash
   git diff $OLD_BASE $COMPANY -- <feature files>
   git diff $OLD_BASE $NEW_BASE -- <seam files>
   git grep -n '<symbol or route>' $NEW_BASE -- server packages apps
   git diff --name-only --diff-filter=A $OLD_BASE $NEW_BASE -- server/migrations/ | grep '\.up\.sql$'
   git log --format='%h %s' $OLD_BASE..$NEW_BASE      # upstream features that may supersede ours
   ```

   Read each new migration for the tables and columns the feature uses.

   Then the upstream tests the company changed: for each entry in `company/upstream-test-changes.yaml`, check what upstream did to that test between the two versions (`git diff $OLD_BASE $NEW_BASE -- <file>`). Unchanged upstream: the company change carries with its feature. Changed upstream: the company change must be redone on the new test, and the entry's `why` checked against it. Deleted or renamed: propose the entry's removal or new path.
   Done when: every seam of every feature is recorded as unchanged or changed, each change with `file:line` on both sides, and every entry in `upstream-test-changes.yaml` is marked unchanged, redo, or drop.

4. **Trial port**, in a separate worktree, so this checkout keeps the guard hook (`company/ci/guard-frozen.py`, which NEW_BASE does not have):

   ```bash
   git worktree add -b port-trial ../port-trial $NEW_BASE && cd ../port-trial
   git cherry-pick $(git rev-list --reverse $OLD_BASE..$COMPANY)   # per conflict: note feature, files, resolution
   (cd server && go build ./... && go vet ./...)
   pnpm install --frozen-lockfile && pnpm typecheck
   # each feature's frozen tests; failures here are evidence for `behavioral`
   (cd server && go test ./internal/<pkg>/ -run '<TestName>' -count=1)
   (cd packages/<pkg> && pnpm exec vitest run <file relative to the package>)
   cd - && git worktree remove --force ../port-trial && git branch -D port-trial
   ```

   A commit that mixes feature code with `company/` files (inventory, fingerprints): keep the code, drop the `company/` part; step 0 carries `company/`. A generated file that conflicts (sqlc's `server/pkg/db/generated/`): regenerate it with `make sqlc` instead of resolving by hand.

   A frozen Go test that needs PostgreSQL (most handler and `cmd/server` tests skip or exit without `DATABASE_URL`): do not start a database on a shared machine. Push the trial branch without a pipeline (`git push -o ci.skip origin port-trial:port-trial/$NEW`), start one pipeline on it with `GO_TEST_PKGS`, `GO_TEST_RUN` and `GO_TEST_COUNT=1` (PIPELINE.md, Flaky list), record the result, then delete the branch.

   Done when: every commit is applied, resolved, or marked unappliable with the reason; build, typecheck and frozen-test results are recorded; the trial worktree and branches are gone.

5. **Verdicts and acceptance criteria.**
   - `unaffected`, `mechanical`: the existing frozen tests stand.
   - `behavioral`, `superseded`, `blocked`: write the behaviour expected on NEW_BASE as observable Given / When / Then lines, each mapped to a test file. A needed frozen-test change goes in as a diff, with the upstream change that forces it.

   Done when: every inventory feature has a verdict with evidence, and every feature not `unaffected` has acceptance criteria.

   Then sort features into two lists for IMPLEMENT.md:
   - **Carry**: the code ports as the trial port left it (`unaffected`, `mechanical` with the recorded resolution, a `behavioral` one whose code needs no change once its tests change, `unmapped` changes marked `carry`). A person carries these in step 0; they get no implementation MR.
   - **Implement**: features whose code must change. Order them so that features breaking the build come first: until they are fixed, every other MR's pipeline is red too.

6. **Cross-feature risks.** New migrations (reversible? cost at production data size? touching customized tables?), added/renamed/removed env vars and settings, removed or changed APIs, CLI commands and webhook formats that outside systems use.
   Done when: every new migration is listed with its impact, "none" included.

7. **Write the report, then stop.** Write `company/porting/reports/$NEW.md` from the template below, and one issue draft per feature on the Implement list in `company/porting/reports/$NEW-issues/` (verdict and evidence, acceptance criteria and test files, expected files to change, dependency order). Commit them on `port-report/$NEW` and open one MR into `company/main` so the review happens on it (push with `-o merge_request.create -o merge_request.target=company/main`, so no separate branch pipeline runs). Drafts only: create no issues, open no implementation MRs.
   Done when: report and drafts are on the review MR. End with the decisions a person must make: every `blocked` feature, whether to drop each `superseded` one, every frozen-test change proposal.

## Report template

```markdown
# Port report: COMPANY → NEW_BASE

Baseline pipeline: <URL> (green)
Trial port: <N> commits, <M> conflicts; build <ok/failed>, typecheck <ok/failed>

## Decisions needed
- [ ] <feature id>: <why blocked / drop superseded? / frozen-test change>

## Carry in step 0
<commits per feature, with conflict resolutions; unmapped changes marked carry>

## Implement, in this order
1. <feature id>: <why first, e.g. breaks the build>

## Verdicts
| Feature | Verdict | Changed seams | Evidence |
|---|---|---|---|

## Per feature
### <feature id>: <verdict>
- Seam changes: OLD_BASE `file:line` → NEW_BASE `file:line`, <what changed>
- Trial port: <conflicts and resolutions>
- Acceptance: Given / When / Then → `test file`
- Frozen-test change proposal (if any): <diff and reason>

## Cross-feature risks
| Item | Detail | Impact |
|---|---|---|

## Inventory updates
<unmapped commits and missed seams>

## Upstream test changes
| File | Feature | Upstream change since OLD_BASE | Action (carry / redo / drop) |
|---|---|---|---|
```
