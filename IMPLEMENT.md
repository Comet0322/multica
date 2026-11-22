# Implement an approved port

Stage 4 of the upgrade flow, run **inside the company** by a person in their own Claude Code session (SKILL.md), after a person approved the port report (PORTING.md). Steps 0 and 6 and every merge are the person's; the agent account does steps 1 to 5. Input: the approved report `company/porting/reports/$NEW.md` (its Carry and Implement lists) and the issue drafts in `company/porting/reports/$NEW-issues/`. Result: every feature on `company/$NEW`, the build-changing ones through one green MR each, deployed to the test environment, with the migration rehearsed.

## Rules

- One feature from the Implement list per MR. Branch each MR from the integration branch `company/$NEW` (step 0), which is green, and target it; the MR brings the feature's commits from `dev`, its acceptance tests (with the approved changes) and the code change. MRs do not depend on each other, so they can merge in any order; rebase an open MR after another one merges.
- Carry-list features need no MR: step 0 brings their code across. If one turns out not to build or pass its tests on `company/$NEW`, leave it out of step 0 and report it; it moves to the Implement list.
- **Acceptance tests** (each feature's `acceptance` in the inventory): make the feature meet them. Change one only as the approved report says, in that feature's MR. When one seems wrong otherwise, stop and report it with the diff you would want.
- Change an upstream test only when the feature truly changes the behaviour it checks, and say which and why in the MR description. Never weaken a test, or skip or exclude one, to make it pass (GUARDRAILS.md).
- The evidence is what CI produced: the JUnit test reports of the MR pipeline. Link them in the MR; your own summary is not evidence.
- You open MRs; a Maintainer merges them.
- Stop background processes you started (load loops, port-forwards) by their PID, not with `pkill -f`: a pattern also matches the port-forward to the copy and other people's processes.

## Steps

0. **Integration branch**, a person's step (a Maintainer: `company/*` is protected). First bring the report MR up to the review's decisions (Carry and Implement lists, approved test changes, issue drafts) and merge it into `dev`, so the report reaches `company/$NEW` with `company/` and the agent can read it there. Then start from the baseline, carry `company/` and the Carry features with their acceptance tests (they may live outside `company/`), apply what the review approved for them, and nothing of the Implement features:

   ```bash
   git fetch origin                     # a local branch can be stale: use origin/
   git switch -c company/$NEW origin/baseline/$NEW
   git checkout origin/dev -- company
   git checkout origin/baseline/$NEW -- company/ci       # keep the stage 2 CI fixes (images.yml, flaky list)
   git diff origin/baseline/$NEW origin/dev -- company/ci
   CARRY="<Carry feature ids, space-separated>"
   tests=$(CARRY=$CARRY yq -r '.features[] | select(.id as $i | strenv(CARRY) | split(" ") | any_c(. == $i)) | .acceptance[]' company/customizations.yaml)
   git checkout origin/dev -- $tests
   # apply the approved inventory updates, and the approved test changes of Carry features, here
   git add -A company $tests
   git commit -m "company: carry customizations inventory, tests and CI to $NEW"
   # Carry list: the Carry features' commits, with the report's recorded resolutions;
   # on a conflict in an acceptance test, keep the version committed above (ours)
   git cherry-pick <carry commits>
   # every company edit to an upstream test made it across
   OLD_BASE=origin/security/$OLD-clean   # the snapshot dev is on, as in PORTING.md
   for f in $(git diff --name-only --diff-filter=M $OLD_BASE origin/dev -- '*_test.go' '*.test.*' '*.spec.*'); do
     git diff $OLD_BASE origin/dev -- "$f" | grep '^+[^+]' | cut -c2- |
       while IFS= read -r l; do grep -qxF -- "$l" "$f" || echo "$f: missing: $l"; done
   done
   git push origin company/$NEW
   ```

   `company/ci` comes from the baseline, not from `dev`: stage 2 fixed it for the new version. The `git diff` lists what differs from `dev`: a stage 2 fix stays as it is; a `dev` change made after the baseline was cut is brought over by hand.

   The company's edits to upstream tests ride in the feature commits, often in commits that look company-only. The loop prints each line of such an edit that is missing on `company/$NEW`; every line it prints must belong to an Implement feature, whose MR brings it. Where upstream changed the test, the report's Upstream test changes table says how the edit was redone; check those lines by hand.

   Done when: `company/$NEW` exists with `company/` (its `company/ci` from the baseline) and the Carry features, their acceptance tests and upstream-test edits, and its pipeline is green. A red job means a Carry feature does not port as the report said: move it to the Implement list and leave it out.

1. **Implement each issue** on its own branch. Run locally before pushing: `go build ./... && go vet ./...`, `pnpm typecheck`, and the feature's acceptance tests.
   Done when: the feature's acceptance tests pass locally.

2. **Open the MR** with: the issue, the verdict, the acceptance criteria, and a link to the pipeline's test report.
   Done when: the pipeline is green. The integration branch is green, so every red job is this MR's; a red job you cannot fix without an unapproved test change goes back to the person.

3. **Back up the copy and rehearse the migration** on a production-like copy, never production, from `company/$NEW` once the MRs are merged. Do it before step 4: the backend runs `migrate up` when it starts, so a deploy against the copy would migrate it before there is a backup. Lock waits only show under traffic, so run a write load against the copy while migrating (for example the app's own load test, or a loop of issue inserts and updates):

   ```bash
   pg_dump -Fc "$COPY_URL" -f before-$NEW.dump
   time (cd server && DATABASE_URL="$COPY_URL" go run ./cmd/migrate up)
   # meanwhile, in another shell, every second: waiting locks
   psql "$COPY_URL" -c "select pid, mode, relation::regclass, now()-query_start age
                        from pg_locks join pg_stat_activity using (pid) where not granted;"
   ```

   Record the time of each migration the report's cross-feature risks flag as costly (a `VALIDATE`, a rewrite or a full-table scan on a large table), not only the total. Done when: the migration time and any lock waits under load are recorded.

4. **Deploy to the test environment** once the MRs are merged into the integration branch and its pipeline has built the images. The project registry is private and the chart has no `imagePullSecrets`, so give the namespace's default service account a pull secret from a deploy token with `read_registry`:

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

   Use the chart of the version being deployed (`deploy/helm/multica` from that branch). Then rehearse the rollback from the backup, not with `migrate down`. Restore into an empty schema: `pg_restore --clean` drops only what the dump contains, so the tables the new migrations created stay behind while `schema_migrations` goes back, and the next `migrate up` fails on them (`relation ... already exists`). Stop the backend before touching the schema and start the old one only on a complete restore: a backend that starts on an empty or half-restored schema migrates it.

   ```bash
   kubectl -n $NS scale deploy/multica-backend --replicas=0
   psql "$COPY_URL" -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
   pg_restore --no-owner --exit-on-error -d "$COPY_URL" before-$NEW.dump || exit 1   # on failure: fix, drop and restore again
   helm rollback multica <previous revision> -n $NS --wait
   kubectl -n $NS scale deploy/multica-backend --replicas=1 && kubectl -n $NS rollout status deploy/multica-backend
   ```

   Then run `migrate up` once more on the restored copy: it must apply the new migrations cleanly, as the real upgrade will after a rollback. Deploy the new version again.

   Done when: all pods Ready and `/healthz` reports the database and migrations ok, the rollback brought the old version back up against the restored data, the second `migrate up` succeeded, and the new version runs again.

5. **Hand over for manual testing**: list what the automated tests do not cover, per feature. Each problem found by hand becomes a new automated test in a follow-up MR.

6. **Go live and release the CLI**, a person's step: move `dev` to `company/$NEW` (keep the old tip as the tag `company/vOLD-final`), then tag the live commit `v$NEW-company.<n>`, with `<n>` one above the highest existing `v$NEW-company.*` tag (`.1` for the first). `company/$NEW` grew from the baseline, not from `dev`, so the move is a force push to a protected branch: allow it on the `dev` rule only for that push. Work goes on on `dev` during the upgrade, and the force push drops whatever `company/$NEW` lacks: stop merging into `dev` for this step, and list what landed after the analysed commit (the report's `Analysed:` line). Each of those changes must be on `company/$NEW` first, through an MR like step 1; the report's own merge (step 0) needs nothing. Both commits are already tested, so push the branch and the `-final` tag without a pipeline:

   ```bash
   git fetch origin --tags
   git log --oneline <analysed commit>..origin/dev           # each change must already be on company/$NEW
   git ls-remote --tags origin "v$NEW-company.*"          # pick <n>
   git tag -a company/v$OLD-final -m "dev before $NEW" origin/dev
   git push -o ci.skip origin company/v$OLD-final
   GITLAB=<GitLab URL>; PROJECT=<project id>; TOKEN=$(cat <your token file>)
   P="$GITLAB/api/v4/projects/$PROJECT/protected_branches/dev"
   curl -sf -X PATCH -H "PRIVATE-TOKEN: $TOKEN" "$P" -d allow_force_push=true
   git push -o ci.skip --force origin origin/company/$NEW:refs/heads/dev
   curl -sf -X PATCH -H "PRIVATE-TOKEN: $TOKEN" "$P" -d allow_force_push=false
   git tag -a v$NEW-company.<n> -m "v$NEW-company.<n>" origin/company/$NEW
   git push origin v$NEW-company.<n>                      # this one runs the tag pipeline
   ```

   Check afterwards that the rule allows no force push again. Never move or delete a tag that already has a Release. Done when: the tag pipeline is green, the Release lists every platform's archive, and one downloaded archive matches `checksums.txt` and prints the new version with `multica version`.

   The tag pipeline runs the tests and the `cli` job, which builds the `multica` CLI for the platforms in `CLI_PLATFORMS` with upstream's release toolchain and `-ldflags`, and publishes the archives and `checksums.txt` to the Package Registry and a Release. The upstream `.goreleaser.yml` stays unused: it publishes to GitHub and Homebrew. A fix to the pipeline needs a new commit and a new tag; retrying an old tag's pipeline reuses that commit's pipeline file. `multica update` asks GitHub for upstream releases and cannot reach it behind the firewall, so installed CLIs are updated from the company Release.

## Report

Reply with: each MR and its pipeline, the Carry list and who carried it, test changes and who approved them, the test environment version and `/healthz` result, migration time, lock waits and rollback result, the manual-test list, and the CLI Release.
