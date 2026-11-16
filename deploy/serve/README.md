# `ocr serve`: PR review webhook service

`ocr serve` receives GitHub App and GitLab webhooks and runs a bounded chain
of review rounds on each pull request or merge request. In each round a
review CLI finds problems, a fix run keeps only the real ones, fixes them,
runs the tests and the service pushes the fix to the PR branch; the next
round reviews that fix commit. Each round posts one PR comment. The review
runs an agent CLI (Codex CLI, Claude Code or Antigravity) by default, or the
in-process OCR review; see [Review engine](#review-engine). A sticky summary
comment holds the chain state, and the commit status `ocr-review` blocks the
merge while the chain runs; see [Review rounds](#review-rounds) and
[Merge gate](#merge-gate).

`ocr serve hooks gitlab` registers the webhooks on a GitLab instance.

## Files in this directory

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage image: static binary on Debian trixie-slim with `git`, `ca-certificates`, `curl` (health check) and `tini` (PID 1), user `ocr` (uid/gid 1000), listens on `0.0.0.0:3012`. |
| `Dockerfile.dockerignore` | Keeps the build context to `go.mod`, `go.sum`, `cmd/` and `internal/`. |
| `compose.example.yml` | Example service. Copy it to `compose.yml`; keep `.env`, `config.json` and `github-app.pem` next to it and out of git. |
| `../komodo/compose.yml` | Komodo stack: every setting from the stack environment; the host agent CLIs and the review skill are mounted read-only. See [Komodo](#komodo). |
| `../pm2/ecosystem.config.cjs` | PM2 app `ocr-review` for a service that runs directly on a host. See [Run on a host with PM2](#run-on-a-host-with-pm2). |
| `../pm2/deploy.sh` | Fetches a branch, builds the binary, reloads the PM2 app and checks `/healthz`; `--rollback` restores the previous binary. |

## Quick start

```bash
cd deploy/serve
cp compose.example.yml compose.yml
printf '{"language": "Vietnamese"}\n' > config.json
# Write .env (see the table below), then copy the GitHub App key:
install -m 600 /path/to/app.private-key.pem github-app.pem
sudo chown 1000:1000 github-app.pem   # the container runs as uid 1000
OCR_GIT_COMMIT=$(git rev-parse --short HEAD) docker compose build
docker compose up -d
curl -s http://127.0.0.1:3012/healthz
```

Without GitHub, remove the `github-app.pem` volume and the
`OCR_SERVE_GITHUB_PRIVATE_KEY_FILE` line from `compose.yml`. The example runs
the OCR engine (`OCR_SERVE_ENGINE=ocr`), which has no fix run, so every round
only reports; for the CLI engine, add the mounts and settings of
[Review engine](#review-engine).

## Komodo

`deploy/komodo/compose.yml` runs the same image as a Komodo stack. Komodo
writes the stack environment to `.env` next to the compose file: the GitHub
App key is an environment value, and the OCR `config.json` is generated
inline from `OCR_LANGUAGE`. The only host files are the agent CLIs, their
credential files and the review skill, mounted as listed in
[Review engine](#review-engine). The inline config needs
Docker Compose 2.23.1 or later on the server.

Stack settings:

| Setting | Value |
|---|---|
| Repo | `kevinle128/open-code-review` on `github.com` |
| Branch | the branch to deploy, for example `feat/ocr-serve` |
| File paths | `deploy/komodo/compose.yml` |
| Run build | on (`docker compose build` before `up -d`) |

Environment (`[[NAME]]` is a secret Komodo Variable):

```bash
OCR_LLM_URL=https://router.example/v1
OCR_LLM_TOKEN=[[OCR_LLM_TOKEN]]
OCR_LLM_MODEL=<model>
OCR_LLM_PROTOCOL=openai
OCR_LANGUAGE=Vietnamese
OCR_SERVE_GITHUB_APP_ID=<app id>
OCR_SERVE_GITHUB_PRIVATE_KEY=[[OCR_SERVE_GITHUB_PRIVATE_KEY]]
OCR_SERVE_GITHUB_WEBHOOK_SECRET=[[OCR_SERVE_GITHUB_WEBHOOK_SECRET]]
OCR_SERVE_GITLAB_URL=https://git.example.com
OCR_SERVE_GITLAB_TOKEN=[[OCR_SERVE_GITLAB_TOKEN]]
OCR_SERVE_GITLAB_WEBHOOK_SECRET=[[OCR_SERVE_GITLAB_WEBHOOK_SECRET]]
# Optional; empty uses the default.
OCR_SERVE_IGNORE_AUTHORS=
OCR_SERVE_MAX_ROUNDS=
OCR_SERVE_REVIEW_TIMEOUT=
OCR_SERVE_FIX_TIMEOUT=
OCR_SERVE_WORKERS=
OCR_SERVE_REVIEW_CONCURRENCY=
OCR_SERVE_BUNDLE_LINES=
OCR_SERVE_EXCLUDE=
OCR_LLM_TIMEOUT=900
# Review engine; empty uses the default (cli with codex, fix run with claude).
OCR_SERVE_ENGINE=
OCR_SERVE_REVIEW_CLI=
OCR_SERVE_VERIFY_CLI=
OCR_SERVE_VERIFY_MODEL=
OCR_SERVE_CODEX_EFFORT=medium
```

- `OCR_LLM_URL`, `OCR_LLM_TOKEN` and `OCR_LLM_MODEL` are required by the
  compose file. The platform values are optional there, so a GitHub-only or
  GitLab-only stack works: leave out the other platform's lines. The service
  still checks that one platform is complete.
- A `.env` line cannot hold a real newline, so give the PEM key on one line
  with literal `\n` escapes (`-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END
  RSA PRIVATE KEY-----`). The service turns `\n` back into newlines when the
  value has no real newline. Compose also does this itself for a
  double-quoted value; both forms work.
- The volumes have the fixed names `ocr-review_ocr-data` and
  `ocr-review_ocr-work`, so a stack that replaces a hand-run `ocr-review`
  project keeps its state whatever project name Komodo uses.
- The container runs as uid/gid 1000. Images before the Debian base ran as
  uid 10001, and the volumes they created still belong to that user, so the
  service cannot write `/data` or `/work` after the upgrade. Give them to uid
  1000 once, with the stack stopped:
  `docker run --rm -v ocr-review_ocr-data:/data -v ocr-review_ocr-work:/work debian:trixie-slim chown -R 1000:1000 /data /work`.
- The container is `ocr-review` on `127.0.0.1:3012`. To register GitLab
  hooks, pass the admin token to that run only (see
  [GitLab hooks](#gitlab-hooks)):
  `docker exec -e OCR_SERVE_GITLAB_ADMIN_TOKEN -e OCR_SERVE_GITLAB_HOOK_URL -e OCR_SERVE_GITLAB_BOT_USERNAME ocr-review opencodereview serve hooks gitlab --dry-run`.

## Run on a host with PM2

The service can run directly on a Linux host under PM2 instead of in a
container. Use this with the [docker sandbox](#sandbox): the service starts
one container per CLI run with the host docker CLI, which a service
container could only do with the docker socket mounted. The Komodo stack
and the image stay as they are (`OCR_SERVE_SANDBOX=none`) for a rollback.

Install for the service user, no root needed: Go (to build the binary),
Node and PM2 (`npm install -g pm2`), git and curl. The user needs the
docker CLI and membership in the `docker` group. The language toolchains
live in the sandbox images, not on the host.

Layout under `~/ocr-review/`:

| Path | Purpose |
|---|---|
| `src/` | Checkout of this repository; `deploy.sh` clones it on the first run. |
| `bin/ocr`, `bin/ocr.prev` | The running binary and the previous one. |
| `data/` | `OCR_SERVE_DATA_DIR`: `cli-home/` (run dirs), `cache/` (toolchain caches). |
| `work/` | `OCR_SERVE_WORK_DIR`: checkout slots. |
| `skills/` | The review skill tree (`OCR_SERVE_SKILL_DIR`). |
| `config.json` | OCR config with `language` (`OCR_CONFIG_FILE`). |
| `ocr-review.env` | Service settings, mode 600. |

`ocr-review.env` holds the same variables as the Komodo stack environment,
one `KEY=VALUE` per line (`#` comments; one pair of quotes around a value is
removed; the GitHub App key on one line with literal `\n`). Quote a value
that holds spaces, so the file also loads in a shell with `set -a; . file`. The secrets are
the values themselves, not `[[NAME]]` Komodo references. Differences from
the stack: there is no `OCR_LANGUAGE` (the language goes into `config.json`),
and the paths are host paths:

```bash
OCR_SERVE_ADDR=127.0.0.1:3012
OCR_CONFIG_FILE=/home/<user>/ocr-review/config.json
OCR_SERVE_DATA_DIR=/home/<user>/ocr-review/data
OCR_SERVE_WORK_DIR=/home/<user>/ocr-review/work
OCR_SERVE_SKILL_DIR=/home/<user>/ocr-review/skills
OCR_SERVE_CODEX_BIN=/home/<user>/.local/bin/codex
OCR_SERVE_CLAUDE_BIN=/home/<user>/.local/bin/claude
OCR_SERVE_CODEX_AUTH=/home/<user>/.codex/auth.json
OCR_SERVE_CLAUDE_SETTINGS=/home/<user>/.claude/settings.json
OCR_SERVE_SANDBOX=docker
OCR_SERVE_WORKERS=1
OCR_SERVE_GITHUB_APP_ID=<app id>
OCR_SERVE_GITHUB_PRIVATE_KEY="-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----"
OCR_SERVE_GITHUB_WEBHOOK_SECRET=<secret>
OCR_SERVE_GITLAB_URL=https://git.example.com
OCR_SERVE_GITLAB_TOKEN=<bot token>
OCR_SERVE_GITLAB_WEBHOOK_SECRET=<secret>
# For OCR_SERVE_REVIEW_CLI=agy (review only):
# OCR_SERVE_AGY_BIN=/home/<user>/.local/bin/agy
# OCR_SERVE_AGY_TOKEN=/home/<user>/.gemini/antigravity-cli/antigravity-oauth-token
# Optional, as on the stack: OCR_SERVE_REVIEW_CLI, OCR_SERVE_VERIFY_CLI,
# OCR_SERVE_VERIFY_MODEL, OCR_SERVE_CODEX_EFFORT, OCR_SERVE_MAX_ROUNDS,
# OCR_SERVE_EXCLUDE, ...
# OCR_LLM_* only for OCR_SERVE_ENGINE=ocr.
```

The CLI binaries are resolved through their links at startup, so after a
Claude or Codex update restart the service (`pm2 restart ocr-review`) to
use the new version.

First deploy and every later one:

```bash
mkdir -p ~/ocr-review && cd ~/ocr-review
printf '{"language": "Vietnamese"}\n' > config.json
install -m 600 /dev/null ocr-review.env   # then fill it in
git clone --branch <branch> https://github.com/<owner>/open-code-review.git src
OCR_REVIEW_BRANCH=<branch> src/deploy/pm2/deploy.sh
```

`deploy.sh` fetches `OCR_REVIEW_BRANCH` (default `main`) into `src/`, builds
`bin/ocr.new` with `go build -trimpath` and the version labels, keeps the
running binary as `bin/ocr.prev` when it is healthy, moves the new binary
into place, runs `pm2 startOrReload deploy/pm2/ecosystem.config.cjs
--update-env` and `pm2 save`, and waits until `/healthz` reports the new
version (up to 2 minutes, which covers the drain of running reviews). It
exits non-zero when a step fails. `deploy.sh --rollback` puts `bin/ocr.prev`
back and checks the health again. Settings: `OCR_REVIEW_ROOT` (default
`~/ocr-review`), `OCR_REVIEW_SRC`, `OCR_REVIEW_REPO`, `OCR_REVIEW_BRANCH`,
`OCR_REVIEW_HEALTH_URL`, `OCR_REVIEW_HEALTH_TRIES`.

The PM2 app runs `bin/ocr serve` with the settings of `ocr-review.env`, read
by the ecosystem file each time PM2 loads it: after an edit run
`pm2 startOrReload ~/ocr-review/src/deploy/pm2/ecosystem.config.cjs
--update-env` (or `deploy.sh`). PM2 stops it with SIGTERM and waits 75
seconds (`kill_timeout`) so running reviews can drain; only the service gets
the signal, not the docker clients of running reviews. It restarts after a
crash with a 5 second delay and when it uses more than 1 GB.

- Logs: `pm2 logs ocr-review` (JSON lines, with timestamps).
- Status: `pm2 status`, `curl -s http://127.0.0.1:3012/healthz`.
- `pm2 save` writes the process list with its environment, secrets
  included, to `~/.pm2/dump.pm2`; keep `~/.pm2` private:
  `chmod 700 ~/.pm2`.
- Start after a reboot: `pm2 startup` prints a systemd unit command that
  needs sudo. Without sudo, add a crontab line
  (`crontab -e`) that restores the saved list:

  ```text
  @reboot PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin pm2 resurrect >> $HOME/ocr-review/resurrect.log 2>&1
  ```

  Adjust `PATH` to where `node` and `pm2` are installed.
- GitLab hooks: run the command with the binary and `ocr-review.env`
  loaded in the shell, adding the admin token for that run only:
  `set -a; . ~/ocr-review/ocr-review.env; set +a; OCR_SERVE_GITLAB_ADMIN_TOKEN=... ~/ocr-review/bin/ocr serve hooks gitlab --dry-run`.

## Routes

| Method and path | Purpose |
|---|---|
| `POST /github/webhook` | GitHub App deliveries. Bad signature: 403. |
| `POST /gitlab/webhook` | GitLab system and project hooks. Bad token: 401. |
| `GET /healthz` | 200 `{"status":"ok","version":"..."}`. |

Every route is also served under the `/ocr` prefix (`/ocr/github/webhook`,
`/ocr/gitlab/webhook`, `/ocr/healthz`), so the URLs work whether or not a
reverse proxy strips that prefix. A disabled platform answers 404, a body over
`OCR_SERVE_MAX_BODY_BYTES` answers 413, and an accepted delivery answers 202
before the review runs.

## Configuration

### Service environment (`.env`)

Secrets also accept `<NAME>_FILE` with a path to a file that holds the value;
setting both forms is an error.

| Variable | Default | Notes |
|---|---|---|
| `OCR_SERVE_ADDR` | `127.0.0.1:3012` | The image sets `0.0.0.0:3012`. The health check uses port 3012. |
| `OCR_SERVE_MAX_ROUNDS` | `3` | Fix rounds per chain, 1..10. One report-only round follows them; see [Review rounds](#review-rounds). |
| `OCR_SERVE_WORKERS` | `2` | Concurrent rounds, 1..16; also the number of checkout slots. |
| `OCR_SERVE_REVIEW_TIMEOUT` | `2h` | Limit for the review part of one round (read, checkout, review), timed from when a worker takes it. Each file group in the review has a limit of 120 minutes per round. |
| `OCR_SERVE_FIX_TIMEOUT` | `1h` | Limit for the fix run of one round with its commit and push. It starts when the review ends, so a long review does not take time from the fix. |
| `OCR_SERVE_REVIEW_CONCURRENCY` | `5` | LLM subtasks that one review runs at the same time, 1..32. `ocr review --concurrency` keeps its default of 8. |
| `OCR_SERVE_BUNDLE_LINES` | `300` | A PR with fewer files than the grouping minimum (4) and fewer changed lines than this value is reviewed as one group; otherwise each file is a separate group. 50..5000. Replaces the template value of 200 for serve reviews only. |
| `OCR_SERVE_MAX_BODY_BYTES` | `10485760` | Webhook body limit. |
| `OCR_SERVE_DATA_DIR` | `~/ocr-review/data` | Holds the CLI run dirs and the sandbox caches. The image sets `/data`; without `HOME` the default is `/data`. A relative path is made absolute. |
| `OCR_SERVE_WORK_DIR` | `~/ocr-review/work` | Checkout slots `slot-N`, emptied after each run. The image sets `/work`; without `HOME` the default is `/work`. |
| `OCR_SERVE_IGNORE_REPOS` | empty | Comma-separated RE2 patterns; match anywhere in the value. |
| `OCR_SERVE_IGNORE_AUTHORS` | empty | Same format. |
| `OCR_SERVE_IGNORE_TITLES` | empty | Same format. |
| `OCR_SERVE_IGNORE_SOURCE_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_TARGET_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_LABELS` | empty | Comma-separated, exact match. |
| `OCR_SERVE_EXCLUDE` | empty | Comma-separated globs added to the serve file filter for every repository; see [File filter](#file-filter). |
| `OCR_SERVE_BOT_INDICATORS` | `bot` | Substrings that mark a GitLab sender as a bot. |
| `OCR_SERVE_MENTIONS` | empty | Comma-separated extra names users may tag the bot with to run a command (`@<name> review`), on top of the GitHub App slug and the GitLab bot username. Letters, digits, `-`, `_` and `.` only, without `@`. |
| `OCR_SERVE_GITHUB_APP_ID` | empty | Enables GitHub. |
| `OCR_SERVE_GITHUB_PRIVATE_KEY` / `_FILE` | | Required with GitHub. The example compose sets `_FILE`. A one-line value with literal `\n` escapes is accepted. |
| `OCR_SERVE_GITHUB_WEBHOOK_SECRET` / `_FILE` | | Required with GitHub. |
| `OCR_SERVE_GITHUB_API_URL` | `https://api.github.com` | |
| `OCR_SERVE_GITLAB_URL` | empty | Enables GitLab, for example `https://git.example.com`. Its scheme and host must match the instance's `external_url`: a project clone URL on another scheme or host is refused, so the token is not sent there. |
| `OCR_SERVE_GITLAB_TOKEN` / `_FILE` | | Required with GitLab. Token of the bot user that posts reviews. The service reads `GET /user` at startup and fails when the token is rejected. |
| `OCR_SERVE_GITLAB_WEBHOOK_SECRET` / `_FILE` | | Required with GitLab. |

At least one platform must be enabled; otherwise the service stops with
`no platform enabled: set OCR_SERVE_GITHUB_APP_ID or OCR_SERVE_GITLAB_URL`.

### Review engine

`OCR_SERVE_ENGINE` picks the engine. Rounds, the round comments, the summary
marker and the merge gate are the same for both; only `cli` has a
[fix run](#fix-run), so with `ocr` every round only reports.

- `cli` (default) runs an installed agent CLI on the checkout. The service
  selects the files with the same filters as the OCR engine (project rule,
  per-repository and service excludes, the [file filter](#file-filter)) and
  writes `pr.json` and `diff.patch` (the diff of the selected files) into a
  run directory. The prompt is the review skill, read from
  `$OCR_SERVE_SKILL_DIR/skills/ak-review-pr/SKILL.md` at run time, followed
  by the full text of `review-workflow.md`, `review-checklist.md`,
  `anti-ai-slop.md`, `pr-body-contract.md` and `writing-language.md` from its
  `references/` (a missing reference is left out with a warning in the log),
  plus the service instructions: read those two files, do not edit files or
  use the network, and answer with findings as JSON. The skill's "Service
  mode (ocr serve)" section overrides the `gh` steps of the references.
  Findings on files outside the selection are dropped. A finding about the
  PR as a whole, such as the PR description check (the agent reports it with
  an empty path, or on `pr.json`), is kept as a PR-level finding with an
  empty path; the fix run never edits the PR description, so a real one is
  listed in the round comment as not fixed. When the round reviews a fix
  commit the service pushed, the prompt tells the agent to report only
  problems that commit introduces or leaves unfixed in its changed lines,
  and not to check the PR description or scope again. The skill is not part
  of this repository.

  A large PR is reviewed in batches, one CLI run each, in its own run
  directory. Files of one directory stay in one batch where they fit. Each
  batch holds at most `OCR_SERVE_BATCH_FILES` files and about
  `OCR_SERVE_BATCH_LINES` changed lines (a larger single file is its own
  batch); a PR within both limits is one run. In a batch, `pr.json` keeps
  the whole PR and every selected file in `files`, and adds `batch`
  (`index` from 1, `count`, and the batch `files`); `diff.patch` holds only
  the batch files. Only the first batch checks the PR description. Findings
  are merged, and a finding with the same path, line and title as an earlier
  one is dropped. The round timeout covers all batches. When a batch fails,
  the others still give their findings and the review is partial: the round
  comment lists the files of the failed batch, and `review` starts the chain
  again. Each batch logs one `review batch` line with its files, changed
  lines, findings and duration. Batching applies to the review of every
  round; the fix run is one run.
- `ocr` runs the in-process OCR review with the `OCR_LLM_*` settings. Use it
  to roll back: set `OCR_SERVE_ENGINE=ocr` and redeploy. The CLI settings and
  mounts are then not used.

| Variable | Default | Notes |
|---|---|---|
| `OCR_SERVE_ENGINE` | `cli` | `cli` or `ocr`. |
| `OCR_SERVE_REVIEW_CLI` | `codex` | CLI for reviews: `codex`, `claude` or `agy` (see [Antigravity CLI](#antigravity-cli-agy)). |
| `OCR_SERVE_CODEX_BIN` | `codex` | Name on `PATH` or absolute path. The Komodo stack sets the mounted binary. |
| `OCR_SERVE_CLAUDE_BIN` | `claude` | Same. |
| `OCR_SERVE_CODEX_MODEL` | empty | Codex `-m`; empty uses the account default. |
| `OCR_SERVE_CODEX_EFFORT` | empty | Codex `model_reasoning_effort`, for example `medium`. |
| `OCR_SERVE_CLAUDE_MODEL` | empty | Claude `--model`; empty uses the `model` of the settings file. |
| `OCR_SERVE_SKILL_DIR` | | Required with `cli`. Absolute path of the directory that holds `skills/ak-review-pr/SKILL.md` and its `references/`, and optionally `agents/kongming.md` for the fix run. |
| `OCR_SERVE_CLI_HOME` | `$OCR_SERVE_DATA_DIR/cli-home` | Directory the service owns. Each run gets `run-<id>/` in it, removed when the run ends; leftovers are removed at startup. |
| `OCR_SERVE_CODEX_AUTH` | | Required with `codex`. The host `~/.codex/auth.json`. Each run's `CODEX_HOME` links `auth.json` to it; nothing else of the host `~/.codex` is used. Mount it read-write so a token refresh reaches the host file. |
| `OCR_SERVE_BATCH_LINES` | `1500` | Changed lines (insertions plus deletions) per review batch, `100`..`50000`. |
| `OCR_SERVE_BATCH_FILES` | `25` | Files per review batch, `1`..`500`. |
| `OCR_SERVE_BATCH_CONCURRENCY` | `1` | Review batches of one PR that run at the same time, `1`..`4`. Each is a full CLI run (and a sandbox container), on top of `OCR_SERVE_WORKERS`. |
| `OCR_SERVE_CLAUDE_SETTINGS` | | Required with `claude`. The host `~/.claude/settings.json`. Each run's `CLAUDE_CONFIG_DIR` gets a `settings.json` with only its `env` and `model` keys, never hooks or permissions. |
| `OCR_SERVE_AGY_BIN` | `agy` | Name on `PATH` or absolute path of the Antigravity CLI. |
| `OCR_SERVE_AGY_TOKEN` | | Required with `agy`. The host `~/.gemini/antigravity-cli/antigravity-oauth-token`. Each run home links to it; nothing else of the host `~/.gemini` is used. Mount it read-write so a token refresh reaches the host file. |
| `OCR_SERVE_AGY_MODEL` | empty | agy `--model`, a slug from `agy models`, for example `gemini-4-hydrogen-high`; empty uses the account default. |
| `OCR_SERVE_AGY_EFFORT` | empty | agy `--effort`: `low`, `medium`, `high`, `xhigh` or `max`. |

At startup the service checks that the review and fix run CLI binaries, the
skill file and the CLI credential files can be read, and stops with the
variable name when one cannot. How each CLI runs:

- Codex: `codex exec --json --ephemeral --dangerously-bypass-approvals-and-sandbox`
  in the checkout. Codex's own sandbox (bubblewrap) needs unprivileged user
  namespaces, which Ubuntu 24.04 denies by default, so the container is the
  only boundary: a PR that tricks codex into running a network command could
  send the mounted `auth.json` out. Run it only on repositories whose authors
  you trust, or use `OCR_SERVE_REVIEW_CLI=claude`, which has no shell tool.
  It runs
  with `project_doc_max_bytes=0` so the checkout's `AGENTS.md`, which the PR
  author controls, is not loaded, and the answer schema passed with
  `--output-schema`.
- Claude: `claude -p --output-format json` with only the `Read`, `Grep` and
  `Glob` tools, `--setting-sources user` and `--strict-mcp-config` so the
  checkout's settings and MCP servers are not loaded, and `--add-dir` for the
  skill and run directories. A [fix run](#fix-run) uses
  `--permission-mode acceptEdits` and the tools `Read`, `Edit`, `MultiEdit`,
  `Write`, `Glob`, `Grep` and `Task`, plus `Bash` only with
  `OCR_SERVE_SANDBOX=docker`, where the shell runs in the run's container
  (see [Sandbox](#sandbox)). A report-only fix run keeps the read tools and
  gets `Bash` the same way, only with the docker sandbox, where the checkout
  is mounted read-only. Without the sandbox neither gets a shell, so neither
  can run the tests.
- agy: `agy -p <prompt> --output-format json --json-schema <schema file>
  --dangerously-skip-permissions --disable-slash-commands --settings <json>`
  in the run input directory, not in the checkout, with `--add-dir` for the
  skill and run directories. See [Antigravity CLI](#antigravity-cli-agy).
- The CLI gets `PATH`, locale, time zone, CA and proxy variables, and `HOME`
  and `TMPDIR` inside the run directory; none of the service secrets. It runs
  in its own process group: when the review limit ends it gets SIGTERM, then
  SIGKILL after 10 seconds, and whatever it started is killed when it exits.
  With the docker sandbox the container is killed and removed instead.
- One CLI process runs at a time when `OCR_SERVE_WORKERS=1`; set that for a
  CLI engine that shares an account with other users.

The Komodo stack mounts these host paths (each one is a stack variable):

| Host path (default) | Container path | Mode |
|---|---|---|
| `/home/vtcm/.local/share/claude` (`OCR_HOST_CLAUDE_DIR`) | `/opt/claude` | read-only |
| `/home/vtcm/.codex/packages/standalone` (`OCR_HOST_CODEX_DIR`) | `/opt/codex` | read-only |
| `/home/vtcm/.codex` (`OCR_HOST_CODEX_HOME`) | `/opt/codex-home` | read-write; only `auth.json` is used |
| `/home/vtcm/.claude` (`OCR_HOST_CLAUDE_HOME`) | `/opt/claude-home` | read-only; only the `env` and `model` keys of `settings.json` are used |
| `/home/vtcm/.local/bin/agy` (`OCR_HOST_AGY_BIN`) | `/opt/agy/agy` | read-only; the binary file |
| `/home/vtcm/.gemini/antigravity-cli/antigravity-oauth-token` (`OCR_HOST_AGY_TOKEN`) | `/opt/agy-token/antigravity-oauth-token` | read-write; the token file only |
| `/home/vtcm/ocr-review/skills` (`OCR_HOST_SKILL_DIR`) | `/opt/ocr-skills` | read-only |

The agy rows are file mounts: docker creates an empty directory at a host
path that does not exist, so install agy and sign in on the host before the
first deploy with this stack. `OCR_SERVE_AGY_BIN` defaults to `/opt/agy/agy`
and `OCR_SERVE_AGY_TOKEN` is `/opt/agy-token/antigravity-oauth-token`.

`OCR_SERVE_CLAUDE_BIN` defaults to `/opt/claude/versions/2.1.322` and
`OCR_SERVE_CODEX_BIN` to
`/opt/codex/releases/0.163.0-x86_64-unknown-linux-musl/bin/codex`. When a CLI
on the host updates and removes the old version, set the variable to the new
path and redeploy. To switch the review CLI, set `OCR_SERVE_REVIEW_CLI` and
redeploy.

### Antigravity CLI (agy)

`OCR_SERVE_REVIEW_CLI=agy` reviews with the Google Antigravity CLI. It is a
review CLI only; the fix run keeps `OCR_SERVE_VERIFY_CLI` (`claude` or
`codex`).

Install it and sign in once, as the service user on the host:

```bash
curl -fsSL https://antigravity.google/cli/install.sh | bash   # installs ~/.local/bin/agy
agy        # prints a Google OAuth URL; open it, sign in, paste the code back
agy models # lists the model slugs for OCR_SERVE_AGY_MODEL
```

On a host without a keyring (a server), agy keeps the OAuth token in the
file `~/.gemini/antigravity-cli/antigravity-oauth-token` (mode 600). Point
`OCR_SERVE_AGY_TOKEN` at that file. Each run gets
`$HOME/.gemini/antigravity-cli/antigravity-oauth-token` in its run home as a
link to it, so agy writes its conversations, logs and caches into the run
home, and the host `~/.gemini` holds only the token. When agy replaces the
link with a refreshed token, the service writes the new token back into the
host file after the run (only when it is valid JSON).

A run is:

```text
agy -p <prompt> --output-format json --json-schema <run out>/schema.json
    --dangerously-skip-permissions --disable-slash-commands
    --settings '{"allowNonWorkspaceAccess":true,"permissions":{"deny":["write_file(*)"]}}'
    [--model <OCR_SERVE_AGY_MODEL>] [--effort <OCR_SERVE_AGY_EFFORT>]
    --add-dir <skill dir> --add-dir <run input dir>
```

with stdin empty and the working directory set to the run input directory.
The service takes `structured_output` from the JSON result, or `response`
when it is missing, and fails the run when `status` is not `SUCCESS` or the
result has an `error`. A prompt longer than 100 KiB (Linux allows 128 KiB
for one argument) goes into `agy-prompt.md` in the run input directory, and
`-p` tells agy to read that file first and follow it.

Why agy does not run in the checkout. agy loads customizations from every
workspace folder (its working directory and each `--add-dir`) and from each
parent folder up to the repository root: `AGENTS.md`, `GEMINI.md`, the
`.agents/` folder (also the legacy `.agent/`, `_agents/` and `_agent/`) with
its `rules/`, `skills/`, `agents/`, `plugins/`, `mcp_config.json` and
`hooks.json`, and a repo-root `.gemini/config.json`. The documentation
([rules](https://antigravity.google/docs/rules/),
[customization discovery](https://antigravity.google/docs/customization-discovery/),
[CLI reference](https://antigravity.google/docs/cli/reference/)) names no
flag or setting that turns this off, and in print mode agy asks nobody
whether to trust the folder
([google-antigravity/antigravity-cli#1400](https://github.com/google-antigravity/antigravity-cli/issues/1400)).
A test with agy 1.5.9 confirmed it: with a test repository as the working
directory, or added with `--add-dir`, its `AGENTS.md` and an `always_on`
rule reached the answer, and the `SessionStart` and `PreInvocation`
commands of its `.agents/hooks.json` ran. A hook is a shell command, and the
checkout belongs to the PR author. So the checkout is never a workspace:
agy starts in the run input directory, which the service writes, reads the
checkout by absolute path through the session setting
`allowNonWorkspaceAccess`, and the prompt starts with a note that names the
checkout path. In the same test, run from a neutral directory with that
setting, agy listed and read the repository, no hook ran, the rules did not
load, and the `write_file(*)` deny rule refused a file write. Keep
`OCR_SERVE_CLI_HOME` outside any git repository, or agy also loads the
customizations of the repository that holds the run directory.

Read-only intent. agy has no read-only mode. The prompt forbids edits, the
deny rule blocks agy's file write tool, and with `OCR_SERVE_SANDBOX=docker`
the checkout is mounted read-only. With `OCR_SERVE_SANDBOX=none` agy can
still change the checkout through a shell command. The checkout slot is
emptied when the round ends, but a change stays in place for the later
batches of the same review and for the fix run, and the service commits
every change in the checkout, so such a change can reach the bot's fix
commit. Run agy (and codex) with the docker sandbox where you can.
As with codex, agy has a shell (`--dangerously-skip-permissions`), so a PR
that tricks it into a network command could send the token file out.

### Fix run

With the CLI engine every round that has findings runs one fix run after the
review. It reads the code for each finding, keeps only the real ones, fixes
them with the smallest change, runs the project tests and answers with what
it fixed. A finding it judges minor, false or out of scope is dropped and is
not shown anywhere. Example: review with agy, fix with Claude Opus:

```bash
OCR_SERVE_REVIEW_CLI=agy
OCR_SERVE_AGY_MODEL=gemini-4-carbon-medium
OCR_SERVE_VERIFY_CLI=claude
OCR_SERVE_VERIFY_MODEL=opus
```

The review findings get the ids `F1`, `F2`, ... and go to the run directory
as `findings.json` (`id`, `path`, `line`, `end_line`, `severity`,
`category`, `content`), next to `pr.json` with `mode` `fix` (and `files`, the
files the branch changes) or `report` (report-only). The prompt is the
skill's `SKILL.md` followed by `mode-fix.md`, `review-checklist.md` and
`pr-body-contract.md` from its `references/`, and the service instructions:
the section "Service fix mode (ocr serve)" of `mode-fix.md` applies, the
`git diff` ranges of the reviewed changes and of the whole PR, the findings,
and the answer schema. In a fix round the current directory is a checkout of
the PR head branch; in a report-only round it is a read-only checkout of the
PR head. For Claude in a fix round, `$OCR_SERVE_SKILL_DIR/agents/kongming.md`
is installed into the run's config dir with `model: inherit`, read-only
tools and no project memory; without the file the run goes on without it.

The skill section gives each finding one verdict:

| Verdict | Meaning |
|---|---|
| `real` | A defect this PR introduces or touches that should be fixed before merge, with a concrete failing scenario. |
| `minor` | True but of low impact. |
| `false` | Wrong: already handled, a misreading, or unreachable. |
| `out_of_scope` | True, but this PR did not introduce or change the code at fault. |

A finding about the PR description is judged by `pr-body-contract.md`; one
about the PR scope is real when the PR clearly mixes unrelated changes. The
fix run never edits the PR description: real PR-level findings, and real
findings without a safe fix, go to `report_only` with the failing scenario
and the proposed fix.

The answer is one JSON object:
`{fixed: [{id, path, line, problem, scenario, fix, changes: [{path, start_line, end_line}], tests_added}], report_only: [{id, path, line, problem, scenario, proposed_fix, proposed_diff}], tests: {command, passed, output_tail}, summary, commit_message}`.
`changes` lists the line ranges of the new files that the fix of that
finding edited (test files included; 0 and 0 for a deleted file). The
service does not take the diff from the agent: it runs `git diff -U3` on the
fix commit against its parent and gives each finding the hunks that its
ranges overlap, so one hunk can serve two findings. `proposed_diff` is a
unified diff against the reviewed head that is not applied; the service
keeps only its diff lines, at most 40000 bytes.
The service keeps only ids of the request, each once; an id in both lists
counts as reported, and a `fixed` entry of a report-only run becomes a
reported one with its fix as the proposal. The texts are in the `language`
of `config.json`; `commit_message` is one short English line, cut to its
first line and 100 characters (`fix: address review findings` when empty).

| Variable | Default | Notes |
|---|---|---|
| `OCR_SERVE_VERIFY_CLI` | `claude` | CLI of the fix run: `claude` or `codex`. `agy` and `none` are refused at startup: agy runs reviews only, and every round needs the fix run. The credential file of the CLI is required, as for the review CLI. Not used with `OCR_SERVE_ENGINE=ocr`. |
| `OCR_SERVE_VERIFY_MODEL` | empty | Model of the fix run: Claude `--model` or Codex `-m`, for example `opus`. Empty uses the model setting of that CLI (`OCR_SERVE_CLAUDE_MODEL` or `OCR_SERVE_CODEX_MODEL`), then the CLI default. Codex keeps `OCR_SERVE_CODEX_EFFORT`. |

The tests run in the fix run's shell. Use `OCR_SERVE_SANDBOX=docker`: the
[sandbox](#sandbox) image of the checkout holds the toolchain, and Claude
gets `Bash` only there. A Claude fix run without the sandbox runs no tests,
and the round comment says the tests were not run. The shell can reach the
network, so a PR that tricks the fix run could send the token of the mounted
settings or auth file out.

### Sandbox

`OCR_SERVE_SANDBOX=docker` runs every CLI process (the review and the fix
run) in a throwaway container of its own. The default
`none` runs the CLI as a child process of the service, which is right when
the service itself runs in a container (the Komodo stack). The sandbox needs
the docker CLI and a daemon the service user may use (on Linux, membership
in the `docker` group); the service does not mount the docker socket into
any container.

| Variable | Default | Notes |
|---|---|---|
| `OCR_SERVE_SANDBOX` | `none` | `none` or `docker`. Used only by the `cli` engine. |
| `OCR_SERVE_DOCKER_BIN` | `docker` | Name on `PATH` or absolute path of the docker CLI. |
| `OCR_SERVE_SANDBOX_MEMORY` | `6g` | `docker run --memory`. |
| `OCR_SERVE_SANDBOX_CPUS` | `4` | `docker run --cpus`. |
| `OCR_SERVE_SANDBOX_ADD_HOSTS` | empty | Comma-separated `host:ip` pairs for `docker run --add-host`. A container resolves names through public DNS, so a name the host resolves through Tailscale MagicDNS (for example an LLM proxy at `vtc.<tailnet>.ts.net`) needs its tailnet IP here, or the CLI cannot reach it. |
| `OCR_SERVE_SANDBOX_IMAGE_GO` | `golang:1-bookworm` | Checkout with `go.mod`. |
| `OCR_SERVE_SANDBOX_IMAGE_DOTNET` | `mcr.microsoft.com/dotnet/sdk:9.0` | `*.csproj`, `*.sln` or `global.json`. |
| `OCR_SERVE_SANDBOX_IMAGE_RUST` | `rust:1-bookworm` | `Cargo.toml`. |
| `OCR_SERVE_SANDBOX_IMAGE_NODE` | `node:22-bookworm` | `package.json`. |
| `OCR_SERVE_SANDBOX_IMAGE_PYTHON` | `python:3.12-bookworm` | `pyproject.toml` or `requirements.txt`. |
| `OCR_SERVE_SANDBOX_IMAGE_DEFAULT` | `buildpack-deps:bookworm` | None of the above. |
| `OCR_SERVE_SANDBOX_CODEX_DIR` | directory of the codex binary | Codex install mounted read-only. The binary is resolved through its links first, so `~/.local/bin/codex` mounts the release directory it points to. |
| `OCR_SERVE_SANDBOX_CLAUDE_DIR` | directory of the claude binary | Same for Claude, for example `~/.local/share/claude/versions`. |
| `OCR_SERVE_SANDBOX_AGY_DIR` | directory of the agy binary | Same for agy. The installer puts the binary itself in `~/.local/bin`, so that directory is mounted unless this names another one that holds the binary. |

Images. The service looks at the files in the checkout root and one
directory level below it (monorepos), not in hidden directories. When
several match, the first row of the table wins: Go, .NET, Rust, Node,
Python. Use Debian (glibc) images only: the Claude Code binary does not
start on Alpine (musl). An image missing on the host is pulled before the
run (`docker pull`, bounded by the limit of the step that runs it); the log line
`sandbox image` names the image and whether it was pulled.

Each run is:

```text
docker run --rm -i --name ocr-run-<id> --init --user <service uid>:<gid>
  --cap-drop ALL --security-opt no-new-privileges --pids-limit 1024
  --memory 6g --cpus 4 --network bridge -w <checkout, or the run input dir for agy>
  --env-file <run dir>/sandbox.env --mount ... <image> <cli> <args>
```

The prompt goes to the container on stdin (for agy, in the arguments, with
an empty stdin). Every mount has the same path in
the container as on the host:

| Mount | Mode |
|---|---|
| The PR checkout (`$OCR_SERVE_WORK_DIR/slot-N`) | read-write for a fix run, read-only for a review and a report-only fix run |
| The run directory (`$OCR_SERVE_CLI_HOME/run-<id>`: home, input, output) | read-write |
| `$OCR_SERVE_SKILL_DIR` | read-only |
| The CLI install (see above) | read-only |
| Codex: the directory of `OCR_SERVE_CODEX_AUTH` (`~/.codex`) | read-write, so a token refresh reaches the host; only `auth.json`, linked from the run home, is used |
| Claude: `OCR_SERVE_CLAUDE_SETTINGS` | read-only; the run home holds the copy with only `env` and `model` |
| agy: `OCR_SERVE_AGY_TOKEN` (the token file only) | read-write, so a token refresh reaches the host; linked from the run home |
| `$OCR_SERVE_DATA_DIR/cache/{go,npm,nuget,cargo,pip}` | read-write toolchain caches |
| `<run dir>/passwd`, `<run dir>/group` at `/etc/passwd`, `/etc/group` | read-only; give the service uid a user name, which some tools look up |

The container gets the CLI environment of a run without the sandbox
(`HOME`, `CODEX_HOME` or `CLAUDE_CONFIG_DIR` in the run home, `TMPDIR`,
locale, CA and proxy variables), a `PATH` that covers the image toolchains,
and the caches: `GOMODCACHE`, `GOCACHE`, `GOFLAGS=-modcacherw` (the module
cache stays deletable), `npm_config_cache`, `NUGET_PACKAGES`, `CARGO_HOME`
and `PIP_CACHE_DIR`. The caches are plain directories owned by the service
user, so repeated runs are fast and need no `chown`. The values go through
an env file, not the command line.

When the run limit ends or the service stops a run, it runs `docker kill
ocr-run-<id>` and waits for the docker client; after every run it runs
`docker rm -f ocr-run-<id>`, so no container stays behind even when the
client died first. At startup it removes every `ocr-run-*` container and run
directory a stopped service left.

What the AI can reach in the sandbox: the PR checkout, its own run
directory, the skill, the CLI install, the host `~/.codex` directory (for
codex), the Claude settings file (for claude), the agy token file (for agy),
the toolchain caches, and the network. What it cannot reach: any other host path (the service `.env`, the
GitHub App key, the forge tokens, the docker socket, other checkouts), the
host processes, and root in the container. Accepted risk: the network is
open, because the CLI needs its API and the fix run needs package registries, so
a CLI tricked by the PR content could still send the mounted auth file (and
the rest of `~/.codex`, or the Claude settings with their API token) out.
Run the service only on repositories whose authors you trust.

### OCR settings

- `config.json` (mounted at `/home/ocr/.opencodereview/config.json`; on a
  host `~/.opencodereview/config.json`, or the absolute path in
  `OCR_CONFIG_FILE`) must set `language`; the service refuses to start
  without it. Use `"Vietnamese"` for
  Vietnamese review comments. The summary and labels use the built-in
  Vietnamese texts.
- `config.json` must not contain `mcp_servers`: their setup commands would run
  inside the checkout of an untrusted PR. The service refuses to start, and
  each review refuses to run, when the key is set.
- The LLM comes from the usual OCR settings. For an OpenAI-compatible gateway
  such as 9Router, set in `.env`:

  | Variable | Example |
  |---|---|
  | `OCR_LLM_URL` | `https://router.example/v1` (`/chat/completions` is appended) |
  | `OCR_LLM_TOKEN` | gateway token |
  | `OCR_LLM_MODEL` | model name |
  | `OCR_LLM_PROTOCOL` | `openai` |
  | `OCR_LLM_TIMEOUT` | optional, seconds |

- OCR does not send `temperature` (or other sampling parameters) in review
  requests, so models that accept only the default temperature work as is.
  If a gateway needs an extra request field, `OCR_LLM_*` cannot carry it:
  configure the endpoint in the `llm` block of `config.json` with `extra_body`
  instead, and leave `OCR_LLM_*` unset, because the environment wins over the
  file. Example: `"llm": {"url": "...", "auth_token": "...", "model": "...",
  "protocol": "openai", "extra_body": {"temperature": 1}}`. Keep the file mode
  readable for uid 1000 only when it holds a token.
- A private CA: mount the bundle and set `GIT_SSL_CAINFO` (git fetch) and
  `SSL_CERT_FILE` (API calls from the service) to it; see the commented lines
  in `compose.example.yml`.

### Per-repository settings

`.opencodereview/serve.json` is read from the base commit of the PR, so a PR
cannot change its own limits. Keys: `enabled` (bool), `max_rounds` (1..10),
`exclude` (globs added to the [file filter](#file-filter)), `background` (string, at most 4000
characters), `effort` (`low`, `medium` or `high`). Unknown keys are logged and
ignored. If the file cannot be read (an API error, not a missing file), the
PR is not reviewed until the next event.

`.opencodereview/rule.json` and the rule files it names are also read from the
base commit, not from the checkout, so a PR cannot change its own review rules
or exclusions. Each file is read through the forge API and may be up to
512 KiB, as in the CLI. If `rule.json` cannot be read, the PR is not reviewed
until the next event (`read project rules` in the log). A rule file that cannot
be read, or is larger, leaves only its own rule empty (`read rule file failed`).
An invalid `rule.json` gives a review with the built-in rules only.

### File filter

A serve review skips files that take long to review and rarely hold a defect.
It applies, in addition to OCR's built-in excludes (binary files, secret
paths, unsupported extensions, dependency, build and generated directories,
test data, fixtures and snapshots):

| Glob | Skips |
|---|---|
| `**/.*/**/*` | every file inside a directory whose name starts with `.`, at any depth (`.github`, `.claude`, `.cursor`, `.vscode`, ...). A dotfile such as `.eslintrc.js` is still reviewed. |
| `**/.mcp.json` | MCP client config |
| `**/_bmad-output/**`, `**/claude-design/**`, `**/plans/**`, `**/third_party/**` | agent and tool output, plans, third-party code |
| `**/*.lock`, `**/yarn.lock`, `**/go.sum`, `**/Cargo.lock`, `**/composer.lock` | lock files |
| `**/*.map` | source maps |
| `**/locales/**/*.json`, `**/i18n/**/*.json` | translation tables |
| `**/docs/**/*.html`, `**/*.html`, `**/*.htm`, `**/*.css` | plain markup and styles |

It also skips a `.json`, `.yaml` or `.yml` file whose diff changes more than
1000 lines.

Unlike `ocr review`, a serve review covers test files (`*_test.go`,
`*.spec.ts`, `test_*.py`, `__tests__/`, ...). To skip them in a repository,
add their globs to `exclude` in its `serve.json`.

`OCR_SERVE_EXCLUDE` and the `serve.json` `exclude` key add globs to this list;
nothing removes a default. The `review done` log line counts the skipped files
by reason in `skipped_files` (`user_exclude` for the globs above,
`default_path` for OCR's built-in paths, `large_data` for the size limit).
The summary's file count covers only the files left after filtering.

## Review rounds

A chain is a series of rounds on one PR. One round:

1. The review CLI reviews the round's scope (see the table below).
2. When it finds something, the [fix run](#fix-run) keeps only the real
   findings, fixes them and runs the tests. With the OCR engine there is no
   fix run, and every finding is reported as the review gave it.
3. When the fix run changed files, the service commits as the bot (GitHub:
   `<app-slug>[bot]` with `<bot-user-id>+<app-slug>[bot]@users.noreply.github.com`;
   GitLab: the bot user's name and commit email) with the fix run's commit
   message, and pushes to the PR head branch without force. Hooks do not
   run, and a fix run that changed `.git/config`, made its own commit or left
   the branch fails the round.
4. The service posts one PR comment for the round.

| Round | Starts when | Scope | Fix and push |
|---|---|---|---|
| 1 | a non-draft PR opens or a draft becomes ready, and the PR has no OCR summary comment yet (a PR with a chain only gets its status copied) | the whole PR (merge base to head) | yes |
| 2 to `max_rounds` | the service pushed a fix in the previous round; it queues the round itself | only that fix commit | yes |
| `max_rounds` + 1 | the same, after round `max_rounds` | only the last fix commit | no: report only |

`max_rounds` is `OCR_SERVE_MAX_ROUNDS` (default 3), or `max_rounds` in the
repository's `serve.json`. A review of a fix commit does not check the PR
description or scope again. The report-only round gets a read-only checkout
and lists the real findings with the failing scenario and the proposed fix.
Nothing runs automatically after it.

The chain ends when a round has no real finding or nothing to review (for
example, every changed file is excluded): no push, so no next round. It also
ends after a round that only reports. A round only reports when:

- it is round `max_rounds` + 1;
- the PR head branch is in a fork (the bot cannot push there);
- the push is refused (protected branch, missing permission, branch moved);
  the comment quotes git's reason;
- the fix run changed no file, or fixed none of the real findings;
- the engine is `ocr`.

When the tests fail or cannot run after a fix, the fix is pushed anyway and
the comment says so with the end of the test output.

The round comment has the title "OCR review, round k/N" (or "round N+1
(report only)"), the reviewed range, and for each fixed finding the file and
line, the problem, the failing scenario, a `diff` block with the hunks of the
fix commit for that finding and the tests added. A diff over 40 lines is
collapsed in a `<details>` element. A finding that matches no hunk shows how
it was fixed in prose instead, and the service logs a warning. Hunks that no
finding claims are listed under "Other changes in the fix commit", so the
comment hides no part of the commit. Then come a link to the fix commit
(`<web>/<repo>/commit/<sha>` on GitHub, `<GitLab URL>/<project>/-/commit/<sha>`
on GitLab) and the test command with its result. Real findings that were not
fixed, PR-level ones included, are listed under "Real findings, not fixed"
with the scenario, the proposed fix and the proposed diff (at most 150
lines), and, when the round could not fix at all, why. When a push is
rejected, the proposed diff of each finding is its part of the commit that
was not pushed. A comment over 65000 characters first collapses every diff,
then shows fewer lines of the largest diffs with a note, and is cut only as a
last resort. Findings the fix run dropped are not shown. The texts come from
the service locale.

A push by a person, or a reopened PR, never starts a round: it copies the
chain's current status to the new head (see [Merge gate](#merge-gate)). The
bot's own push is filtered as a bot event; the round that pushed queues the
next round. One chain runs per PR, and the rounds of a PR run one at a time.

The summary comment holds the chain state: the status and a table with one
row per round (round, reviewed range, real findings, fixed, fix commit). The
round comments are separate comments.

Commands in a PR comment:

- `/review [--effort=low|medium|high]` restarts the chain as round 1 on the
  whole PR. A round of the old chain that runs is cancelled, and its queued
  rounds are skipped.
- `/review-commit` does the same, but round 1 covers only the latest commit.
- The command comment gets a reaction on receipt (eyes) and when round 1
  ends (GitHub: rocket; GitLab: white_check_mark); the round comment is the
  answer. A command that is rejected or whose round fails gets GitHub:
  confused or GitLab: x, plus a reply. An unknown command, `fix` included,
  gets the help reply. On GitLab, assigning the bot as reviewer acts as
  `/review`.
- A command can also tag the bot instead of starting with `/`:
  `@<bot> review --effort=high` or `@<bot> review-commit`, with an optional
  `:` or `,` after the name (`@<bot>: review`). `<bot>` is the GitHub App
  slug (the bot login without `[bot]`) or the GitLab bot username (the user
  of `OCR_SERVE_GITLAB_TOKEN`, and `OCR_SERVE_GITLAB_BOT_USERNAME` when set),
  plus any name in `OCR_SERVE_MENTIONS`; the name matches without regard to
  case. Only the first non-empty line counts, and only when the tag starts
  it: a tag later in the line or a tag of another user is an ordinary
  comment. A tag with no command or an unknown word gets the help reply. The
  help reply and the summary show the tag form with the bot name of the
  platform. The service reads the GitHub App slug at startup. When that fails
  it logs a warning and, until a later API call reads the slug, accepts only
  the slash form and the `OCR_SERVE_MENTIONS` names; the texts then show the
  first of those names, or only the slash form when none is set. Editing a
  comment from `@<bot> review` to `/review` does not run it again.

## Merge gate

The service sets the commit status `ocr-review` on the PR head:

| State | When |
|---|---|
| `pending` | the chain runs |
| `success` | the chain ended: a round had no real finding, a report-only round reported, or the bot cannot push |
| `failure` (GitLab: `failed`) | a round failed (CLI error, quota, timeout, forge error, or a shutdown during the round), until a later `review` succeeds |

When the chain ends, the current head gets the status too, in case a person
pushed while the round ran. A round that waits for a worker sets `pending`
on the head first, so a merge cannot pass before the review starts. A
service restart or crash that cuts a running or waiting round leaves the
status `pending`, and the next start runs that round again.

- GitHub: the Statuses API. Make `ocr-review` a required status check in the
  branch protection of each repository, for example:

  ```bash
  gh api -X PUT repos/OWNER/REPO/branches/main/protection \
    -F required_status_checks[strict]=false \
    -F 'required_status_checks[contexts][]=ocr-review' \
    -F enforce_admins=false -F required_pull_request_reviews=null \
    -F restrictions=null -F required_conversation_resolution=false
  ```

  GitHub enforces branch protection on private repositories only with a paid
  plan; the Free plan enforces it on public repositories.
- GitLab: `POST /projects/:id/statuses/:sha` with `name=ocr-review` and
  `ref` set to the source branch. GitLab counts the status as an external
  pipeline, so "Pipelines must succeed" blocks the merge; see
  [GitLab hooks](#gitlab-hooks). GitLab's "Cannot transition status" answer
  means the status already has that state and is not an error.

## GitHub App

Webhook URL `https://<host>/ocr/github/webhook` with the webhook secret.

- Repository permissions: Contents read and write (the fix rounds push to
  the PR branch), Pull requests read and write, Issues read and write,
  Commit statuses read and write (the [merge gate](#merge-gate)), Metadata
  read.
- Events: Pull request, Issue comment.

For the merge block, make `ocr-review` a required status check; see
[Merge gate](#merge-gate).

## GitLab hooks

`ocr serve hooks gitlab` needs an admin token (`api` and `admin_mode` scopes).
Pass it for the one run only; never put it in the service `.env`.

```bash
export OCR_SERVE_GITLAB_ADMIN_TOKEN=...   # this shell only
export OCR_SERVE_GITLAB_HOOK_URL=https://<host>/ocr/gitlab/webhook
export OCR_SERVE_GITLAB_BOT_USERNAME=<bot user>
docker compose run --rm \
  -e OCR_SERVE_GITLAB_ADMIN_TOKEN -e OCR_SERVE_GITLAB_HOOK_URL -e OCR_SERVE_GITLAB_BOT_USERNAME \
  --entrypoint opencodereview ocr-review serve hooks gitlab --dry-run
```

| Flag | Effect |
|---|---|
| `--dry-run` (default) | Lists the changes; changes nothing. |
| `--apply` | Creates or refreshes the hooks and settings. Needs `OCR_SERVE_GITLAB_WEBHOOK_SECRET` (read from `.env`). Running it twice reports 0 changes the second time. |
| `--remove` | Deletes the hooks. Bot membership and the merge setting stay. |
| `--project <group/name>` | Limits the run to one project hook; no system hook and no group work. |

Without `--project` the command manages a system hook for merge request
events, a project hook on every project for note events, the bot as Developer
in every top-level group, and turns on "Pipelines must succeed"
(`only_allow_merge_if_pipeline_succeeds`) on every project, so the
`ocr-review` status gates the merge. Run it again after new projects appear.
It does not turn off "All threads must be resolved": on a project where an
earlier version of the service set it, turn it off by hand, since old OCR
threads may stay open and block the merge.

A fix round pushes to the merge request's source branch with the bot token,
so the bot needs push rights there: Developer can push to unprotected
branches; a protected source branch must allow Developers (or the bot) to
push, otherwise the round only reports and its comment quotes the refusal.

## Operations

- Logs: JSON lines on stdout with `request_id`; OCR progress lines on stderr
  are plain text prefixed with `[ocr]`.
- `docker compose stop` sends SIGTERM: the service stops taking webhooks and
  waits up to 60 seconds for running rounds. A round cut short runs again
  after the restart (see `/data/rounds.json` below). `stop_grace_period` in the example is 75
  seconds.
- The chain state lives in the PR summary comment, so `/data` and `/work` can
  be dropped without losing it. `/data/rounds.json` lists the rounds that
  started or were queued and have not ended. A round cut by a restart or a
  crash stays in it, keeps the status `pending`, and runs again when the
  service starts. Without a data directory there is no such list: a cut
  round fails and sets `failure` until someone comments `review`.

## Upgrade from the thread flow

Earlier versions posted each finding as a review thread, gated the merge on
resolved threads and fixed on `/fix`. When you upgrade:

- These settings are now ignored; remove them: `OCR_SERVE_FIX_CLI`,
  `OCR_SERVE_VERIFY_POST`, `OCR_SERVE_POLL_INTERVAL`. The GitLab poller and
  `waiting.json` are gone; an old `waiting.json` in the data dir is not read.
- `OCR_SERVE_VERIFY_CLI` now names the fix run CLI and defaults to `claude`;
  `none` and `agy` stop the service at startup.
- `/fix` is an unknown command and gets the help reply.
- GitHub: add the App permission "Commit statuses: Read and write", drop the
  "Pull request review thread" event, and replace "Require conversation
  resolution before merging" with the required status check `ocr-review`.
- GitLab: run `ocr serve hooks gitlab --apply` to turn on "Pipelines must
  succeed", and turn off "All threads must be resolved" by hand.
- A summary comment with a marker of the old flow reads as no chain. Such a
  PR gets no automatic round 1 (it already has a summary) and no status;
  comment `review` to start a chain on it.
