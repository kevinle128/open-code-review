# syntax=docker/dockerfile:1
#
# Image for `ocr serve`, the PR review webhook service.
# Build from the repository root:
#   docker build -f deploy/serve/Dockerfile \
#     --build-arg VERSION=$(git describe --tags --always) \
#     --build-arg GIT_COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -t ocr-serve .
# The version values are build args because the build context holds no .git
# (see Dockerfile.dockerignore next to this file).

FROM golang:1.26@sha256:978e0a4e3d953662577d784471db27742f543ab09eead06839a838188c73314b AS compile
# go.mod pins the toolchain; never download another one during the build.
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG GIT_COMMIT=
ARG BUILD_DATE=
RUN go build -trimpath \
    -ldflags "-s -w -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}" \
    -o /out/opencodereview ./cmd/opencodereview

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
# git runs every checkout and diff; ca-certificates verifies the forges and
# the LLM and agent CLI endpoints; curl runs the health check. The Claude Code
# binary needs glibc, which is why the runtime is Debian and not Alpine.
# tini runs as PID 1 and reaps the tools an agent CLI leaves behind, which
# the service would otherwise keep as zombies.
#
# The user has uid and gid 1000, the owner of the host CLI installs and
# credential files that the CLI engine mounts. The directories are created
# and owned here because a single-file bind mount of config.json would
# otherwise create a root-owned parent, and session files could then not be
# written.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates curl tini \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd -g 1000 ocr \
    && useradd -u 1000 -g 1000 -m -d /home/ocr -s /usr/sbin/nologin ocr \
    && mkdir -p /home/ocr/.opencodereview /data /work \
    && chown -R ocr:ocr /home/ocr /data /work
COPY --from=compile /out/opencodereview /usr/local/bin/opencodereview
ENV HOME=/home/ocr \
    OCR_SERVE_ADDR=0.0.0.0:3012 \
    OCR_SERVE_DATA_DIR=/data \
    OCR_SERVE_WORK_DIR=/work
USER 1000:1000
WORKDIR /home/ocr
VOLUME ["/data", "/work"]
EXPOSE 3012
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -fsS -o /dev/null http://127.0.0.1:3012/healthz || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "opencodereview", "serve"]
