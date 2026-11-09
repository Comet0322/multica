# Self-Hosting Guide

Deploy Multica on your own infrastructure in minutes.

## Architecture

| Component | Description | Technology |
|-----------|-------------|------------|
| **Backend** | REST API + WebSocket server | Go (single binary) |
| **Frontend** | Web application | Next.js 16 |
| **Database** | Primary data store | PostgreSQL 17 (`pgcrypto` + `pg_trgm`) |

Each user who runs AI agents locally also installs the **`multica` CLI** and runs the **agent daemon** on their own machine.

## Anonymous Deployment Telemetry

The self-hosted API server sends one first-party, deployment-level anonymous snapshot per UTC day to the fixed Multica endpoint `https://telemetry.multica.ai/v1/telemetry/events`. This helps maintainers understand which release versions are in use, deployment-size ranges, aggregate run volume, and how many members actively use a deployment. The endpoint is compiled into the server and cannot be redirected through configuration.

The snapshot contains only the release version; bucketed counts of workspaces, distinct human members, active agents, and daemons seen in the previous 24 hours; aggregate counts of runs started, completed, failed, and cancelled in that window; and bucketed counts of members who opened the signed-in web or desktop app, and of members who personally started an agent run (assign, mention, comment, chat, manual run, or rerun — not autopilot or agent-to-agent delegation), in the previous 1, 7, and 28 complete UTC days. Members are counted per deployment and are never identified. It does not include names, email addresses or domains, IP addresses, business IDs, host/device details, repository or organization details, model/plugin data, prompts or output, comments or chats, paths, token/cost data, credentials, errors, stacks, or logs. This telemetry is not used for billing, licensing, authentication, or security decisions.

Telemetry is enabled by default. To stop both collection and network delivery, set `DO_NOT_TRACK=1` or `DO_NOT_TRACK=true` on the API server and recreate/restart the backend. The deployment identity remains in PostgreSQL, so enabling it again does not create a new identity. `ANALYTICS_DISABLED` controls the separate PostHog integration and does not disable this first-party telemetry.

When cloning a production database for staging or testing, clear the clone's `instance_telemetry_state` table before starting its API server (or set `DO_NOT_TRACK=true`); otherwise both deployments share one identity and their same-day snapshots can silently deduplicate each other.

## Quick Install (Recommended)

Two commands to set up everything — server, CLI, and configuration.

<details open>
<summary><b>macOS / Linux</b></summary>

<br/>

```bash
# 1. Install CLI + provision the self-host server
curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --with-server

# 2. Configure CLI, authenticate, and start the daemon
multica setup self-host
```
</details>
<details>
<summary><b>Windows (PowerShell)</b></summary>

<br/>

```powershell
# 1. Install CLI + provision the self-host server
$env:MULTICA_MODE="with-server"; irm https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.ps1 | iex

# 2. Configure CLI, authenticate, and start the daemon
multica setup self-host
```
</details>

This installs the `multica` CLI, checks out the latest self-host assets, pulls the official Multica images from GHCR, and configures everything for localhost.

Open http://localhost:3000. To log in, configure `RESEND_API_KEY` in `.env` for email-based codes (recommended), or leave Resend unset and copy the generated code from the backend logs. See [Step 2 — Log In](#step-2--log-in) for details.

> **Prerequisites:** Docker and Docker Compose must be installed. The script checks for this and provides install links if missing.
>
> **CLI only?** If the self-host server is already running and you only need the CLI on a macOS/Linux machine, install it with Homebrew:
>
> ```bash
> brew install multica-ai/tap/multica
> ```

---

## Step-by-Step Setup (Alternative)

If you prefer to run each step manually:

### Step 1 — Start the Server

**Prerequisites:** Docker and Docker Compose.

```bash
git clone https://github.com/multica-ai/multica.git
cd multica
make selfhost
```

`make selfhost` automatically creates `.env` from the example, generates a random `JWT_SECRET`, and starts all services via Docker Compose.

By default it pulls the latest stable release images from GHCR. To build the backend/web from your current checkout instead, run `make selfhost-build`.
If the selected GHCR tag has not been published yet, `make selfhost` now tells you to fall back to `make selfhost-build`.
`make selfhost-build` uses local `multica-backend:dev` / `multica-web:dev` tags, so it does not overwrite the pulled `:latest` images.

Once ready:

- **Frontend:** http://localhost:3000
- **Backend API:** http://localhost:8080

