# SPDX-License-Identifier: MIT
# Multi-stage build for go-mtec2mqtt.
#
# CGO is disabled so the resulting binary is statically linked against
# Go's net package — that means the runtime image can be distroless,
# carrying no shell, no package manager and no userland to attack.
#
# registers.yaml is copied alongside the binary (the project's
# deliberate "no go:embed" choice) so an operator can override it via
# a bind-mount without rebuilding.

# ---------- Stage 1: build ----------
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache go.mod / go.sum separately so unrelated source edits don't
# bust the dependency-download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

ENV CGO_ENABLED=0
RUN go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/mtec2mqtt ./cmd/mtec2mqtt && \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.Version=${VERSION} \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.Commit=${COMMIT} \
        -X github.com/SukramJ/go-mtec2mqtt/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/mtec-util ./cmd/mtec-util

# ---------- Stage 2: runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# Binary + YAML assets — registers.yaml lives next to the binary so
# the daemon's locator picks it up via os.Executable.
COPY --from=builder /out/mtec2mqtt /out/mtec-util /app/
COPY --from=builder /src/registers.yaml /src/config-template.yaml /app/

# /config is the canonical mount point for the operator's config.yaml.
# XDG_CONFIG_HOME steers config.Locate at the mount so a `docker run -v
# ./my-config:/config:ro` Just Works.
VOLUME ["/config"]
ENV XDG_CONFIG_HOME=/config

USER nonroot:nonroot
ENTRYPOINT ["/app/mtec2mqtt"]
