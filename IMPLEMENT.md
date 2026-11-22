# Implement an approved port

Stage 4 of the upgrade flow, run **inside the company** by a person in their own Claude Code session (SKILL.md), after a person approved the port report (PORTING.md). Steps 0 and 6 and every merge are the person's; the agent account does steps 1 to 5. Input: the approved report `company/porting/reports/$NEW.md` (its Carry and Implement lists) and the issue drafts in `company/porting/reports/$NEW-issues/`. Result: every feature on `company/$NEW`, the build-changing ones through one green MR each, deployed to the test environment, with the migration rehearsed.

## Rules

- One feature from the Implement list per MR, in the report's order: features that break the build come first, because until they are merged every other MR's pipeline is red on their account. Branch each MR from the integration branch `company/$NEW` (step 0) and target it. Rebase an MR onto the integration branch after an earlier one merges.
- Carry-list features need no MR: step 0 brings their code across. If one turns out not to build or pass its acceptance tests on `company/$NEW`, report it; it moves to the Implement list.
- **Acceptance tests** (each feature's `acceptance` in the inventory): make the feature meet them. Change one only as the approved report says, in that feature's MR. When one seems wrong otherwise, stop and report it with the diff you would want.
- Change an upstream test only when the feature truly changes the behaviour it checks, and say which and why in the MR description. Never weaken a test, or skip or exclude one, to make it pass (GUARDRAILS.md).
- The evidence is what CI produced: the JUnit test reports of the MR pipeline. Link them in the MR; your own summary is not evidence.
- You open MRs; a Maintainer merges them.

## Steps

0. **Integration branch**, a person's step (a Maintainer: `company/*` is protected): start from the baseline, carry `company/` and every acceptance test over (they may live outside `company/`), apply what the review approved, and bring the Carry list across:

   ```bash
   git fetch origin                     # a local branch can be stale: use origin/
   git switch -c company/$NEW origin/baseline/$NEW
   git checkout origin/company/main -- company
   git checkout origin/baseline/$NEW -- company/ci       # keep the stage 2 CI fixes (images.yml, flaky list)
   git diff origin/baseline/$NEW origin/company/main -- company/ci
   git checkout origin/company/main -- $(yq -r '.features[].acceptance[]' company/customizations.yaml)
   # apply the approved inventory updates, and the approved test changes of Carry features, here
   git add -A company $(yq -r '.features[].acceptance[]' company/customizations.yaml)
   git commit -m "company: carry customizations inventory, tests and CI to $NEW"
   # Carry list: the report's feature commits, with its recorded resolutions;
   # on a conflict in an acceptance test, keep the version committed above (ours)
   git cherry-pick <carry commits>
   # every company edit to an upstream test made it across
   OLD_BASE=origin/security/$OLD-clean   # the snapshot company/main is on, as in PORTING.md
   for f in $(git diff --name-only --diff-filter=M $OLD_BASE origin/company/main -- '*_test.go' '*.test.*' '*.spec.*'); do
     git diff $OLD_BASE origin/company/main -- "$f" | grep '^+[^+]' | cut -c2- |
       while IFS= read -r l; do grep -qxF -- "$l" "$f" || echo "$f: missing: $l"; done
   done
   git push origin company/$NEW
   ```

   `company/ci` comes from the baseline, not from `company/main`: stage 2 fixed it for the new version. The `git diff` lists what differs from `company/main`: a stage 2 fix stays as it is; a `company/main` change made after the baseline was cut is brought over by hand.

   The company's edits to upstream tests ride in the feature commits, often in commits that look company-only. The loop prints each line of such an edit that is missing on `company/$NEW`; it must print nothing. Where upstream changed the test, the report's Upstream test changes table says how the edit was redone; check those lines by hand.

   Done when: `company/$NEW` exists with `company/` (its `company/ci` from the baseline), the acceptance tests, the company's upstream-test edits and the Carry list on it. Its pipeline stays red until the Implement list is merged; that is the work of the steps below.

1. **Implement each issue** on its own branch. Run locally before pushing: `go build ./... && go vet ./...`, `pnpm typecheck`, and the feature's acceptance tests.
   Done when: the feature's acceptance tests pass locally.

2. **Open the MR** with: the issue, the verdict, the acceptance criteria, and a link to the pipeline's test report.
   Done when: the pipeline is green. A red job caused by another Implement feature not merged yet is not yours: say which, and wait for that MR. A red job you cannot fix without an unapproved test change goes back to the person.

3. **Deploy to the test environment** once the MRs are merged into the integration branch and its pipeline has built the images. The project registry is private and the chart has no `imagePullSecrets`, so give the namespace's default service account a pull secret from a deploy token with `read_registry`:

   ```bash
   NS=multica-test; TAG=<image tag CI built>
   kubectl get namespace $NS || kubectl create namespace $NS
   kubectl -n $NS get secret multica-secrets || kubectl -n $NS create secret generic multica-secrets \
     --from-literal=JWT_SECRET="$(openssl rand -hex 32)" --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 16)" \
     --from-literal=RESEND_API_KEY="" --from-literal=GOOGLE_CLIENT_SECRET="" \
     --from-literal=CLOUDFRONT_PRIVATE_KEY="" --from-literal=MULTICA_DEV_VERIFICATION_CODE=""
   kubectl -n $NS get secret registry-pull || kubectl -n $NS create secret docker-registry registry-pull \
     --docker-server=$REGISTRY_HOST --docker-username=<deploy token user> --docker-password="$(cat <deploy token file>)"
   kubectl -n $NS patch serviceaccount default -p '{"imagePullSecrets":[{"name":"registry-pull"}]}'
   helm upgrade --install multica deploy/helm/multica -n $NS --wait \
     --set images.backend.repository=$REGISTRY/multica-backend --set images.backend.tag=$TAG \
     --set images.frontend.repository=$REGISTRY/multica-web --set images.frontend.tag=$TAG
   kubectl -n $NS port-forward svc/multica-backend 8080:8080 & sleep 2
   curl -s localhost:8080/healthz
   ```

   Use the chart of the version being deployed (`deploy/helm/multica` from that branch). Done when: all pods Ready and `/healthz` reports the database and migrations ok.

4. **Rehearse the migration** on a production-like copy, never production. Lock waits only show under traffic, so run a write load against the copy while migrating (for example the app's own load test, or a loop of issue inserts and updates):

   ```bash
   pg_dump -Fc "$COPY_URL" -f before-$NEW.dump
   time (cd server && DATABASE_URL="$COPY_URL" go run ./cmd/migrate up)
   # meanwhile, in another shell, every second: waiting locks
   psql "$COPY_URL" -c "select pid, mode, relation::regclass, now()-query_start age
                        from pg_locks join pg_stat_activity using (pid) where not granted;"
   ```

   Roll back from the backup, not with `migrate down`. Restore into an empty schema: `pg_restore --clean` drops only what the dump contains, so the tables the new migrations created stay behind while `schema_migrations` goes back, and the next `migrate up` fails on them (`relation ... already exists`).

   ```bash
   helm rollback multica <previous revision> -n $NS --wait
   kubectl -n $NS scale deploy/multica-backend --replicas=0
   psql "$COPY_URL" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
   pg_restore --no-owner --exit-on-error -d "$COPY_URL" before-$NEW.dump
   kubectl -n $NS scale deploy/multica-backend --replicas=1
   ```

   Then run `migrate up` once more on the restored copy: it must apply the new migrations cleanly, as the real upgrade will after a rollback.

   Done when: the migration time and any lock waits under load are recorded, the rollback brought the old version back up against the restored data, and the second `migrate up` succeeded.

5. **Hand over for manual testing**: list what the automated tests do not cover, per feature. Each problem found by hand becomes a new automated test in a follow-up MR.

6. **Go live and release the CLI**, a person's step: move `company/main` to `company/$NEW` (keep the old tip as the tag `company/vOLD-final`), then tag the live commit `v$NEW-company.<n>` (e.g. `v0.11.0-company.1`). The tag pipeline runs the tests and the `cli` job, which builds the `multica` CLI for the platforms in `CLI_PLATFORMS` with upstream's release toolchain and `-ldflags`, and publishes the archives and `checksums.txt` to the Package Registry and a Release. The upstream `.goreleaser.yml` stays unused: it publishes to GitHub and Homebrew. A fix to the pipeline needs a new commit and a new tag; retrying an old tag's pipeline reuses that commit's pipeline file. `multica update` asks GitHub for upstream releases and cannot reach it behind the firewall, so installed CLIs are updated from the company Release.

   Done when: the Release exists, and the downloaded binary for one platform reports that version (`multica version`).

## Report

Reply with: each MR and its pipeline, the Carry list and who carried it, test changes and who approved them, the test environment version and `/healthz` result, migration time, lock waits and rollback result, the manual-test list, and the CLI Release.