> **Note:** If you prefer to run the Docker Compose steps manually, see [Manual Docker Compose Setup](#manual-docker-compose-setup) below.

### Step 2 — Log In

Open http://localhost:3000 in your browser. The Docker self-host stack defaults to `APP_ENV=production` (set in `docker-compose.selfhost.yml`), and there is no fixed verification code by default. Pick one of the following to log in:

- **Recommended (production):** configure `RESEND_API_KEY` in `.env`, then restart the backend. Real verification codes will be sent to the email address you enter. See [Advanced Configuration → Email](SELF_HOSTING_ADVANCED.md#email-required-for-authentication).
- **Without email configured:** the verification code is generated server-side and printed to the backend container logs (look for `[DEV] Verification code for ...:`). Useful for one-off testing on a single machine.
- **Deterministic local/private testing:** set `APP_ENV=development` and `MULTICA_DEV_VERIFICATION_CODE=888888` in `.env`, then restart the backend. This fixed code is ignored when `APP_ENV=production`.

Changes to `ALLOW_SIGNUP`, `DISABLE_WORKSPACE_CREATION`, `WORKSPACE_CREATION_ALLOWED_EMAILS`, and `GOOGLE_CLIENT_ID` also take effect after restarting the backend / compose stack. The web UI reads `ALLOW_SIGNUP`, `DISABLE_WORKSPACE_CREATION`, and `GOOGLE_CLIENT_ID` from `/api/config` at runtime, so no web rebuild is needed. `WORKSPACE_CREATION_ALLOWED_EMAILS` is read only by the backend and is not exposed through `/api/config`. See [Advanced Configuration → Signup Controls](SELF_HOSTING_ADVANCED.md#signup-controls-optional) for the recommended sequence to lock down workspace creation, or to limit it to selected users.

> **Warning:** do **not** set `MULTICA_DEV_VERIFICATION_CODE` on a publicly reachable instance — anyone who knows an email address can then log in with that fixed code.

### Step 3 — Install CLI & Start Daemon

The daemon runs on your local machine (not inside Docker). It detects installed AI agent CLIs, registers them with the server, and executes tasks when agents are assigned work.

Each team member who wants to run AI agents locally needs to:

### a) Install the CLI and an AI agent

```bash
brew install multica-ai/tap/multica
```

You also need at least one AI agent CLI installed:
- [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` on PATH)
- [Antigravity CLI](https://antigravity.google/docs/cli-install) (`agy` on PATH)
- [CodeBuddy Code](https://www.codebuddy.ai/docs/cli/quickstart) (`codebuddy` on PATH)
- [DevEco Code](https://gitcode.com/openharmony-sig/deveco-code) (`deveco` on PATH)
- [Codex](https://github.com/openai/codex) (`codex` on PATH)
- [GitHub Copilot CLI](https://docs.github.com/en/copilot) (`copilot` on PATH)
- [OpenClaw](https://github.com/openclaw/openclaw) (`openclaw` on PATH)
- [OpenCode](https://github.com/anomalyco/opencode) (`opencode` on PATH)
- [Huawei Cloud CodeArts](https://support.huaweicloud.com/qs-codeartssnap/codeartsagent_qs_0004.html) (`codearts` on PATH)
- [Hermes](https://github.com/NousResearch/hermes) (`hermes` on PATH)
- [Pi](https://pi.dev/) (`pi` on PATH)
- [Cursor Agent](https://cursor.com/) (`cursor-agent` on PATH)
- Kimi (`kimi` on PATH)
- [Reasonix](https://github.com/esengine/DeepSeek-Reasonix) (`reasonix` on PATH; run `reasonix setup` first)
- Dim (`dim` on PATH)
- Kiro CLI (`kiro-cli` on PATH)
- Qoder CLI (`qodercli` on PATH)
- Qoder CN CLI (`qoderclicn` on PATH)
- Trae CLI (`traecli` on PATH)
- [Grok Build CLI](https://docs.x.ai/) (`grok` on PATH)
- Qwen Code (`qwen` on PATH)
- [QwenPaw](https://github.com/agentscope-ai/QwenPaw) (`qwenpaw` on PATH; pick its model in QwenPaw's own configuration)
- [MiniMax Code CLI](https://www.npmjs.com/package/@minimax-ai/code) (`mcode` 0.1.2+ on PATH). Install a supported Node.js release (`>=22.19 <23` or `>=24 <27`), run `npm install --global @minimax-ai/code@latest`, then `mcode login`.
- [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (`dsh` on PATH with the Multica runtime profile installed; set `DEEPSEEK_API_KEY`)

### b) One-command setup

```bash
multica setup self-host
```

This automatically:
1. Configures the CLI to connect to `localhost` (ports 8080/3000)
2. Opens your browser for authentication
3. Discovers your workspaces
4. Starts the daemon in the background

For on-premise deployments with custom domains:

```bash
multica setup self-host --server-url https://api.example.com --app-url https://app.example.com
```

To verify the daemon is running:

```bash
multica daemon status
```

> **Alternative:** If you prefer manual steps, see [Manual CLI Configuration](#manual-cli-configuration) below.

### Step 4 — Verify & Start Using

1. Open your workspace in the web app at http://localhost:3000
2. Navigate to **Settings → Runtimes** — you should see your machine listed
3. Go to **Settings → Agents** and create a new agent
4. Create an issue and assign it to your agent — it will pick up the task automatically

---

## Usage Dashboard Rollup

The Usage / Runtime dashboards read from a derived `task_usage_hourly` table populated by `rollup_task_usage_hourly()`. As of MUL-2957 the backend runs this rollup **in-process** on every replica via a DB-backed scheduler (`sys_cron_executions`); a fresh self-host install needs no operator action and the bundled `pgvector/pgvector:pg17` image works without changes — you do **not** need to swap it for an image that ships `pg_cron`, register an external cron job, set up a systemd timer, or run a Kubernetes `CronJob`.

Multiple backend replicas are safe: each replica ticks every 30 seconds and tries to claim the current 5-minute UTC plan, but the unique key `(job_name, scope_kind, scope_id, plan_time)` means only one wins each plan. Inspect steady-state operation:

> **WeCom (企业微信) smart bot and replica count.** Unlike Slack and Lark, whose outbound is stateless HTTP that any replica can perform, the WeCom smart bot's only outbound path is an in-process WebSocket long connection, held by the replica that owns that bot's lease. What happens to a reply produced on a *different* replica depends on the realtime relay:
>
> - **Sharded or dual relay mode (`REDIS_URL` set, the default with Redis):** the reply or inbox push is forwarded to the lease holder over the relay and delivered. Multi-replica WeCom is supported in this mode.
> - **Legacy relay mode, or no Redis:** the reply is dropped and the WeCom user sees nothing. Run the WeCom-enabled backend as a single replica in this configuration.
>
> In **every** mode there is one residual window: a reply produced while *no* replica holds a live connection to that bot — all of them mid-reconnect — is not delivered. It is **counted**: the replica that routed it checks afterwards whether any replica ever claimed the delivery, and increments `multica_wecom_outbound_dropped_total{reason="no_live_connection"}` when none did, so the window can be measured on a deployment rather than guessed at. If a rare lost reply during reconnects is unacceptable, a single replica remains the most conservative deployment. Everything else (including the rollup scheduler above) is multi-replica safe.

```sql
SELECT plan_time, status, attempt, runner_id,
       error_code, error_msg, started_at, finished_at
  FROM sys_cron_executions
 WHERE job_name = 'rollup_task_usage_hourly'
 ORDER BY plan_time DESC
 LIMIT 20;
```

Full reference (audit table semantics, advisory lock 4246, the standalone backfill command, flag descriptions, the `v0.3.4 → v0.3.5+` migration auto-hook) lives in [Advanced Configuration → Usage Dashboard Rollup](SELF_HOSTING_ADVANCED.md#usage-dashboard-rollup).

> **Upgrading from `v0.3.4` to `v0.3.5+`?** As of MUL-2957 the `migrate up` command runs an idempotent monthly-slice backfill automatically right before applying migration `103`, so the upgrade completes in a single invocation — no operator step required. If you are still on a pre-MUL-2957 binary or the auto-hook fails for an environmental reason, run `backfill_task_usage_hourly` against the same database and re-run the upgrade. See [Advanced Configuration → Usage Dashboard Rollup](SELF_HOSTING_ADVANCED.md#usage-dashboard-rollup) for the recovery flow.

### Compatibility paths (existing deployments only)

External schedulers — **`pg_cron` registered on the database, an external cron job, a systemd timer, or a Kubernetes `CronJob`** — that call `SELECT rollup_task_usage_hourly()` directly were the only option before MUL-2957 and remain a supported compatibility path. They are no longer the recommended setup; new deployments should rely on the in-process scheduler instead. The SQL function holds advisory lock 4246 internally, so the in-process scheduler and any pre-existing external schedule can coexist without ever double-writing the rollup.

If you already have a `pg_cron` job in production, the safe sequence to retire it is:

1. Confirm the in-process scheduler is healthy on at least one backend replica — recent SUCCESS rows should be landing in `sys_cron_executions` for `rollup_task_usage_hourly`:

   ```sql
   SELECT plan_time, status, runner_id, finished_at
     FROM sys_cron_executions
    WHERE job_name = 'rollup_task_usage_hourly'
      AND status = 'SUCCESS'
    ORDER BY plan_time DESC
    LIMIT 5;
   ```

2. Once SUCCESS rows are arriving on schedule, unschedule the redundant `pg_cron` entry:

   ```sql
   SELECT cron.unschedule('rollup_task_usage_hourly')
     FROM cron.job WHERE jobname = 'rollup_task_usage_hourly';
   ```

3. Leave the `pg_cron` extension itself installed unless you are sure no other workload depends on it. The bundled `pgvector/pgvector:pg17` image does **not** ship `pg_cron`, so nothing in Multica's default install needs it; uninstalling `pg_cron` from a custom image that other workloads still use is a separate decision.

External cron / systemd timer / Kubernetes `CronJob` setups that call `SELECT rollup_task_usage_hourly()` directly can be retired the same way — once `sys_cron_executions` shows steady SUCCESS rows from the in-process scheduler, the external job is redundant and can be removed.

## Stopping Services

If you installed via the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh | bash -s -- --stop
```

If you cloned the repo manually:

```bash
# Stop the Docker Compose services (backend, frontend, database)
make selfhost-stop

# Stop the local daemon
multica daemon stop
```

## Switching to Multica Cloud

If you've been self-hosting and want to switch your CLI to [Multica Cloud](https://multica.ai):

```bash
multica setup
```

This reconfigures the CLI for multica.ai, re-authenticates, and restarts the daemon. You will be prompted before overwriting the existing configuration.

> Your local Docker services are unaffected. Stop them separately if you no longer need them.

## Upgrading

```bash
docker compose -f docker-compose.selfhost.yml pull
docker compose -f docker-compose.selfhost.yml up -d
```

Pin `MULTICA_IMAGE_TAG` in `.env` to an exact version like `v0.2.4` if you want to stay on a specific release. Migrations run automatically on backend startup.
If the selected GHCR tag has not been published yet, fall back to `make selfhost-build` or `docker compose -f docker-compose.selfhost.yml -f docker-compose.selfhost.build.yml up -d --build`.

> **Upgrading from `v0.3.4` to `v0.3.5+` fails with `refusing to drop legacy daily rollups: ...`?** That's migration `103`'s fail-closed guard: it requires `task_usage_hourly` to be seeded before the legacy daily rollups are dropped. As of MUL-2957 `migrate up` runs that backfill automatically right before applying `103`, so the upgrade completes in a single invocation. If you are still on a pre-MUL-2957 binary or the auto-hook fails, run `backfill_task_usage_hourly` manually first, then re-run the upgrade. Full instructions in [Advanced Configuration → Usage Dashboard Rollup](SELF_HOSTING_ADVANCED.md#usage-dashboard-rollup).

---

## Manual Docker Compose Setup

If you prefer running Docker Compose steps manually instead of `make selfhost`:

```bash
git clone https://github.com/multica-ai/multica.git
cd multica
cp .env.example .env
```

Edit `.env` — set `JWT_SECRET` (required): docker compose refuses to start without
it, and a production backend refuses to boot on the dev default or any known
placeholder.

```bash
JWT_SECRET=$(openssl rand -hex 32)
```

Then start everything:

```bash
docker compose -f docker-compose.selfhost.yml pull
docker compose -f docker-compose.selfhost.yml up -d
```

## Manual CLI Configuration

If you prefer configuring the CLI step by step instead of `multica setup`:

```bash
# Point CLI to your local server
multica config set server_url http://localhost:8080
multica config set app_url http://localhost:3000

# Login (opens browser)
multica login

# Start the daemon
multica daemon start
```

For production deployments with TLS:

```bash
multica config set app_url https://app.example.com
multica config set server_url https://api.example.com
multica login
multica daemon start
```

## Advanced Configuration

For environment variables, manual setup (without Docker), reverse proxy configuration, database setup, and more, see the [Advanced Configuration Guide](SELF_HOSTING_ADVANCED.md).
