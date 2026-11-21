---
name: multica-fork
description: Maintain the hardened Multica fork and bring it into the company. Use when the user asks to upgrade the Multica fork to a new upstream release; to port company customizations onto a new fork version or implement an approved port; or to set up or audit the company GitLab pipeline for the fork.
---

# Multica fork

The fork tracks upstream Multica with security fixes on top, so it passes the company's SAT, virus and secret scans. Each upstream release goes through four stages; each lives in its own file. Read the one for the stage at hand:

| Stage | Where | Read |
|---|---|---|
| 1. Upgrade the fork to a new upstream tag: replay fixes, build offline, scan, clean snapshot, push | outside the company | [UPGRADE.md](UPGRADE.md) |
| 2. Baseline: the imported snapshot green on the company CI, tests and images | inside | [PIPELINE.md](PIPELINE.md); first time, or when the runner, registry or mirrors change: [DIAGNOSE.md](DIAGNOSE.md) |
| Before an agent does stages 3 and 4: frozen tests, guard hook, merge settings (once) | inside | [GUARDRAILS.md](GUARDRAILS.md) |
| 3. Port analysis: impact of the new version on company customizations, for review | inside | [PORTING.md](PORTING.md) |
| 4. Implement the approved port: MRs, test environment, migration rehearsal | inside | [IMPLEMENT.md](IMPLEMENT.md) |

Stages 3 and 4 are run by a person, in their own Claude Code session (on the company's private model) in a clone of the company project, with the agent account (Developer) for git and the GitLab API. Not from CI: each stage stops for a person's decision (the report review, the approved test changes, step 0, every merge), and stage 4 waits on many pipelines. The guard hook comes with the repo (GUARDRAILS.md), so it is active in that session.

Scanning belongs to the **security-scan** skill, which is generic and works for any repo; stage 1 loads it. The checks that only make sense for Multica, such as building the web image with no internet (fonts) and the company's npm pins, stay in this skill (UPGRADE.md, step 4).

## Branches

- `security/vX.Y.Z`: the upstream tag plus the fix commits. Its history keeps the fixes reviewable and replayable.
- `security/vX.Y.Z-clean`: one parentless commit with the same tree. This is what the company imports, because its git history holds no credential-looking strings from earlier commits.

In the company project:
- `baseline/vX.Y.Z`: the clean snapshot plus `company/ci/`, the stage 2 run.
- `port-report/vX.Y.Z`: the stage 3 report, reviewed as an MR.
- `company/vX.Y.Z`: the integration branch, baseline plus `company/` carried over; `port/vX.Y.Z/<feature id>` MRs target it.
- `company/main`: what runs in production. Each clean snapshot is a parentless commit, so a new version shares no history with the old one: going live moves `company/main` to `company/vX.Y.Z`, and the old tip is kept as the tag `company/vOLD-final`. Customizations carry across by being ported again, which is why the inventory must stay complete.

## Installing

The skill is the fork's orphan branch `skill/multica-fork`, which holds nothing else and keeps its own history, apart from the fork's. Inside the company, take it from the public fork the same way as the snapshot:

```bash
git clone --single-branch -b skill/multica-fork https://github.com/Comet0322/multica ~/.claude/skills/multica-fork
```

Update it with `git -C ~/.claude/skills/multica-fork pull`. Changes are committed and pushed outside the company; anything found inside is written down and brought out.

## Files here

- `accepted-rules/multica.txt`: Semgrep/gosec rules reviewed and accepted for the fork, each with its reason. Stage 1 compares each scan against it.
- `scripts/fixlock.sh`: regenerates `pnpm-lock.yaml` under the fork's supply-chain settings (7-day release age, no provenance downgrade).
- `scripts/clean-snapshot.sh`: builds the `-clean` branch and checks its history with Gitleaks.
- `scripts/frozen-tests.sh`: fingerprints the company's frozen acceptance tests and checks them in CI.
- `scripts/npm-unblock.sh`: inside the company, pins the nearest fetchable version for each npm package version the security policy blocks.
- `scripts/guard-frozen.py`: Claude Code hook that blocks the agent from writing frozen paths; committed to the repo as `company/ci/guard-frozen.py` and registered by `.claude/settings.json` (`templates/claude-settings.json`), as GUARDRAILS.md says.
- `patches/offline-fonts-v0.11.0.patch`: the offline-fonts fix as first made on v0.11.0 (UPGRADE.md, step 2).
- `templates/`: the reference implementation of the company pipeline, run green on GitLab CE with the runner offline and images from a Harbor-like registry; PIPELINE.md says what it must keep doing.
  - `multica.gitlab-ci.yml`: the pipeline; `ci-tools.Dockerfile`: the CI image it runs in; `prepare-dockerfile.sh`: build copies of upstream Dockerfiles with company base images and mirrors.
  - `required-images.sh`: the images a version needs, with substitute rules; drafts `images.yml`.
  - `probe.gitlab-ci.yml`: the environment probe of DIAGNOSE.md.
  - `gitlab-settings.sh` and `gitlab-settings.env`: apply or audit the GitLab project settings.
  - `claude-settings.json`: the project settings that register the guard hook (GUARDRAILS.md).
  - `upstream-test-changes.yaml`: the record of upstream tests the company changed, which the `test-changes-recorded` job enforces (GUARDRAILS.md).
