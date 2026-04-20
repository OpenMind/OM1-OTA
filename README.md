# OTA

System setup tools for NVIDIA Orin devices with Over-The-Air (OTA) update capabilities.

## Overview

This project provides a comprehensive OTA (Over-The-Air) update system for NVIDIA Orin devices, enabling remote firmware updates, Docker container management, and system configuration updates. The system consists of two main components:

- **OTA Agent**: Manages device-side updates including Docker containers and system files
- **OTA Updater**: Handles self-updates of the OTA agent itself

## Features

- 🔄 **OTA Updates**: Secure over-the-air updates for firmware and software
- 🐳 **Docker Management**: Pull, update, and manage Docker containers remotely
- 📁 **File Management**: Download and deploy configuration files from S3
- 📊 **Progress Reporting**: Real-time update progress via WebSocket
- 🔐 **Secure Authentication**: API key-based authentication with OpenMind API
- 🏥 **Health Monitoring**: Container status tracking and reporting
- 🔄 **Self-Updating**: OTA agent can update itself automatically

## Architecture

### Components

```
OTA/
├── agent/          # OTA Agent - manages device updates
│   ├── main.py     # Agent entry point
│   └── README.md
├── updater/        # OTA Updater - manages agent self-updates
│   ├── main.py     # Updater entry point
│   └── README.md
├── ota/            # Core OTA functionality
│   ├── ota.py              # Base OTA class
│   ├── action_handlers.py  # Action handler implementations
│   ├── docker_operations.py# Docker management
│   ├── file_manager.py     # File operations
│   └── progress_reporter.py# Progress tracking
└── utils/          # Utility modules
    ├── s3_utils.py         # S3 file operations
    └── ws_client.py        # WebSocket client
```

### Managed Containers

The OTA Agent manages the following Docker containers:
- **om1**: Main container running the robot OS
- **om1_sensor**: Sensor processing container handling sensor data
- **orchestrator**: ROS2 Orchestrator service managing ROS2 nodes
- **watchdog**: ROS2 Watchdog service monitoring system health

## Installation

### Prerequisites

- Python 3.9 or higher
- [uv](https://docs.astral.sh/uv/) - Fast Python package installer and resolver
- Docker (for containerized deployment)
- NVIDIA Orin device (for production deployment)

Install `uv`:
```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

### Install from Source

1. Clone the repository:
```bash
git clone https://github.com/OpenMind/OM1-OTA.git
cd OM1-OTA
```

2. Create a virtual environment and install dependencies:
```bash
uv venv
source .venv/bin/activate  # On Windows: .venv\Scripts\activate
uv pip install -e .
```

### Install Development Dependencies

```bash
uv pip install -e ".[dev]"
```

### Quick Start with uv (Recommended)

Alternatively, use `uv run` to automatically manage the virtual environment:

```bash
uv run python -m OTA.agent.main
```

## Configuration

### Environment Variables

The following environment variables must be set:

#### OTA Agent
```bash
export OM_API_KEY="your-api-key"
export OM_API_KEY_ID="your-api-key-id"
export OTA_AGENT_SERVER_URL="wss://api.openmind.com/api/core/ota/agent"
export DOCKER_STATUS_URL="https://api.openmind.com/api/core/ota/agent/docker"
```

#### OTA Updater
```bash
export OM_API_KEY="your-api-key"
export OM_API_KEY_ID="your-api-key-id"
export OTA_UPDATER_SERVER_URL="wss://api.openmind.com/api/core/ota/updater"
```

## Usage

### Running the OTA Agent

```bash
uv run python -m OTA.agent.main
```

The agent will:
1. Connect to the OTA server via WebSocket
2. Listen for update commands
3. Execute updates (Docker pulls, file downloads, etc.)
4. Report progress back to the server

### Running the OTA Updater

```bash
uv run python -m OTA.updater.main
```

The updater handles self-updates of the OTA agent.

## Docker Deployment

### Build the Docker Image

```bash
docker build -t orin-ota-agent .
```

### Run with Docker Compose

```bash
docker-compose up -d
```

### Run with Docker

```bash
docker run -d \
  --name ota-agent \
  -e OM_API_KEY=your-api-key \
  -e OM_API_KEY_ID=your-api-key-id \
  -e OTA_AGENT_SERVER_URL=wss://api.openmind.com/api/core/ota/agent \
  -e DOCKER_STATUS_URL=https://api.openmind.com/api/core/ota/agent/docker \
  -v /var/run/docker.sock:/var/run/docker.sock \
  orin-ota-agent
```

## Development

### Code Formatting

This project uses `black`, `isort`, and `ruff` for code formatting:

```bash
# Format code
uv run black .
uv run isort .

# Lint code
uv run ruff check .
```

### Type Checking

```bash
# Using mypy
uv run mypy OTA/

# Using pyright
uv run pyright
```

### Running Tests

```bash
# Run all tests
uv run pytest

# Run with coverage
uv run pytest --cov=OTA tests/
```

### Pre-commit Hooks

Install pre-commit hooks:
```bash
uv run pre-commit install
```

Run manually:
```bash
uv run pre-commit run --all-files
```

## Project Structure

- `OTA/agent/`: OTA Agent implementation for device-side updates
- `OTA/updater/`: OTA Updater for agent self-updates
- `OTA/ota/`: Core OTA functionality and action handlers
- `OTA/utils/`: Utility modules (S3, WebSocket)
- `Dockerfile`: Container definition for deployment
- `docker-compose.yml`: Docker Compose configuration
- `pyproject.toml`: Project metadata and dependencies

## API Integration

The OTA system integrates with the OpenMind API for:
- **Authentication**: API key-based secure access
- **WebSocket Communication**: Real-time command and status updates
- **Container Status Reporting**: Health and state monitoring
- **Progress Tracking**: Update progress and completion status

## Security

- API keys must be kept secure and not committed to version control
- WebSocket connections use secure WSS protocol
- File downloads from S3 are verified before execution
- Docker operations run with appropriate permissions

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Support

For issues and questions, please contact the OpenMind team or create an issue in the repository.
