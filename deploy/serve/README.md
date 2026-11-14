# `ocr serve`: PR review webhook service

`ocr serve` receives GitHub App and GitLab webhooks, reviews each pull request
or merge request in bounded rounds, and posts every finding as a resolvable
review thread. The review runs an agent CLI (Codex CLI or Claude Code) by
default, or the in-process OCR review; see [Review engine](#review-engine). A sticky summary comment holds the round state.
Combined with the forge setting "all threads must be resolved", open findings
block the merge.

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
the OCR engine (`OCR_SERVE_ENGINE=ocr`); for the CLI engine, add the mounts
and settings of [Review engine](#review-engine).

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
OCR_SERVE_POLL_INTERVAL=
OCR_SERVE_WORKERS=
OCR_SERVE_REVIEW_CONCURRENCY=
OCR_SERVE_BUNDLE_LINES=
OCR_SERVE_EXCLUDE=
OCR_LLM_TIMEOUT=900
# Review engine; empty uses the default (cli with codex).
OCR_SERVE_ENGINE=
OCR_SERVE_REVIEW_CLI=
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
| `data/` | `OCR_SERVE_DATA_DIR`: `waiting.json`, `cli-home/` (run dirs), `cache/` (toolchain caches). |
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
# Optional, as on the stack: OCR_SERVE_REVIEW_CLI, OCR_SERVE_FIX_CLI,
# OCR_SERVE_CODEX_EFFORT, OCR_SERVE_MAX_ROUNDS, OCR_SERVE_EXCLUDE, ...
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
| `OCR_SERVE_MAX_ROUNDS` | `3` | Automatic rounds per PR, 1..10. |
| `OCR_SERVE_POLL_INTERVAL` | `2m` | How often waiting GitLab MRs are checked for resolved threads. |
| `OCR_SERVE_WORKERS` | `2` | Concurrent reviews, 1..16; also the number of checkout slots. |
| `OCR_SERVE_REVIEW_TIMEOUT` | `2h` | Limit for one review run, timed from when a worker takes it. Each file group in the review has a limit of 120 minutes per review round. |
| `OCR_SERVE_REVIEW_CONCURRENCY` | `5` | LLM subtasks that one review runs at the same time, 1..32. `ocr review --concurrency` keeps its default of 8. |
| `OCR_SERVE_BUNDLE_LINES` | `300` | A PR with fewer files than the grouping minimum (4) and fewer changed lines than this value is reviewed as one group; otherwise each file is a separate group. 50..5000. Replaces the template value of 200 for serve reviews only. |
| `OCR_SERVE_MAX_BODY_BYTES` | `10485760` | Webhook body limit. |
| `OCR_SERVE_DATA_DIR` | `~/ocr-review/data` | Holds `waiting.json` (GitLab MRs waiting for resolution), the CLI run dirs and the sandbox caches. The image sets `/data`; without `HOME` the default is `/data`. A relative path is made absolute. |
| `OCR_SERVE_WORK_DIR` | `~/ocr-review/work` | Checkout slots `slot-N`, emptied after each run. The image sets `/work`; without `HOME` the default is `/work`. |
| `OCR_SERVE_IGNORE_REPOS` | empty | Comma-separated RE2 patterns; match anywhere in the value. |
| `OCR_SERVE_IGNORE_AUTHORS` | empty | Same format. |
| `OCR_SERVE_IGNORE_TITLES` | empty | Same format. |
| `OCR_SERVE_IGNORE_SOURCE_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_TARGET_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_LABELS` | empty | Comma-separated, exact match. |
| `OCR_SERVE_EXCLUDE` | empty | Comma-separated globs added to the serve file filter for every repository; see [File filter](#file-filter). |
| `OCR_SERVE_BOT_INDICATORS` | `bot` | Substrings that mark a GitLab sender as a bot. |
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

`OCR_SERVE_ENGINE` picks the engine. Both post findings the same way: inline
threads, the summary marker, rounds and the merge gate do not change.

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
  an empty path, or on `pr.json`), becomes a PR-level thread: on GitLab a
  discussion without a position, on GitHub a file-level thread on the first
  changed file, since only review threads can be resolved there; its body
  says it is about the whole PR. Both block the merge until resolved. The
  skill is not part of this repository.

  A large PR is reviewed in batches, one CLI run each, in its own run
  directory. Files of one directory stay in one batch where they fit. Each
  batch holds at most `OCR_SERVE_BATCH_FILES` files and about
  `OCR_SERVE_BATCH_LINES` changed lines (a larger single file is its own
  batch); a PR within both limits is one run. In a batch, `pr.json` keeps
  the whole PR and every selected file in `files`, and adds `batch`
  (`index` from 1, `count`, and the batch `files`); `diff.patch` holds only
  the batch files, and the dismissed findings go to every batch. Only the
  first batch checks the PR description. Findings are merged, and a finding
  with the same path, line and title as an earlier one is dropped. The run
  timeout covers all batches. When a batch fails, the others still post
  their findings and the review is partial: the summary lists the files of
  the failed batch, and `/review` reviews again. Each batch logs one
  `review batch` line with its files, changed lines, findings and duration.
  Batching applies to every review (automatic rounds, `/review`,
  `/review-commit` and the review after a `/fix` commit); `/fix` is one run.
- `ocr` runs the in-process OCR review with the `OCR_LLM_*` settings. Use it
  to roll back: set `OCR_SERVE_ENGINE=ocr` and redeploy. The CLI settings and
  mounts are then not used.

| Variable | Default | Notes |
|---|---|---|
| `OCR_SERVE_ENGINE` | `cli` | `cli` or `ocr`. |
| `OCR_SERVE_REVIEW_CLI` | `codex` | CLI for reviews: `codex` or `claude`. |
| `OCR_SERVE_FIX_CLI` | `claude` | CLI for `/fix` runs: `claude` or `codex`. Its credential file is required too. |
| `OCR_SERVE_CODEX_BIN` | `codex` | Name on `PATH` or absolute path. The Komodo stack sets the mounted binary. |
| `OCR_SERVE_CLAUDE_BIN` | `claude` | Same. |
| `OCR_SERVE_CODEX_MODEL` | empty | Codex `-m`; empty uses the account default. |
| `OCR_SERVE_CODEX_EFFORT` | empty | Codex `model_reasoning_effort`, for example `medium`. |
| `OCR_SERVE_CLAUDE_MODEL` | empty | Claude `--model`; empty uses the `model` of the settings file. |
| `OCR_SERVE_SKILL_DIR` | | Required with `cli`. Absolute path of the directory that holds `skills/ak-review-pr/SKILL.md` and its `references/`, and optionally `agents/kongming.md` for `/fix`. |
| `OCR_SERVE_CLI_HOME` | `$OCR_SERVE_DATA_DIR/cli-home` | Directory the service owns. Each run gets `run-<id>/` in it, removed when the run ends; leftovers are removed at startup. |
| `OCR_SERVE_CODEX_AUTH` | | Required with `codex`. The host `~/.codex/auth.json`. Each run's `CODEX_HOME` links `auth.json` to it; nothing else of the host `~/.codex` is used. Mount it read-write so a token refresh reaches the host file. |
| `OCR_SERVE_BATCH_LINES` | `1500` | Changed lines (insertions plus deletions) per review batch, `100`..`50000`. |
| `OCR_SERVE_BATCH_FILES` | `25` | Files per review batch, `1`..`500`. |
| `OCR_SERVE_BATCH_CONCURRENCY` | `1` | Review batches of one PR that run at the same time, `1`..`4`. Each is a full CLI run (and a sandbox container), on top of `OCR_SERVE_WORKERS`. |
| `OCR_SERVE_CLAUDE_SETTINGS` | | Required with `claude`. The host `~/.claude/settings.json`. Each run's `CLAUDE_CONFIG_DIR` gets a `settings.json` with only its `env` and `model` keys, never hooks or permissions. |

At startup the service checks that the review and fix CLI binaries, the
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
  skill and run directories. A `/fix` run uses `--permission-mode acceptEdits`
  and the tools `Read`, `Edit`, `MultiEdit`, `Write`, `Glob`, `Grep` and
  `Task`, plus `Bash` only with `OCR_SERVE_SANDBOX=docker`, where the shell
  runs in the run's container (see [Sandbox](#sandbox)). Without the sandbox
  it gets no shell.
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
| `/home/vtcm/ocr-review/skills` (`OCR_HOST_SKILL_DIR`) | `/opt/ocr-skills` | read-only |

`OCR_SERVE_CLAUDE_BIN` defaults to `/opt/claude/versions/2.1.322` and
`OCR_SERVE_CODEX_BIN` to
`/opt/codex/releases/0.163.0-x86_64-unknown-linux-musl/bin/codex`. When a CLI
on the host updates and removes the old version, set the variable to the new
path and redeploy. To switch the review CLI, set `OCR_SERVE_REVIEW_CLI` and
redeploy.

### Sandbox

`OCR_SERVE_SANDBOX=docker` runs every CLI process (the codex review, the
claude or codex `/fix`) in a throwaway container of its own. The default
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

Images. The service looks at the files in the checkout root and one
directory level below it (monorepos), not in hidden directories. When
several match, the first row of the table wins: Go, .NET, Rust, Node,
Python. Use Debian (glibc) images only: the Claude Code binary does not
start on Alpine (musl). An image missing on the host is pulled before the
run (`docker pull`, bounded by `OCR_SERVE_REVIEW_TIMEOUT`); the log line
`sandbox image` names the image and whether it was pulled.

Each run is:

```text
docker run --rm -i --name ocr-run-<id> --init --user <service uid>:<gid>
  --cap-drop ALL --security-opt no-new-privileges --pids-limit 1024
  --memory 6g --cpus 4 --network bridge -w <checkout>
  --env-file <run dir>/sandbox.env --mount ... <image> <cli> <args>
```

The prompt goes to the container on stdin. Every mount has the same path in
the container as on the host:

| Mount | Mode |
|---|---|
| The PR checkout (`$OCR_SERVE_WORK_DIR/slot-N`) | read-write for `/fix`, read-only for a review |
| The run directory (`$OCR_SERVE_CLI_HOME/run-<id>`: home, input, output) | read-write |
| `$OCR_SERVE_SKILL_DIR` | read-only |
| The CLI install (see above) | read-only |
| Codex: the directory of `OCR_SERVE_CODEX_AUTH` (`~/.codex`) | read-write, so a token refresh reaches the host; only `auth.json`, linked from the run home, is used |
| Claude: `OCR_SERVE_CLAUDE_SETTINGS` | read-only; the run home holds the copy with only `env` and `model` |
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
codex), the Claude settings file (for claude), the toolchain caches, and the
network. What it cannot reach: any other host path (the service `.env`, the
GitHub App key, the forge tokens, the docker socket, other checkouts), the
host processes, and root in the container. Accepted risk: the network is
open, because the CLI needs its API and `/fix` needs package registries, so
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

- Round 1 runs when a non-draft PR opens, is reopened, or a draft becomes
  ready. It reviews the whole PR (merge base to HEAD).
- A push while OCR threads are open does nothing. When every OCR thread is
  resolved and HEAD moved, the next round reviews the new commits only
  (reviewed SHA to HEAD). GitHub reports resolution by webhook; GitLab has no
  such event, so waiting MRs are polled and re-checked on push and comments.
- A force-push that drops the reviewed commit gives a full review, which still
  counts as one round.
- After `max_rounds` automatic rounds the service posts one note and stops
  automatic reviews for that PR.
- A review that fails does not count as a round; the next trigger retries.
  A review that finds nothing to review (for example, every changed file is
  excluded) does not count either: it moves the reviewed SHA so the same
  commits are not reviewed again, and the summary says so.
- Commands in a PR comment always run and never count toward the limit:
  `/review [--effort=low|medium|high]` reviews the whole PR and moves the
  reviewed SHA; `/review-commit` reviews the HEAD commit against its first
  parent; `/fix` fixes the open findings and pushes (see
  [Fixing findings](#fixing-findings-fix)). Their findings are threads that must be resolved too. The command
  comment gets a reaction on receipt (eyes) and on the result (GitHub: rocket
  or confused; GitLab: white_check_mark or x), plus a reply. On GitLab,
  assigning the bot as reviewer of a non-draft MR acts as `/review`.

## Fixing findings (`/fix`)

A PR comment `/fix` (no arguments) asks the service to fix the open finding
threads and push the fix to the PR branch. Like `/review` it always runs,
never counts as a round, is queued with the other runs of the PR, and gets
the same reactions. It needs `OCR_SERVE_ENGINE=cli`; with `ocr` it answers
that it is not available.

1. A PR whose head branch is in a fork gets a reply that the bot cannot push
   there, and nothing runs. With no open finding thread it replies that there
   is nothing to fix.
2. The service checks out the head branch itself (not detached) with a token
   that can push, and writes `pr.json` and `findings.json` into the run
   directory. `findings.json` lists the open threads of the bot: `id` (the
   finding tag), `path`, `line`, `severity`, `title`, `body`, and `replies`
   (`author`, `body`) from people in the thread; the agent skips a finding
   they disputed.
3. `OCR_SERVE_FIX_CLI` runs the skill as `/ak-review-pr <PR> --advice --fix
   --reply` in its "Service fix mode" section, read from
   `$OCR_SERVE_SKILL_DIR/skills/ak-review-pr/references/mode-fix.md`; the
   prompt holds the full text of `mode-fix.md` and `review-checklist.md`. For
   Claude, `$OCR_SERVE_SKILL_DIR/agents/kongming.md` is installed into the
   run's config dir with `model: inherit`, read-only tools and no project
   memory; without the file the run goes on without it. The agent answers
   with `fixed`, `skipped` (id and reason), `summary` and `commit_message`.
4. When the checkout changed, the service commits as the bot (GitHub:
   `<app-slug>[bot]` with `<bot-user-id>+<app-slug>[bot]@users.noreply.github.com`;
   GitLab: the bot user's name and commit email) with the agent's message cut
   to one line (`fix: address review findings` when empty), and pushes without
   force. Hooks do not run, and a run that changed `.git/config`, made its own
   commit or left the branch is refused. A refused push (protected branch,
   missing permission, branch moved) gets a reply with git's reason. Without
   changes it posts the summary and the skipped findings and stops.
5. After the push it resolves the fixed threads, replies in each skipped
   thread with the reason (the thread stays open), posts one note with the
   summary, the commit and the counts, and reviews the fix commit as
   `/review-commit` does, without counting a round.

The bot's own push does not start an automatic round: GitHub reports it with
the App as sender, GitLab with the bot user, and both are filtered as bot
events. `OCR_SERVE_REVIEW_TIMEOUT` limits a fix run as it limits a review; the
run directory is removed and the CLI process group is stopped when it ends.

Reviews remember dismissed findings: a resolved finding thread with at least
one reply from a person is passed to the review CLI as `dismissed.json`
(`path`, `line`, `title`, `body`, `reason` = the replies), and the agent is told
not to report it again. A thread resolved without a reply counts as fixed.

## GitHub App

Webhook URL `https://<host>/ocr/github/webhook` with the webhook secret.

- Repository permissions: Contents read and write (`/fix` pushes to the PR
  branch; read is enough without it), Pull requests read and write, Issues
  read and write, Metadata read.
- Events: Pull request, Issue comment, Pull request review thread.

For the merge block, enable "Require conversation resolution before merging"
in the branch protection of each repository.

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
in every top-level group, and turns on "All threads must be resolved" on every
project. Run it again after new projects appear.

`/fix` pushes to the merge request's source branch with the bot token, so the
bot needs push rights there: Developer can push to unprotected branches; a
protected source branch must allow Developers (or the bot) to push, otherwise
`/fix` replies that the push was refused.

## Operations

- Logs: JSON lines on stdout with `request_id`; OCR progress lines on stderr
  are plain text prefixed with `[ocr]`.
- `docker compose stop` sends SIGTERM: the service stops taking webhooks and
  waits up to 60 seconds for running reviews. A review cut short does not
  count as a round. `stop_grace_period` in the example is 75 seconds.
- The round state lives in the PR summary comment, so `/data` and `/work` can
  be dropped without losing rounds. Without `/data/waiting.json`, a GitLab MR
  that was waiting for resolution is checked again on its next push or
  comment instead of by the poller.
