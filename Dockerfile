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
COPY third_party/go-smb ./third_party/go-smb
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked go mod download
COPY . .
COPY --from=web-build /src/web/dist ./web/dist
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torrentfs ./cmd/torrentfs

FROM debian:bookworm-slim
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bash ca-certificates fuse3 passwd tini util-linux; \
    groupadd --gid 1000 torrentfs; \
    useradd --uid 1000 --gid 1000 --no-create-home --shell /usr/sbin/nologin torrentfs; \
    install -d -o root -g root -m 0755 /etc/torrentfs; \
    install -d -o torrentfs -g torrentfs -m 0755 /share /share-strm; \
    install -d -o root -g root -m 0777 /torrents; \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/torrentfs /usr/local/bin/torrentfs
COPY docker/entrypoint.sh /usr/local/bin/torrentfs-entrypoint
COPY docker/torrentfs.toml /etc/torrentfs/torrentfs.toml
COPY LICENSE /usr/share/licenses/torrentfs-go/LICENSE
COPY third_party/go-smb/LICENSE /usr/share/licenses/torrentfs-go/go-smb.LICENSE
RUN chmod 0755 /usr/local/bin/torrentfs-entrypoint

USER 1000:1000
WORKDIR /
# Publish host TCP 445 to container TCP 1445 for native SMB. FUSE is optional
# and only needs its device/capabilities when filesystem.backend=fuse.
EXPOSE 8080 1445/tcp 6881/tcp 6881/udp
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/torrentfs-entrypoint"]
CMD ["-config", "/etc/torrentfs/torrentfs.toml", "/torrents"]
