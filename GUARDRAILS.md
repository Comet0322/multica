# Guardrails for agent work

Set up **inside the company**, once, before an agent does stages 3 and 4 (PORTING.md, IMPLEMENT.md). The pipeline of PIPELINE.md proves the code works; these guardrails keep an agent from changing what proves it, and keep merging with a person.

## Frozen paths

**Frozen paths** are `company/ci/`, `company/customizations.yaml`, `.gitlab-ci.yml`, `.claude/settings.json` and every path in `company/ci/frozen-files.txt`: the pipeline and the acceptance tests, wherever the tests live (`company/tests/`, or next to the feature code). They change only with a person's approval. Three layers enforce it:

1. **Guard hook** (below): the agent cannot write them.
2. **`protected-changes` job** (MR pipelines, warns without blocking): lists changed frozen paths, and separately every other changed test file (`*_test.go`, `*.test.*`, `*.spec.*`), so a person looks at each.
3. **Maintainer**: before merging, look at the MR's changed-files list for frozen paths. This catches what the first two miss, and takes seconds. An MR runs its own branch's pipeline file, so an MR that edits `company/ci/` can also disable checks; only layers 1 and 3 catch that.

**Upstream tests** are not frozen: a port may change one when the feature really changes the behaviour it checks. Every such change is recorded in `company/upstream-test-changes.yaml` (from `templates/upstream-test-changes.yaml`): the file, the feature, and why. The **`test-changes-recorded` job** (MR pipelines, blocking) fails an MR that changes a test, other than a frozen one, without an entry with a reason. The file is not frozen, so the agent writes the entry and the Maintainer reviews it with the change; it lives under `company/`, so it carries to every new version, where PORTING.md re-checks each entry.

**`frozen-tests` job** (MR pipelines, after go-test and frontend): `frozen-tests.sh check` fails when a frozen file changed without new fingerprints, or when a frozen test did not run and pass. That catches `.skip`, `t.Skip` and `vitest.config` exclusions even with regenerated fingerprints. It reads `reports/go.json` and `reports/*.vitest.json`, which the test jobs upload.

Both jobs are in `templates/multica.gitlab-ci.yml`; they run only in MR pipelines, so the baseline never runs them. `frozen-files.txt` and `frozen-tests.sha256` come from `bash ~/.claude/skills/multica-fork/scripts/frozen-tests.sh gen .`, which reads `company/customizations.yaml`.

**Approving a frozen-test change**: a person reviews the proposed diff (port report or MR description), makes it (or starts an agent session with `MULTICA_FROZEN_UNLOCK=1`), and commits it with regenerated fingerprints (`frozen-tests.sh gen .`).

## Guard hook

`scripts/guard-frozen.py` is a Claude Code `PreToolUse` hook. In a repo that has `company/ci/frozen-files.txt` it blocks:
- Edit / Write on a frozen path.
- Bash commands that write one they name: `sed -i`, redirects, `cp`/`mv`/`rm`/`tee`, `git checkout/restore/rm`, interpreter one-liners, `bash -c`.
- `git commit` while a frozen path differs from HEAD, and `git push` of commits that touch one. This is the net for writes it cannot see, such as a script the agent wrote and ran: the script's write succeeds, the commit is refused.

Elsewhere it allows everything. It also refuses writes to Claude Code settings files (the project's `.claude/settings*.json`, the user's `~/.claude/settings*.json`, `/etc/claude-code/`), since removing the hook or setting `disableAllHooks` there would switch it off. It covers Claude Code only: an agent run through another CLI is protected only by layers 2 and 3.

It lives in the repo, so nobody's own settings change and every Claude Code session in the repo gets it:

```bash
cp ~/.claude/skills/multica-fork/scripts/guard-frozen.py company/ci/guard-frozen.py
mkdir -p .claude && cp ~/.claude/skills/multica-fork/templates/claude-settings.json .claude/settings.json
git add -f .claude/settings.json company/ci/guard-frozen.py   # upstream's .gitignore ignores .claude/
git commit -m "company: guard hook in the project settings"
```

`.claude/settings.json` registers `python3 "$CLAUDE_PROJECT_DIR/company/ci/guard-frozen.py"` for Edit, Write, MultiEdit, NotebookEdit and Bash. Both files are frozen, so the hook protects itself. It is outside `company/`, so IMPLEMENT.md step 0 carries it to each new integration branch. When a company needs a guarantee that holds even against someone editing settings by hand, the same command can also go into the managed settings (`/etc/claude-code/managed-settings.json`, root-owned); that is optional.

A person who approved a change and wants an agent session to make it starts that session with `MULTICA_FROZEN_UNLOCK=1`; an `export` inside the session does not unlock it.

## Merge settings

`templates/gitlab-settings.sh apply` sets these (and `check` audits them):

- Protected branches `company/*` and `baseline/*`: Maintainers push and merge, no force push.
- Protected tags `v*`: Maintainers only (CLI releases, IMPLEMENT.md).
- Merge requests: **Pipelines must succeed** on; **Skipped pipelines are considered successful** off, so `[skip ci]` cannot slip through (it creates a pipeline with status `skipped`, and the MR shows `ci_must_pass`).
- The agent's account is Developer: it opens MRs, a Maintainer merges them.

## Acceptance

Each drill counts only when it has actually been run, with the session output, pipeline or MR as the record.

- [ ] `.claude/settings.json` and `company/ci/guard-frozen.py` are committed on `company/main`.
- [ ] In an agent session, overwrite `.claude/settings.json`, or write `.claude/settings.local.json` with `disableAllHooks`: both blocked.
- [ ] `gitlab-settings.sh check` passes.
- [ ] In an agent session, ask for an edit to a frozen test, by Edit and by `sed -i`: both blocked by the hook.
- [ ] In an agent session, have a script the agent writes change a frozen test, then commit: the commit is blocked.
- [ ] In an agent session, `export MULTICA_FROZEN_UNLOCK=1` then `sed -i` a frozen test: still blocked.
- [ ] An MR (pushed by a person, outside the hook) edits a frozen test without new fingerprints: `protected-changes` warns and `frozen-tests` fails; with new fingerprints, `frozen-tests` passes.
- [ ] An MR edits an upstream test without an entry in `company/upstream-test-changes.yaml`: `test-changes-recorded` fails and `protected-changes` lists the test; with an entry (file, feature, why), `test-changes-recorded` passes.
- [ ] An MR adds `.skip` to a frozen test, or excludes it in `vitest.config`, with regenerated fingerprints: `frozen-tests` fails.
- [ ] An MR commit says `[skip ci]`: the MR cannot be merged.
- [ ] The agent account tries to merge its own MR, to push to `company/main`, and to create a `v*` tag: all refused.
