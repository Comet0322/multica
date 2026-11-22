# Guardrails for agent work

Set up **inside the company**, once, before an agent does stages 3 and 4 (PORTING.md, IMPLEMENT.md). The pipeline of PIPELINE.md proves the code works; these settings keep merging with a person, who reviews every change to a test.

## Tests in an MR

CI runs every test in the tree (`go test ./...`, vitest in `core`, `ui`, `web`, `views`), so a new or moved test needs no pipeline change. What a port may not do is make a failing test pass by weakening it. The Maintainer checks this in the MR's changed-files list: every changed test file (`*_test.go`, `*.test.*`, `*.spec.*`), and any change to `company/ci/`, `.gitlab-ci.yml`, a `vitest.config.*` or `playwright*.config.ts` (a skip or an exclusion there hides a test). An MR runs its own branch's pipeline file, so only that review catches an MR that edits the pipeline.

**Company acceptance tests** are the `acceptance` files of each feature in `company/customizations.yaml`. A port changes one only as the approved port report says (PORTING.md proposes the change as a diff).

**Upstream tests** may change when the feature really changes the behaviour they check; the MR description says which and why, for the Maintainer. PORTING.md finds them again on the next version from the git history.

## GitLab settings

A Maintainer sets these once in the project, by hand (GitLab Free):

| Where | Setting |
|---|---|
| Settings → General → Visibility | Container registry on |
| Settings → General → Merge requests | **Pipelines must succeed** on; **Skipped pipelines are considered successful** off, so `[skip ci]` cannot slip through (it creates a pipeline with status `skipped`, and the MR shows `ci_must_pass`) |
| Settings → CI/CD → General pipelines | CI/CD configuration file `company/ci/multica.gitlab-ci.yml` |
| Settings → Repository → Branch defaults | Default branch `dev` (once it exists) |
| Settings → Repository → Protected branches | `dev`, `company/*`, `baseline/*`: allowed to merge and to push Maintainers, force push off. The one exception is the go-live move of `dev` (IMPLEMENT.md step 6), with force push on for that push only |
| Settings → Repository → Protected tags | `v*`: allowed to create Maintainers (CLI releases) |
| Settings → CI/CD → Variables | `PG_PASSWORD`, masked, from `openssl rand -hex 16`: the test database password. The mirror variables PIPELINE.md lists (`GOPROXY`, `npm_config_registry`, `COREPACK_NPM_REGISTRY`, and as DIAGNOSE.md decides `APT_MIRROR`, `APK_MIRROR`, `BUILD_PROXY`, `NO_PROXY_HOSTS`, `IMAGE_REPO`, `PUSH_REGISTRY`, `PUSH_USER`, masked `PUSH_PASSWORD`, `KANIKO_EXTRA`, `GO_TEST_P`, `VITEST_WORKERS`) |
| Manage → Members | The agent's account is Developer: it opens MRs, a Maintainer merges them |

## Acceptance

Each drill counts only when it has actually been run, with the pipeline or MR as the record.

- [ ] Every row of the GitLab settings table matches the project, checked on its page.
- [ ] An MR commit says `[skip ci]`: the MR cannot be merged.
- [ ] The agent account tries to merge its own MR, to push to `dev`, and to create a `v*` tag: all refused.
