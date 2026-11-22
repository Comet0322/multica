# Diagnose the company CI environment

Run **inside the company**, before the first baseline (PIPELINE.md) and again when the runner, the registry or the mirrors change. The company environment cannot be known from outside: which images the registry (Harbor) holds and under which paths, what the Kubernetes runner allows, what the mirrors can do. This stage finds out by running things there, and turns the answers into `company/ci/images.yml`, CI/CD variables and a short `company/ci/environment.md`.

Nothing here needs a person's approval: it runs on a throwaway branch `probe/env`.

```bash
NEW=v0.12.0                               # the version the pipeline is for
FORK=~/.claude/skills/multica-fork
```

## 1. Image list

On the version's snapshot, list what it needs and the substitute rule for each image:

```bash
git switch -c probe/env security/$NEW-clean
mkdir -p company/ci
bash $FORK/templates/required-images.sh .                 # table: ask for what is missing
bash $FORK/templates/required-images.sh . --yaml > company/ci/images.yml
```

For each `IMG_*` in `images.yml`, find the image in the registry and replace the value with its full path. Keep each `UPSTREAM_IMG_*` line as generated: the images job maps `FROM <upstream>` to the value. When the exact tag is missing, pick one that satisfies the substitute rule in the comment, and add a comment saying what was substituted and why. When nothing satisfies the rule, ask the registry's owners for the image; note it in `environment.md`.

`IMG_CI_TOOLS` is the image this project builds; point it at where built images go (the project's GitLab registry, or a Harbor project the CI can push to).

Done when: every `IMG_*` names a registry path, and every substitute has its reason.

## 2. Probe

```bash
cp $FORK/templates/probe.gitlab-ci.yml company/ci/multica.gitlab-ci.yml
cp $FORK/templates/ci-tools.Dockerfile company/ci/
git add company && git commit -m "ci: environment probe" && git push -u origin probe/env
```

Set the mirror variables you know of first (Settings → CI/CD → Variables): `GOPROXY`, `npm_config_registry`, `COREPACK_NPM_REGISTRY`, and if they exist `APT_MIRROR`, `APK_MIRROR`, `BUILD_PROXY`, `NO_PROXY_HOSTS`, `PUSH_REGISTRY`/`PUSH_USER`/`PUSH_PASSWORD`. Then read the `report` job's `probe-report.md`, and the log of every job it lists as "did not run".

Change `images.yml` or the variables and push again until the report has no open item. Each run takes a few minutes.

## 3. Read the report

| Report item | Means | Do |
|---|---|---|
| `image:*` did not run, log says the image cannot be pulled | path or tag wrong, or not in the registry | fix `images.yml`; if missing, ask for it or substitute by the rule |
| `image:*` user is not 0 for `go-build`, `node-alpine` | the upstream Dockerfile's `RUN apk add` may fail as non-root | pick another tag of that image |
| `runner` cpu-limit, memory-limit | what one job gets (`max` = unlimited) | set `GO_TEST_P` to the CPU count and `VITEST_WORKERS` to about the same; below 4 CPUs expect a longer flaky list |
| `runner:bigger` shows larger limits than `runner` | the project may raise its pod size | put `KUBERNETES_CPU_LIMIT`/`KUBERNETES_MEMORY_LIMIT` on `go-test` and `frontend` |
| `runner` user | the uid jobs start as | if the runner forces a uid other than the image's, set `CI_UID` when building the CI image, or ask for the image's user to be allowed |
| `runner` capabilities not privileged | no Docker daemon in jobs | fine: nothing in the template needs one; the `scripts` tests use the docker CLI without a daemon |
| `service` postgres-at-postgres no, at-localhost yes | services share the pod's network, alias not resolved | set `DATABASE_URL` host to `localhost` in `go-test` |
| `service` both no | services not allowed | stop: go-test needs PostgreSQL; ask for services, or for a database the CI may use |
| `direct ...` codes | what is reachable without the mirrors | anything not reachable must come through a mirror below |
| `GOPROXY proxies sum.golang.org` not 200 | `go run pkg@version` (in `make sqlc`) cannot verify | ask for sumdb proxying; otherwise set `GONOSUMDB`/`GOSUMDB` as the company's policy allows |
| `npm metadata has publish times` no | `min-release-age` in `.npmrc` cannot be honoured; installs fail | ask the mirror's owners; do not disable the setting without the company's agreement |
| `npm-lockfile` FAILED, with failed tarballs or E403 | the company's security policy blocks those package versions (direct or transitive) | list them in `environment.md` as **npm constraints**; see PIPELINE.md, Blocked npm versions |
| `APT_MIRROR <distro>/dists/<release>` | which Debian/Ubuntu releases the apt mirror serves | pick `IMG_CI_BASE` from them (git >= 2.40: Debian trixie, Ubuntu 24.04 or newer); the CI image build fails with a clear message on an older one |
| `APK_MIRROR v3.x` | which Alpine releases the apk mirror serves | `IMG_ALPINE_RUNTIME` and the Alpine variants of `IMG_GO_BUILD`/`IMG_NODE_ALPINE` must be one of them, or use `BUILD_PROXY` |
| `BUILD_PROXY to dl-cdn` | an outbound proxy for image builds | the alternative to the mirrors above |
| `direct https://fonts.googleapis.com` unreachable | `next/font/google` in `apps/web/app/layout.tsx` downloads fonts at build time | the web image build fails; the fix is a company customization (load the same fonts from `@fontsource-variable/*` npm packages, as upstream's desktop app does), entered in `customizations.yaml` so every port carries it |
| `package registry upload` not 201 | the CLI release cannot publish | ask for Package Registry and job-token access; afterwards delete the package `probe` |
| `ci-tools build and push` failed | the log says which: a tool image, apt, Go, npm, or the push | fix that item; when it passes, the CI image exists as `<IMG_CI_TOOLS>:probe` |

Done when: every report item is either fine or has a decision written in `environment.md`.

## 4. Keep the result

- `company/ci/images.yml` goes on the baseline branch with the rest of `company/ci/` (PIPELINE.md).
- The variables go into Settings → CI/CD → Variables (GUARDRAILS.md, GitLab settings).
- `company/ci/environment.md`: the report's key facts (registry paths, runner limits, uid, services, mirrors), every substitute and every open request. The next upgrade starts from it and reruns only steps 1 to 3.
- Delete the branch `probe/env` and the package `probe`.

On later upgrades, rerun step 1 on the new snapshot: `required-images.sh` shows which upstream images changed, and the images job fails on any `FROM` that `images.yml` does not map.
