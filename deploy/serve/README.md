# `ocr serve`: PR review webhook service

`ocr serve` receives GitHub App and GitLab webhooks, reviews each pull request
or merge request with OCR in bounded rounds, and posts every finding as a
resolvable review thread. A sticky summary comment holds the round state.
Combined with the forge setting "all threads must be resolved", open findings
block the merge.

`ocr serve hooks gitlab` registers the webhooks on a GitLab instance.

## Files in this directory

| File | Purpose |
|---|---|
| `Dockerfile` | Multi-stage image: static binary on Alpine with `git` and `ca-certificates`, user `ocr` (uid 10001), listens on `0.0.0.0:3012`. |
| `Dockerfile.dockerignore` | Keeps the build context to `go.mod`, `go.sum`, `cmd/` and `internal/`. |
| `compose.example.yml` | Example service. Copy it to `compose.yml`; keep `.env`, `config.json` and `github-app.pem` next to it and out of git. |

## Quick start

```bash
cd deploy/serve
cp compose.example.yml compose.yml
printf '{"language": "Vietnamese"}\n' > config.json
# Write .env (see the table below), then copy the GitHub App key:
install -m 600 /path/to/app.private-key.pem github-app.pem
sudo chown 10001:10001 github-app.pem   # the container runs as uid 10001
OCR_GIT_COMMIT=$(git rev-parse --short HEAD) docker compose build
docker compose up -d
curl -s http://127.0.0.1:3012/healthz
```

Without GitHub, remove the `github-app.pem` volume and the
`OCR_SERVE_GITHUB_PRIVATE_KEY_FILE` line from `compose.yml`.

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
| `OCR_SERVE_REVIEW_TIMEOUT` | `1h` | Limit for one review run, timed from when a worker takes it. Each file group in the review has a limit of 60 minutes per review round. |
| `OCR_SERVE_MAX_BODY_BYTES` | `10485760` | Webhook body limit. |
| `OCR_SERVE_DATA_DIR` | `/data` | Holds `waiting.json` (GitLab MRs waiting for resolution). |
| `OCR_SERVE_WORK_DIR` | `/work` | Checkout slots `slot-N`, emptied after each run. |
| `OCR_SERVE_IGNORE_REPOS` | empty | Comma-separated RE2 patterns; match anywhere in the value. |
| `OCR_SERVE_IGNORE_AUTHORS` | empty | Same format. |
| `OCR_SERVE_IGNORE_TITLES` | empty | Same format. |
| `OCR_SERVE_IGNORE_SOURCE_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_TARGET_BRANCHES` | empty | Same format. |
| `OCR_SERVE_IGNORE_LABELS` | empty | Comma-separated, exact match. |
| `OCR_SERVE_BOT_INDICATORS` | `bot` | Substrings that mark a GitLab sender as a bot. |
| `OCR_SERVE_GITHUB_APP_ID` | empty | Enables GitHub. |
| `OCR_SERVE_GITHUB_PRIVATE_KEY` / `_FILE` | | Required with GitHub. Compose sets `_FILE`. |
| `OCR_SERVE_GITHUB_WEBHOOK_SECRET` / `_FILE` | | Required with GitHub. |
| `OCR_SERVE_GITHUB_API_URL` | `https://api.github.com` | |
| `OCR_SERVE_GITLAB_URL` | empty | Enables GitLab, for example `https://git.example.com`. Its scheme and host must match the instance's `external_url`: a project clone URL on another scheme or host is refused, so the token is not sent there. |
| `OCR_SERVE_GITLAB_TOKEN` / `_FILE` | | Required with GitLab. Token of the bot user that posts reviews. The service reads `GET /user` at startup and fails when the token is rejected. |
| `OCR_SERVE_GITLAB_WEBHOOK_SECRET` / `_FILE` | | Required with GitLab. |

At least one platform must be enabled; otherwise the service stops with
`no platform enabled: set OCR_SERVE_GITHUB_APP_ID or OCR_SERVE_GITLAB_URL`.

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
  readable for uid 10001 only when it holds a token.
- A private CA: mount the bundle and set `GIT_SSL_CAINFO` (git fetch) and
  `SSL_CERT_FILE` (API calls from the service) to it; see the commented lines
  in `compose.example.yml`.

### Per-repository settings

`.opencodereview/serve.json` is read from the base commit of the PR, so a PR
cannot change its own limits. Keys: `enabled` (bool), `max_rounds` (1..10),
`exclude` (globs added to OCR excludes), `background` (string, at most 4000
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
  parent. Their findings are threads that must be resolved too. The command
  comment gets a reaction on receipt (eyes) and on the result (GitHub: rocket
  or confused; GitLab: white_check_mark or x), plus a reply. On GitLab,
  assigning the bot as reviewer of a non-draft MR acts as `/review`.

## GitHub App

Webhook URL `https://<host>/ocr/github/webhook` with the webhook secret.

- Repository permissions: Contents read, Pull requests read and write, Issues
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
