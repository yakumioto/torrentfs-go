# syntax=docker/dockerfile:1

FROM node:22.23.2-bookworm-slim AS web-build
WORKDIR /src
COPY web/package.json web/package-lock.json ./web/
RUN npm ci --prefix web
COPY web ./web
RUN npm run build --prefix web

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torrentfs ./cmd/torrentfs

FROM debian:bookworm-slim
ARG TORRENTFS_UID=1000
ARG TORRENTFS_GID=1000
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends fuse3 ca-certificates passwd; \
    runtime_group="$(getent group "$TORRENTFS_GID" | cut -d: -f1 || true)"; \
    if [ -z "$runtime_group" ]; then \
        runtime_group=torrentfs; \
        while getent group "$runtime_group" >/dev/null 2>&1; do \
            runtime_group="${runtime_group}_"; \
        done; \
        groupadd --gid "$TORRENTFS_GID" "$runtime_group"; \
    fi; \
    if ! getent passwd "$TORRENTFS_UID" >/dev/null 2>&1; then \
        runtime_user=torrentfs; \
        while getent passwd "$runtime_user" >/dev/null 2>&1; do \
            runtime_user="${runtime_user}_"; \
        done; \
        useradd --uid "$TORRENTFS_UID" --gid "$runtime_group" --no-create-home --shell /usr/sbin/nologin "$runtime_user"; \
    fi; \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/torrentfs /usr/local/bin/torrentfs
RUN mkdir -p /etc/torrentfs /torrents
COPY docker/torrentfs.toml /etc/torrentfs/torrentfs.toml

WORKDIR /
# 8080 serves the HTTP API and Web UI. 6881 is the fixed peer port from
# docker/torrentfs.toml; publish it for both TCP and UDP to accept inbound
# peers: -p 6881:6881/tcp -p 6881:6881/udp. EXPOSE alone publishes nothing.
EXPOSE 8080 6881/tcp 6881/udp
ENTRYPOINT ["/usr/local/bin/torrentfs"]
CMD ["-config", "/etc/torrentfs/torrentfs.toml", "/torrents"]
