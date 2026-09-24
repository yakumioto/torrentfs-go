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
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bash ca-certificates fuse3 passwd samba samba-common-bin tini util-linux; \
    install -d -o root -g root -m 0755 /etc/torrentfs /share; \
    install -d -o root -g root -m 0777 /torrents; \
    install -d -o root -g root -m 0755 \
        /run/samba /run/samba/lock /run/samba/state /run/samba/cache \
        /var/log/samba /var/cache/samba /var/lib/samba; \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /out/torrentfs /usr/local/bin/torrentfs
COPY docker/entrypoint.sh /usr/local/bin/torrentfs-entrypoint
COPY docker/smb.conf /etc/samba/torrentfs-smb.conf
COPY docker/torrentfs.toml /etc/torrentfs/torrentfs.toml
RUN chmod 0755 /usr/local/bin/torrentfs-entrypoint

WORKDIR /
# 8080 serves the HTTP API and Web UI. 6881 is the fixed peer port from
# docker/torrentfs.toml; publish it for both TCP and UDP to accept inbound
# peers: -p 6881:6881/tcp -p 6881:6881/udp. SMB uses only direct-hosted TCP 445.
# EXPOSE alone publishes nothing.
EXPOSE 8080 445/tcp 6881/tcp 6881/udp
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/torrentfs-entrypoint"]
CMD ["-config", "/etc/torrentfs/torrentfs.toml", "/torrents"]
