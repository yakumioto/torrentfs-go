# syntax=docker/dockerfile:1

# Build stage: compile a static torrentfs binary.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/torrentfs ./cmd/torrentfs

# Runtime stage. This image runs rootful because mounting FUSE requires the
# host to grant /dev/fuse and the matching capability; the image itself cannot
# obtain those, it only carries the tooling that needs them. See the README for
# the required `docker run` flags.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends fuse3 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/torrentfs /usr/local/bin/torrentfs

# Data and mountpoint are supplied at run time; declare them as the working
# conventions for `docker run -v` overrides.
WORKDIR /
ENTRYPOINT ["/usr/local/bin/torrentfs"]
CMD ["-h"]
