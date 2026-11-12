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

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
# git runs every checkout and diff; ca-certificates verifies the forges and
# the LLM endpoint. The directories are created and owned here because a
# single-file bind mount of config.json would otherwise create a root-owned
# parent, and session files could then not be written.
RUN apk add --no-cache git ca-certificates \
    && addgroup -S -g 10001 ocr \
    && adduser -S -D -u 10001 -G ocr -h /home/ocr ocr \
    && mkdir -p /home/ocr/.opencodereview /data /work \
    && chown -R ocr:ocr /home/ocr /data /work
COPY --from=compile /out/opencodereview /usr/local/bin/opencodereview
ENV HOME=/home/ocr \
    OCR_SERVE_ADDR=0.0.0.0:3012 \
    OCR_SERVE_DATA_DIR=/data \
    OCR_SERVE_WORK_DIR=/work
USER ocr
WORKDIR /home/ocr
VOLUME ["/data", "/work"]
EXPOSE 3012
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- http://127.0.0.1:3012/healthz >/dev/null || exit 1
ENTRYPOINT ["opencodereview", "serve"]
