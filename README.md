# OTA

System setup tools for NVIDIA Orin devices with Over-The-Air (OTA) update capabilities, written in Go.

## Overview

This project provides an OTA update system for NVIDIA Orin devices, enabling remote Docker container management and system configuration updates. It consists of two Docker-based binaries that hold a persistent WebSocket connection to the OpenMind API and execute remote commands against the local Docker daemon, plus one optional host-native binary:

- **agent** – manages the device's containers and continuously reports their status to the cloud.
- **updater** – self-updates the OTA agent itself (same engine, different WebSocket endpoint, no status reporting).
- **terminal** – *(optional, host-native via systemd, not Docker)* gives a real remote shell on the device itself rather than one scoped to a container's namespaces. Fully independent of agent/updater; see [Terminal (host-native, not Docker)](#terminal-host-native-not-docker).

Docker operations are performed by shelling out to the `docker` and `docker-compose` CLIs

## Features

- 🔄 Secure over-the-air container updates
- 🐳 Pull, start, stop, pause, unpause, and restart Docker services remotely
- 📁 Download and verify compose configs (SHA256) from S3 / HTTPS
- 📊 Real-time progress reporting via WebSocket
- 🔐 API-key authentication and short-lived private ECR credentials
- 🏥 Periodic container status and image-digest reporting
- 🔄 Self-updating agent
- 🖥️ Optional host-native remote terminal, self-updating, independent of the Docker-based agent/updater

### Managed containers

The agent reports status for a built-in set of containers (om1, om1_sensor, orchestrator, watchdog, zenoh_bridge, grafana, prometheus, and others). The set can be refreshed at runtime from the server's `/info` endpoint.

## Build

Requires Go 1.25+.

```bash
# Build all three binaries into ./bin
go build -o bin/agent ./cmd/agent
go build -o bin/updater ./cmd/updater
go build -o bin/terminal ./cmd/terminal

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

### Terminal (host-native, not Docker)

`cmd/terminal` is a separate, minimal binary that provides remote terminal access. It's built to run **directly on the host via systemd**, not inside a container — `ota_agent`/`ota_updater` run in Docker, so a shell spawned from inside them would only ever see the container's own namespaces, not the real host. This binary is deliberately independent of `ota_agent`/`ota_updater` and requires no changes to either. It holds a persistent connection to the cloud API's terminal-agent WebSocket (its own dedicated channel — nothing to do with the OTA agent/updater WebSockets) and is notified directly the moment the portal creates a session for it.

> **Security note:** this binary runs as root and gives an unrestricted host shell to anyone who can open a terminal session for this device through the portal. Treat `OM_API_KEY` on a device running this service as equivalent to a root credential.

```bash
export OM_API_KEY="your-api-key"
export OM_API_KEY_ID="your-api-key-id"
# optional:
export TERMINAL_AGENT_URL="wss://api.openmind.com/api/core/v1/terminal/agent"
export TERMINAL_WS_URL="wss://api.openmind.com/api/core/v1/terminal/ws"
export TERMINAL_SHELL="/bin/bash"
export TERMINAL_UPDATE_MANIFEST_URL="https://assets.openmind.com/ota/terminal/schema.json"
export TERMINAL_UPDATES_DIR="/var/lib/om1-terminal/updates"
```

It also self-updates: every 10 minutes (and once at startup) it checks `TERMINAL_UPDATE_MANIFEST_URL` for a build newer than the one it was compiled with, and if found, downloads, verifies, and atomically replaces its own binary, then exits so systemd (`Restart=always`) relaunches it. Install with the provided [`deploy/om1-terminal.service`](deploy/om1-terminal.service) unit file.

The terminal binary can be built and run directly from source, but in production it should be downloaded from the OTA server and installed as a systemd service.

Download the `terminal` binary:

```bash
sudo mkdir -p /opt/om1/bin
sudo curl -sL -o /opt/om1/bin/terminal https://assets.openmind.com/ota/terminal/1787172974/terminal-linux-arm64
sudo chmod +x /opt/om1/bin/terminal
```

Create the environment file at `/etc/om1/terminal.env`:

```bash
sudo mkdir -p /etc/om1
sudo tee /etc/om1/terminal.env > /dev/null <<'EOF'
OM_API_KEY=your_api_key_here
OM_API_KEY_ID=your_api_key_id_here
EOF
```

Create the service file at `/etc/systemd/system/om1-terminal.service`:

```bash
sudo tee /etc/systemd/system/om1-terminal.service > /dev/null <<'EOF'
[Unit]
Description=OM1 Remote Terminal Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/om1/terminal.env
ExecStart=/opt/om1/bin/terminal
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
EOF
```

Then enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable om1-terminal.service
sudo systemctl start om1-terminal.service
```

You can check its status and logs with:

```bash
sudo systemctl status om1-terminal.service
sudo journalctl -u om1-terminal.service -f
```

## Usage

```bash
# Run the agent
./bin/agent

# Run the updater
./bin/updater

# Run the terminal agent (host-native — install via systemd instead in production, see deploy/om1-terminal.service)
./bin/terminal
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

The compose file mounts the host Docker socket and the host `docker` / `docker-compose` binaries into the containers so the Go binaries can drive the host's Docker daemon.

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

This project is licensed under the MIT License – see the [LICENSE](LICENSE) file for details.
