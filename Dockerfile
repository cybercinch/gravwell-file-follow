# Gravwell File Follow + Docker Log Watcher
# Multi-architecture build (linux/amd64, linux/arm64)
#
# Three-stage build:
#   1. watcher-builder  – compiles docker-log-watcher (Go)
#   2. ff-builder       – compiles gravwell_file_follow from source (Go)
#   3. runtime          – minimal busybox image with tini as PID 1

# =============================================================================
# Stage 1: Build docker-log-watcher
# =============================================================================
FROM golang:1.26.8-bookworm AS watcher-builder

# TARGETARCH is injected by Buildx: "amd64" or "arm64".
ARG TARGETARCH

WORKDIR /src

# Copy go.mod / go.sum first so Docker caches the module download layer.
COPY watcher/go.mod watcher/go.sum ./
RUN go mod download

# Copy the rest of the watcher source and build a fully-static binary.
# GOARCH is set explicitly so cross-compilation works when building arm64
# on an amd64 host (or vice-versa).
COPY watcher/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /docker-log-watcher \
    ./cmd/docker-log-watcher

# =============================================================================
# Stage 2: Build gravwell_file_follow from source + fetch tini
# =============================================================================
FROM golang:1.26.8-bookworm AS ff-builder

ARG GRAVWELL_VERSION=v3.8.84
# TARGETARCH is injected by Buildx: "amd64" or "arm64".
ARG TARGETARCH

WORKDIR /build

RUN apt-get update && \
    apt-get install -y --no-install-recommends git ca-certificates wget && \
    rm -rf /var/lib/apt/lists/* && \
    git clone --depth 1 --branch ${GRAVWELL_VERSION} \
        https://github.com/gravwell/gravwell.git .

WORKDIR /build/ingesters/fileFollow

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /gravwell_file_follow .

# Download the arch-correct static tini binary.
# TARGETARCH is "amd64" or "arm64" — matches tini's release naming exactly.
RUN wget -qO /tini \
        "https://github.com/krallin/tini/releases/download/v0.19.0/tini-static-${TARGETARCH}" && \
    chmod 755 /tini

# =============================================================================
# Stage 3: Minimal runtime image
# =============================================================================
FROM busybox:stable AS runtime

ARG GRAVWELL_VERSION=v3.8.84
# TARGETARCH is injected by Buildx: "amd64" or "arm64".
ARG TARGETARCH

# ── CA certificates ──────────────────────────────────────────────────────────
COPY --from=ff-builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Optional: append a custom CA certificate bundle for private/self-signed TLS.
# Supply a PEM file at build time with: --build-arg CUSTOM_CA=path/to/ca.crt
# If not provided, the placeholder (empty file) is a no-op.
ARG CUSTOM_CA=docker/empty.crt
COPY ${CUSTOM_CA} /tmp/custom-ca.crt
RUN if [ -s /tmp/custom-ca.crt ]; then \
        cat /tmp/custom-ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
        echo "Custom CA appended to trust store."; \
    fi && \
    rm -f /tmp/custom-ca.crt

# ── tini (init process / zombie reaper) ─────────────────────────────────────
# Fetched in the ff-builder stage using TARGETARCH so it is always
# arch-correct for both linux/amd64 and linux/arm64.
COPY --from=ff-builder /tini /sbin/tini

# ── Directory structure ──────────────────────────────────────────────────────
RUN mkdir -p \
        /opt/gravwell/bin \
        /opt/gravwell/etc/file_follow.conf.d \
        /opt/gravwell/log \
        /opt/gravwell/cache

# ── Binaries ─────────────────────────────────────────────────────────────────
COPY --from=ff-builder      /gravwell_file_follow      /opt/gravwell/bin/gravwell_file_follow
COPY --from=watcher-builder /docker-log-watcher        /usr/local/bin/docker-log-watcher

RUN chmod 755 /opt/gravwell/bin/gravwell_file_follow \
              /usr/local/bin/docker-log-watcher

# ── Config & entrypoint ──────────────────────────────────────────────────────
# Template lives at /etc/gravwell/ — outside the state volume mount point
# (/opt/gravwell/etc) so it can never be shadowed by volume contents.
COPY file_follow.conf.template /etc/gravwell/file_follow.conf.template
COPY entrypoint.sh             /usr/local/bin/entrypoint.sh

RUN chmod 755 /usr/local/bin/entrypoint.sh && \
    chmod 640 /etc/gravwell/file_follow.conf.template

# ── Runtime defaults ─────────────────────────────────────────────────────────
ENV GRAVWELL_INGEST_SECRET=IngestSecrets \
    GRAVWELL_CLEARTEXT_TARGETS="" \
    GRAVWELL_ENCRYPTED_TARGETS="" \
    GRAVWELL_LOG_LEVEL=INFO \
    WATCHER_CONF_DIR=/opt/gravwell/etc/file_follow.conf.d \
    WATCHER_DEFAULT_TAG=docker \
    WATCHER_SELF_CONTAINER=gravwell-file-follow \
    WATCHER_DEBOUNCE_SECS=2 \
    WATCHER_LOG_LEVEL=info \
    WATCHER_EXCLUDE_NAMES=""

WORKDIR /opt/gravwell

# tini as PID 1 — handles zombie reaping and clean signal forwarding.
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/entrypoint.sh"]

# ── Image metadata ───────────────────────────────────────────────────────────
LABEL org.opencontainers.image.title="Gravwell File Follow + Docker Log Watcher" \
      org.opencontainers.image.description="Auto-configuring Gravwell file_follow ingester with per-container log routing" \
      org.opencontainers.image.version="${GRAVWELL_VERSION}" \
      org.opencontainers.image.vendor="CyberCinch" \
      org.opencontainers.image.source="https://github.com/cybercinch/gravwell-file-follow"
