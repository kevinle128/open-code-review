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
| `OCR_SERVE_DATA_DIR` | `/data` | Holds `waiting.json` (GitLab MRs waiting for resolution). |
| `OCR_SERVE_WORK_DIR` | `/work` | Checkout slots `slot-N`, emptied after each run. |
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
  `$OCR_SERVE_SKILL_DIR/skills/ak-review-pr/SKILL.md` at run time, plus the
  service instructions: read those two files, do not edit files or use the
  network, and answer with findings as JSON. Findings on files outside the
  selection are dropped. The skill is not part of this repository.
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
  `Task`, never a shell.
- The CLI gets `PATH`, locale, time zone, CA and proxy variables, and `HOME`
  and `TMPDIR` inside the run directory; none of the service secrets. It runs
  in its own process group: when the review limit ends it gets SIGTERM, then
  SIGKILL after 10 seconds, and whatever it started is killed when it exits.
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

### OCR settings

- `config.json` (mounted at `/home/ocr/.opencodereview/config.json`) must set
  `language`; the service refuses to start without it. Use `"Vietnamese"` for
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
   `$OCR_SERVE_SKILL_DIR/skills/ak-review-pr/references/mode-fix.md`. For
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
