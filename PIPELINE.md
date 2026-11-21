# Upstream green on the company CI (stage 2)

Run **inside the company**, for each new version. The goal is one thing: the imported snapshot `security/$NEW-clean`, with nothing company-specific but the pipeline, passes the company's GitLab CI. Its tests are green and its images build, using only company sources (registry, mirrors). Everything else (guardrails for agents, porting, deployment, CLI release) belongs to later stages.

## What it takes

In the repo, under `company/ci/` (start from `templates/` in this skill):

| File | What it is |
|---|---|
| `multica.gitlab-ci.yml` | the pipeline |
| `images.yml` | every container image's path in the company registry, for this version |
| `ci-tools.Dockerfile` | the CI image the jobs run in |
| `prepare-dockerfile.sh` | writes build copies of upstream's Dockerfiles with company base images and mirrors |
| `flaky-tests.txt` | tests allowed one rerun |

In GitLab: CI/CD file path `company/ci/multica.gitlab-ci.yml`, a masked `PG_PASSWORD`, and the mirror variables (`GOPROXY`, `npm_config_registry`, `COREPACK_NPM_REGISTRY`, plus an apt/apk mirror or `BUILD_PROXY` for image builds). `templates/gitlab-settings.sh` sets them from an env file.

Only if the company's npm policy blocks package versions: a pins commit made by `scripts/npm-unblock.sh` (Blocked npm versions, below).

The template also carries jobs for later stages (`protected-changes` and `frozen-tests` run only in MR pipelines, `cli` only on tags); on the baseline they stay idle.

## Steps

1. **Diagnose**, the first time and whenever the runner, registry or mirrors change: DIAGNOSE.md. It fills `images.yml` and the variables. On later versions, rerun only its step 1: `required-images.sh` shows which upstream images changed.
2. **Baseline branch**, a person's step:

   ```bash
   git fetch origin                                 # a local company/main can be stale
   git switch -c baseline/$NEW origin/security/$NEW-clean
   git checkout origin/company/main -- company/ci   # or templates/ the first time
   # replace company/ci/images.yml with the one for this version
   git commit -m "ci: pipeline for the $NEW baseline"
   git push origin baseline/$NEW
   ```

   If `pnpm install` fails on blocked versions, add the pins as a second commit (Blocked npm versions).
3. **Make it green.** The `ci-tools` job builds the CI image first, then the test jobs and `images` run. Fix each red job with Known problems below. A red job that is none of these is a real failure of the snapshot: report it back to stage 1.
4. **Make it stable.** Run the pipeline three times, once while other pipelines run: starting all three at once covers both (`POST /projects/:id/pipeline?ref=baseline/$NEW`, or Run pipeline in the UI). Add to `flaky-tests.txt` only tests that pass when run alone (Flaky list).
5. **Record** the green pipeline URL; PORTING.md step 1 needs it. Write every deviation from the template, and why, in `company/ci/environment.md`.

Done when: the baseline pipeline is green three times in a row and its URL is recorded.

## What the jobs must do

The template does each of these; keep them when adapting it.

- One pipeline per change: MR pipelines for MRs, branch pipelines otherwise, tag pipelines for version tags (`workflow:rules`).
- Every container image comes from `images.yml`; no job and no build names an upstream registry.
- **ci-tools**: builds the CI image (non-root user, git >= 2.40, the toolchain) when its recipe or `images.yml` changes.
- **go-test**: a PostgreSQL + pgvector service of the chart's major version; migrations, then every Go package. One rerun, allowed only for tests on the flaky list. Writes `reports/go.json` and JUnit XML.
- **sqlc-check**: `make sqlc` leaves no diff.
- **frontend-check**: typecheck and lint (mobile and desktop excluded).
- **frontend**: vitest for `core`, `ui`, `web`, `views`, split into parallel shards (`parallel:`, `vitest --shard`); in each shard, failed files rerun only when all are on the flaky list. Writes `reports/<pkg>-<shard>.vitest.json` and JUnit XML. More shards shorten the pipeline only while the runner has free slots or pods.
- **helm**: `scripts/helm-config.test.sh` (lint and render; deploys nothing).
- **scripts**: `scripts/*.test.sh`; drop the ones the company does not use, and say so in `environment.md`.
- **images**: backend and web images from upstream's Dockerfiles, untouched in the repo; fails when a `FROM` is not mapped in `images.yml`. Building the web image runs `next build`. It runs alongside the tests (`needs: []`); a red test job still makes the pipeline red.

