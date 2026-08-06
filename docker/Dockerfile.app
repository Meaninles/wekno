# Build stage
ARG BASE_IMAGE_REGISTRY_ARG=docker.io/library
FROM ${BASE_IMAGE_REGISTRY_ARG}/golang:1.26-bookworm AS builder

WORKDIR /app

# 通过构建参数接收敏感信息
ARG GOPRIVATE_ARG
ARG GOPROXY_ARG
ARG GOSUMDB_ARG=off
ARG APK_MIRROR_ARG

# 设置Go环境变量
ENV GOPRIVATE=${GOPRIVATE_ARG}
ENV GOPROXY=${GOPROXY_ARG}
ENV GOSUMDB=${GOSUMDB_ARG}

# Install dependencies
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y git build-essential libsqlite3-dev

# Install migrate tool
ARG MIGRATE_VERSION_ARG=v4.19.1
RUN go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@${MIGRATE_VERSION_ARG}

# Copy go mod and sum files
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/download cmd/download
# Production preparation can preload the exact DuckDB extensions from the
# current immutable app image. The download helper still validates them by
# installing/loading both extensions and remains the fallback for dev builds.
COPY packages/duckdb/ /root/.duckdb/
RUN go run cmd/download/duckdb/duckdb.go
COPY . .

# Get version and commit info for build injection
ARG VERSION_ARG
ARG COMMIT_ID_ARG
ARG BUILD_TIME_ARG
ARG GO_VERSION_ARG

# Set build-time variables
ENV VERSION=${VERSION_ARG}
ENV COMMIT_ID=${COMMIT_ID_ARG}
ENV BUILD_TIME=${BUILD_TIME_ARG}
ENV GO_VERSION=${GO_VERSION_ARG}

# Build the application with version info
RUN --mount=type=cache,target=/go/pkg/mod make build-prod
RUN --mount=type=cache,target=/go/pkg/mod cp -r /go/pkg/mod/github.com/yanyiwu/ /app/yanyiwu/

# Docker CLI extraction stage. The runtime only needs the client for the
# host-socket sandbox, not containerd/runc/the Docker daemon.
FROM ${BASE_IMAGE_REGISTRY_ARG}/debian:12.12-slim AS docker-cli

