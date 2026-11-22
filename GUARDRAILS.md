# Guardrails for agent work

Set up **inside the company**, once, before an agent does stages 3 and 4 (PORTING.md, IMPLEMENT.md). The pipeline of PIPELINE.md proves the code works; these settings keep merging with a person, who reviews every change to a test.

## Tests in an MR

CI runs every test in the tree (`go test ./...`, vitest in `core`, `ui`, `web`, `views`), so a new or moved test needs no pipeline change. What a port may not do is make a failing test pass by weakening it. The Maintainer checks this in the MR's changed-files list: every changed test file (`*_test.go`, `*.test.*`, `*.spec.*`), and any change to `company/ci/`, `.gitlab-ci.yml`, a `vitest.config.*` or `playwright*.config.ts` (a skip or an exclusion there hides a test). An MR runs its own branch's pipeline file, so only that review catches an MR that edits the pipeline.

**Company acceptance tests** are the `acceptance` files of each feature in `company/customizations.yaml`. A port changes one only as the approved port report says (PORTING.md proposes the change as a diff).

**Upstream tests** may change when the feature really changes the behaviour they check; the MR description says which and why, for the Maintainer. PORTING.md finds them again on the next version from the git history.

## Merge settings

`templates/gitlab-settings.sh apply` sets these (and `check` audits them):

- Protected branches `company/*` and `baseline/*`: Maintainers push and merge, no force push.
- Protected tags `v*`: Maintainers only (CLI releases, IMPLEMENT.md).
- Merge requests: **Pipelines must succeed** on; **Skipped pipelines are considered successful** off, so `[skip ci]` cannot slip through (it creates a pipeline with status `skipped`, and the MR shows `ci_must_pass`).
- The agent's account is Developer: it opens MRs, a Maintainer merges them.

## Acceptance

Each drill counts only when it has actually been run, with the pipeline or MR as the record.

- [ ] `gitlab-settings.sh check` passes.
- [ ] An MR commit says `[skip ci]`: the MR cannot be merged.
- [ ] The agent account tries to merge its own MR, to push to `company/main`, and to create a `v*` tag: all refused.