Compare with upstream's `.github/workflows/ci.yml` on each version and record in `environment.md` which of its checks the company also runs. Not in the template: `-race`, `go vet -tags agentintegration`, `govulncheck`, the repo-script and UI checks (`pnpm test:scripts`, knip, catalog, radius, wildcard exports), the generated-file checks (reserved slugs, text-preview whitelist), and running `pkg/agent` on its own with low parallelism (`scripts/test-go.sh --only agent`).

## Known problems

Each was a red pipeline in the simulation. Symptom, cause, fix:

| Symptom | Cause | Fix |
|---|---|---|
| `pkg/agent` Claude tests: "refuses bypassPermissions under root"; `TestMigrateHermesTaskMemoriesFailureKeepsSource` fails | jobs run as root | CI image with a non-root user |
| `TestPrepareLocalWorktree*`: "unknown option merge-base" | git < 2.40 | a base OS with newer git |
| CI image build: `git ... < 2.40: pick a newer IMG_CI_BASE` | base OS too old (e.g. Ubuntu 22.04) | a Debian trixie or Ubuntu 24.04+ base the apt mirror serves |
| CI image build: `Unable to locate package` | apt cannot reach a repository: no mirror for that release, or only `HTTP_PROXY` passed (apt reads lowercase `http_proxy`) | `APT_MIRROR` for a release the mirror has, or both cases of the proxy (the template passes both) |
| `error obtaining VCS status: exit status 128`; `git diff` exits 129 in sqlc-check | checkout cloned by another user; git refuses it | `safe.directory` (the CI image sets it) |
| `go run gotestsum@v1.12.0`: "invalid array length" | v1.12.0 does not build with Go 1.26 | v1.13.0 or later, in the CI image |
| `selfhost-config.test.sh`: "unknown flag: --env-file" | docker CLI without the compose plugin | the compose plugin; no daemon needed |
| `permission denied` under the Go or pnpm cache | cache created by a root image | caches in the project dir (`cache:`), as the template does |
| job cannot start: image pull fails | path or tag not in the registry | fix `images.yml`, substitute by its rule, or ask for the image |
| `images`: `FROM not mapped in company/ci/images.yml` | the new version changed a base image | rerun `required-images.sh`, map it |
| `images`: `apk` cannot fetch | Alpine repository unreachable | `APK_MIRROR` for that Alpine release, or `BUILD_PROXY` |
| `images`: `go mod download` / `pnpm install` / `corepack` fail although the mirror variables are set | upstream Dockerfiles do not declare them as `ARG`; corepack ignores `HTTPS_PROXY` | `prepare-dockerfile.sh` adds the `ARG`s; `COREPACK_NPM_REGISTRY` |
| `images`: web build fails fetching `fonts.googleapis.com` | `next/font/google` downloads fonts during `next build` | the snapshot lacks stage 1's offline-fonts fix (UPGRADE.md): report it back |
| `make sqlc` cannot verify `sqlc@<version>` | the Go proxy does not serve `sum.golang.org` | a proxy that does, or the company's `GONOSUMDB` policy |
| pnpm install fails on release age | npm mirror without publish times (`.npmrc` `min-release-age`) | a mirror that serves `time` |
| pnpm install: `ERR_PNPM_FETCH_403` | the security policy blocks that version | Blocked npm versions |
| views tests time out at 5 s, a different file each run | too many vitest workers for the pod's CPU | `VITEST_WORKERS`, `GO_TEST_P` from the CPU limit (DIAGNOSE.md) |
| a different `pkg/agent` test fails each run | subprocess tests with hard deadlines under load | the package prefix on the flaky list, or upstream's throttled `test-go.sh --only agent` |
| several `internal/handler` `TestClaimTask*` fail with "expected task in response" (more than 5 failures, so nothing is rerun) | the test runtime's heartbeat is refreshed only now and then, and a claim needs one within 150 s (`RuntimeClaimFreshnessSeconds`); the package ran slowly under load (over 5 min) | lower `GO_TEST_P`, or fewer pipelines at once on the runner; they pass alone |

