# syntax=docker/dockerfile:1.7
#
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Multi-stage distroless build for the `switchboard` binary. Mirrors
# core-agent's / k8s-lookout's Dockerfile conventions (alpine builder,
# distroless/static final stage, version stamped via -ldflags). Keeping
# switchboard distroless preserves the same minimal-attack-surface
# posture as the core-agent brain it sits beside.

# ---- Builder stage ----
ARG GO_VERSION=1.26.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Cache module downloads in a separate layer.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Build-time inputs; release-images.yml overrides them all.
ARG VERSION=v0.0.0-dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 is mandatory — a fully-static binary that drops into
# distroless/static without any glibc/musl runtime.
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

RUN go build \
    -ldflags "-s -w \
      -X github.com/go-steer/switchboard/internal/version.Version=${VERSION} \
      -X github.com/go-steer/switchboard/internal/version.Commit=${COMMIT} \
      -X github.com/go-steer/switchboard/internal/version.Date=${BUILD_DATE}" \
    -trimpath \
    -o /out/switchboard \
    ./cmd/switchboard

# ---- Final stage ----
# distroless/static-debian12:nonroot — CA certs, /etc/passwd with the
# nonroot user, tzdata; no shell, no package manager, no userland.
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/go-steer/switchboard" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /out/switchboard /switchboard

WORKDIR /workspace

USER nonroot:nonroot

# Default subcommand is `serve`; container args splice in after it.
# Other subcommands (`switchboard version`) are reachable by overriding
# the entrypoint.
ENTRYPOINT ["/switchboard", "serve"]
