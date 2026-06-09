# OTA

System setup tools for NVIDIA Orin devices with Over-The-Air (OTA) update
capabilities, written in Go.

## Overview

This project provides an OTA update system for NVIDIA Orin devices, enabling
remote Docker container management and system configuration updates. It consists
of two binaries that hold a persistent WebSocket connection to the OpenMind API
and execute remote commands against the local Docker daemon:

- **agent** – manages the device's containers and continuously reports their
  status to the cloud.
- **updater** – self-updates the OTA agent itself (same engine, different
  WebSocket endpoint, no status reporting).

Docker operations are performed by shelling out to the `docker` and
`docker-compose` CLIs, matching the original Python implementation.

## Features

- 🔄 Secure over-the-air container updates
- 🐳 Pull, start, stop, pause, unpause, and restart Docker services remotely
- 📁 Download and verify compose configs (SHA256) from S3 / HTTPS
- 📊 Real-time progress reporting via WebSocket
- 🔐 API-key authentication and short-lived private ECR credentials
- 🏥 Periodic container status and image-digest reporting
- 🔄 Self-updating agent

## Architecture

```
OM1-OTA/
├── cmd/
│   ├── agent/      # agent entry point (status reporting + update engine)
│   └── updater/    # updater entry point (update engine only)
├── internal/
│   ├── agent/      # AgentOTA: container status & info reporting
│   ├── config/     # environment helpers
│   ├── ota/        # core engine
│   │   ├── ota.go          # BaseOTA: WS message dispatcher + wiring
│   │   ├── actions.go      # action handlers (upgrade/start/stop/...)
│   │   ├── docker.go       # docker / docker-compose operations
│   │   ├── filemanager.go  # .ota state (compose yaml + env files)
│   │   ├── ecr.go          # private ECR auth
│   │   └── progress.go     # ota_progress WebSocket frames
│   ├── s3/         # artifact download, checksum verify, schema
│   └── ws/         # reconnecting WebSocket client
├── Dockerfile
├── docker-compose.yml
└── go.mod
```

### Managed containers

The agent reports status for a built-in set of containers (om1, om1_sensor,
orchestrator, watchdog, zenoh_bridge, grafana, prometheus, and others). The set
can be refreshed at runtime from the server's `/info` endpoint.

## Build

Requires Go 1.25+.

```bash
# Build both binaries into ./bin
go build -o bin/agent ./cmd/agent
go build -o bin/updater ./cmd/updater

# Or build everything
go build ./...
```

## Configuration

Configuration is provided via environment variables.

### Agent

```bash
export OM_API_KEY="your-api-key"
export OM_API_KEY_ID="your-api-key-id"
export OTA_AGENT_SERVER_URL="wss://api.openmind.com/api/core/ota/agent"
export DOCKER_STATUS_URL="https://api.openmind.com/api/core/ota/agent/docker"
# optional:
export ECR_CREDENTIALS_URL="https://api.openmind.com/api/core/ota/ecr/credentials"
```

### Updater

```bash
export OM_API_KEY="your-api-key"
export OM_API_KEY_ID="your-api-key-id"
export OTA_UPDATER_SERVER_URL="wss://api.openmind.com/api/core/ota/updater"
```

## Usage

```bash
# Run the agent
./bin/agent

# Run the updater
./bin/updater
```

The agent will:
1. Connect to the OTA server via WebSocket.
2. Listen for update commands.
3. Execute updates (compose pulls, container lifecycle, file downloads).
4. Report progress and container status back to the server.

## Docker Deployment

```bash
# Build the image
docker build -t orin-ota-agent .

# Run with Docker Compose (runs both agent and updater)
docker-compose up -d
```

The compose file mounts the host Docker socket and the host `docker` /
`docker-compose` binaries into the containers so the Go binaries can drive the
host's Docker daemon.

## Development

```bash
# Format
gofmt -w .

# Static analysis
go vet ./...

# Tests
go test ./...

# Keep modules tidy
go mod tidy
```

## License

This project is licensed under the MIT License – see the [LICENSE](LICENSE) file
for details.