## Flaky list

`company/ci/flaky-tests.txt`: one entry per line, matched as a substring against the Go rerun report (`pkg.TestName: 2 runs, 1 failures`) and the failed vitest file paths. An entry is a Go test name, a test file path, or a package prefix such as `github.com/multica-ai/multica/server/pkg/agent.`. Add one only after the test passes when run alone (`go test ./<pkg> -run '<name>' -count=5`, `pnpm exec vitest run <file>`). A Go test that needs PostgreSQL can be run alone in CI instead of locally: start a pipeline on the branch with `GO_TEST_PKGS=./internal/<pkg>`, `GO_TEST_RUN=<regex>` and `GO_TEST_COUNT=5` (only go-test and the cheap checks run). Do not start a local database for it on a shared machine. The list depends on the runner's load, not on the code. Entries seen in the simulation, as a start:

```text
github.com/multica-ai/multica/server/pkg/agent.
TestMcodeWaitsForSessionNewReadinessAfterInitialize
packages/views/issues/components/issue-detail.test.tsx
packages/views/issues/components/issues-header.property-filter.test.tsx
packages/views/projects/components/project-date-pickers.test.tsx
packages/views/inbox/components/inbox-snooze-menu.test.tsx
packages/views/issues/components/annotation-reply-interactions.test.tsx
packages/views/issues/components/pickers/custom-property-picker.select.test.tsx
packages/views/search/search-command.test.tsx
packages/views/common/date-only-picker.test.tsx
```

## Blocked npm versions

The company's security policy can block package versions, direct or transitive (HTTP 403 on the tarball); a nearby version of the same major always exists. Pin it rather than editing dependencies by hand:

```bash
npm_config_registry=<company npm mirror> bash ~/.claude/skills/multica-fork/scripts/npm-unblock.sh company/npm-pins.txt
```

It installs from a fresh store (cached tarballs hide blocks). For each blocked `name@version` it adds a `pnpm.overrides` entry to the nearest fetchable version of the same major (same minor first), regenerates `pnpm-lock.yaml` and checks the blocked version is gone; a package resolved through the pnpm catalog needs a bare-name override, which it uses only when every resolved version of that package has the same major. It stops, for a person to decide, when that does not hold or when no version of the major is fetchable. Each pin is appended to `company/npm-pins.txt`.

- Commit the result on the baseline (`package.json`, `pnpm-lock.yaml`, `company/npm-pins.txt`). Pins are not ported: each version reruns the script on its own baseline.
- Take `npm-pins.txt` out to stage 1: the same overrides in the fork's `package.json` let the next snapshot install as imported.

## Acceptance

Each drill counts only when it has actually been run, with the pipeline as the record.

- [ ] DIAGNOSE.md done; `images.yml` and `environment.md` in `company/ci/`.
- [ ] The baseline pipeline is green three times in a row, once while other pipelines run.
- [ ] A test not on the flaky list fails, even if its rerun passes: the pipeline is red.
- [ ] A flaky-list test fails once: it is rerun and the pipeline passes (one Go test, one vitest file).
- [ ] The `images` job builds both images using only the company registry and mirrors.
- [ ] A `FROM` not in `images.yml` makes `images` fail with its name.
- [ ] A sqlc query changed without regenerating: `sqlc-check` fails.
