#!/usr/bin/env bash

# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 alibaba/open-code-review Contributors

# Builds `ocr` from a branch and (re)starts it under PM2 as `ocr-review`.
#
#   deploy.sh              fetch, build, replace bin/ocr, reload, health check
#   deploy.sh --rollback   put bin/ocr.prev back, reload, health check
#
# The previous binary is kept as bin/ocr.prev only when it was serving
# (healthy) at deploy time, so a failed deploy run twice does not replace
# the last good binary. The health check waits until /healthz reports the
# version of the new binary.
#
#   OCR_REVIEW_ROOT           default ~/ocr-review
#   OCR_REVIEW_SRC            default $OCR_REVIEW_ROOT/src
#   OCR_REVIEW_REPO           clone URL when src does not exist yet
#   OCR_REVIEW_BRANCH         default main
#   OCR_REVIEW_HEALTH_URL     default http://127.0.0.1:3012/healthz
#   OCR_REVIEW_HEALTH_TRIES   default 60, 2 seconds apart; covers the drain
#                             of running reviews (up to 75 seconds)

set -euo pipefail

ROOT="${OCR_REVIEW_ROOT:-$HOME/ocr-review}"
SRC="${OCR_REVIEW_SRC:-$ROOT/src}"
REPO="${OCR_REVIEW_REPO:-https://github.com/kevinle128/open-code-review.git}"
BRANCH="${OCR_REVIEW_BRANCH:-main}"
HEALTH_URL="${OCR_REVIEW_HEALTH_URL:-http://127.0.0.1:3012/healthz}"
HEALTH_TRIES="${OCR_REVIEW_HEALTH_TRIES:-60}"
BIN_DIR="$ROOT/bin"
BIN="$BIN_DIR/ocr"
PREV="$BIN_DIR/ocr.prev"
ECOSYSTEM="$SRC/deploy/pm2/ecosystem.config.cjs"

log() { printf '[deploy] %s\n' "$*"; }
die() { printf '[deploy] error: %s\n' "$*" >&2; exit 1; }

# binary_version prints the version a binary reports, which /healthz shows.
binary_version() {
  "$1" --version | awk 'NR == 1 { print $2 }'
}

healthy_once() {
  curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1
}

# wait_healthy waits until /healthz answers ok with the given version.
wait_healthy() {
  local want="$1" body i
  for ((i = 1; i <= HEALTH_TRIES; i++)); do
    if body="$(curl -fsS --max-time 5 "$HEALTH_URL" 2>/dev/null)" &&
      [[ "$body" == *'"status":"ok"'* && "$body" == *"\"version\":\"$want\""* ]]; then
      log "healthy: $body"
      return 0
    fi
    sleep 2
  done
  return 1
}

reload() {
  [[ -f "$ECOSYSTEM" ]] || die "missing $ECOSYSTEM"
  OCR_REVIEW_ROOT="$ROOT" OCR_REVIEW_BIN="$BIN" pm2 startOrReload "$ECOSYSTEM" --update-env
  pm2 save >/dev/null
}

rollback() {
  [[ -x "$PREV" ]] || die "no previous binary at $PREV"
  cp -p "$PREV" "$BIN.new"
  mv -f "$BIN.new" "$BIN"
  local version
  version="$(binary_version "$BIN")"
  log "rolled back to $version"
  reload
  wait_healthy "$version" || die "service not healthy after rollback; see: pm2 logs ocr-review"
}

deploy() {
  command -v go >/dev/null || die "go is not on PATH"
  command -v pm2 >/dev/null || die "pm2 is not on PATH"
  mkdir -p "$BIN_DIR"
  if [[ ! -d "$SRC/.git" ]]; then
    log "cloning $REPO into $SRC"
    git clone --quiet --branch "$BRANCH" "$REPO" "$SRC"
  fi
  log "fetching $BRANCH"
  git -C "$SRC" fetch --quiet --prune origin "+refs/heads/$BRANCH:refs/remotes/origin/$BRANCH"
  git -C "$SRC" checkout --quiet --force -B "$BRANCH" "origin/$BRANCH"

  local version commit date
  version="$(git -C "$SRC" describe --tags --always)"
  commit="$(git -C "$SRC" rev-parse --short HEAD)"
  date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  log "building $version ($commit)"
  (cd "$SRC" && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.Version=$version -X main.GitCommit=$commit -X main.BuildDate=$date" \
    -o "$BIN.new" ./cmd/opencodereview)
  [[ "$(binary_version "$BIN.new")" == "$version" ]] || die "the new binary does not run"

  if [[ -x "$BIN" ]] && healthy_once; then
    cp -p "$BIN" "$PREV"
    log "kept the running binary as $PREV"
  fi
  # A rename replaces the file in one step; the running process keeps the
  # old one open until PM2 restarts it.
  mv -f "$BIN.new" "$BIN"
  reload
  if ! wait_healthy "$version"; then
    die "service not healthy with $version; see: pm2 logs ocr-review; restore with: $0 --rollback"
  fi
}

case "${1:-}" in
  "") deploy ;;
  --rollback) rollback ;;
  *) die "usage: $0 [--rollback]" ;;
esac
