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
RUN apt-get update \
    && apt-get install -y --no-install-recommends fuse3 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/torrentfs /usr/local/bin/torrentfs
RUN mkdir -p /etc/torrentfs /data /torrents
COPY docker/torrentfs.toml /etc/torrentfs/torrentfs.toml

WORKDIR /
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/torrentfs"]
CMD ["-config", "/etc/torrentfs/torrentfs.toml", "/torrents"]