ARG APK_MIRROR_ARG
COPY packages/docker-cli/ /opt/preloaded-docker-cli/
RUN mkdir -p /opt/docker-cli/usr/bin && \
    if [ -x /opt/preloaded-docker-cli/docker ]; then \
        cp /opt/preloaded-docker-cli/docker /opt/docker-cli/usr/bin/docker; \
    else \
        apt-get -o Acquire::Retries=3 update && \
        apt-get -o Acquire::Retries=3 install -y --no-install-recommends ca-certificates && \
        if [ -n "$APK_MIRROR_ARG" ]; then \
            sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
        fi && \
        apt-get -o Acquire::Retries=3 update && \
        cd /tmp && \
        apt-get -o Acquire::Retries=3 download docker.io && \
        dpkg-deb -x docker.io_*.deb /opt/docker-cli && \
        rm -f docker.io_*.deb; \
    fi && \
    chmod 0755 /opt/docker-cli/usr/bin/docker && \
    /opt/docker-cli/usr/bin/docker --version && \
    rm -rf /opt/preloaded-docker-cli /var/lib/apt/lists/*

# Final stage
FROM ${BASE_IMAGE_REGISTRY_ARG}/debian:12.12-slim

WORKDIR /app

ARG APK_MIRROR_ARG
ARG PIP_INDEX_URL_ARG=https://mirrors.tencent.com/pypi/simple
ARG UV_VERSION_ARG=0.11.32
ARG PIP_VERSION_ARG=26.1.2
ARG SETUPTOOLS_VERSION_ARG=83.0.0
ARG WHEEL_VERSION_ARG=0.47.0
ARG PACKAGING_VERSION_ARG=26.2

COPY packages/uv/ /opt/python-build-tools/
COPY packages/python-build-tools/ /opt/python-build-tools/

# Create a non-root user first
RUN useradd -m -s /bin/bash appuser

# Seed the trust bundle from the approved internal Go base so the first APT
# request can use the HTTPS mirror without contacting deb.debian.org.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Then switch to mirror if specified and install other packages
RUN if [ -n "$APK_MIRROR_ARG" ]; then \
        sed -i "s@deb.debian.org@${APK_MIRROR_ARG}@g" /etc/apt/sources.list.d/debian.sources; \
    fi && \
    apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
        build-essential postgresql-client default-mysql-client tzdata sed curl bash vim wget \
        7zip libarchive-tools \
        libsqlite3-0 \
        python3 python3-pip python3-dev libffi-dev libssl-dev \
        nodejs npm \
        gosu \
        ffmpeg && \
    if [ "$(find /opt/python-build-tools -maxdepth 1 -type f -name '*.whl' | wc -l)" -ge 5 ]; then \
        python3 -m pip install --no-index --find-links /opt/python-build-tools \
            --break-system-packages --upgrade \
            "pip==$PIP_VERSION_ARG" \
            "setuptools==$SETUPTOOLS_VERSION_ARG" \
            "wheel==$WHEEL_VERSION_ARG" \
            "packaging==$PACKAGING_VERSION_ARG" \
            "uv==$UV_VERSION_ARG"; \
    else \
        PIP_INDEX_URL="$PIP_INDEX_URL_ARG" python3 -m pip install \
            --break-system-packages --upgrade \
            "pip==$PIP_VERSION_ARG" \
            "setuptools==$SETUPTOOLS_VERSION_ARG" \
            "wheel==$WHEEL_VERSION_ARG" \
            "packaging==$PACKAGING_VERSION_ARG" \
            "uv==$UV_VERSION_ARG"; \
    fi && \
    /usr/local/bin/uvx --version && \
    rm -rf /opt/python-build-tools && \
    mkdir -p /home/appuser/.local/bin && \
    chown -R appuser:appuser /home/appuser && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# Create data directories and set permissions
RUN mkdir -p /data/files && \
    chown -R appuser:appuser /app /data/files

# Copy migrate tool from builder stage
COPY --from=builder /go/bin/migrate /usr/local/bin/
COPY --chown=appuser:appuser --from=builder /app/yanyiwu/ /go/pkg/mod/github.com/yanyiwu/

# Copy the binary from the builder stage
COPY --chown=appuser:appuser --from=builder /app/config ./config
COPY --chown=appuser:appuser --from=builder /app/scripts ./scripts
COPY --chown=appuser:appuser --from=builder /app/migrations ./migrations
COPY --chown=appuser:appuser --from=builder /app/dataset/samples ./dataset/samples
COPY --chown=appuser:appuser --from=builder /app/skills/preloaded ./skills/preloaded
COPY --chown=appuser:appuser --from=builder /app/skills/professional ./skills/professional
# Keep a read-only backup so bind-mount cannot erase built-in skills
COPY --chown=appuser:appuser --from=builder /app/skills/preloaded ./skills/_builtin
COPY --chown=appuser:appuser --from=builder /root/.duckdb /home/appuser/.duckdb
COPY --chown=appuser:appuser --from=builder /app/WeKnora .
# Docker sandbox mode talks to the host daemon through docker.sock. Include
# only the distribution-packaged CLI binary; no daemon or runtime runs in app.
COPY --from=docker-cli /opt/docker-cli/usr/bin/docker /usr/local/bin/docker

# Copy and make entrypoint script executable
COPY --chown=appuser:appuser --from=builder /app/scripts/docker-entrypoint.sh ./scripts/docker-entrypoint.sh

# Make scripts executable
RUN chmod +x ./scripts/*.sh && \
    chown -R appuser:appuser /app/skills /data/files

# Expose ports
EXPOSE 8080


ENTRYPOINT ["./scripts/docker-entrypoint.sh"]
CMD ["./WeKnora"]
