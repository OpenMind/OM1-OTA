# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.25-bookworm AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both static binaries.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/agent   ./cmd/agent && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/updater ./cmd/updater

# --- runtime stage ---
# The agent shells out to the docker / docker-compose CLIs (mounted from the
# host via docker-compose), so we only need a minimal base with CA certs.
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/agent   /usr/local/bin/agent
COPY --from=build /out/updater /usr/local/bin/updater

# Default to the agent; docker-compose overrides the command for the updater.
CMD ["agent"]
