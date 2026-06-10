// Command agent runs the device-side OTA agent: it connects to the OTA server,
// processes update commands, and reports container status.
package main

import (
	"log/slog"
	"os"

	"github.com/OpenMind/OM1-OTA/internal/agent"
	"github.com/OpenMind/OM1-OTA/internal/config"
	"github.com/OpenMind/OM1-OTA/internal/ota"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)

	serverURL := config.GetEnv("OTA_AGENT_SERVER_URL", "wss://api.openmind.com/api/core/ota/agent")
	dockerStatusURL := config.GetEnv("DOCKER_STATUS_URL", "https://api.openmind.com/api/core/ota/agent/docker")
	ecrCredentialsURL := config.GetEnv("ECR_CREDENTIALS_URL", "https://api.openmind.com/api/core/ota/ecr/credentials")
	configSyncURL := config.GetEnv("CONFIG_SYNC_URL", "https://api.openmind.com/api/core/ota/config/manifest")
	configDir := config.GetEnv("OM1_CONFIG_DIR", "/home/openmind/om1/om1/config")
	apiKey := os.Getenv("OM_API_KEY")
	apiKeyID := os.Getenv("OM_API_KEY_ID")

	if apiKey == "" || apiKeyID == "" {
		slog.Error("OM_API_KEY and OM_API_KEY_ID environment variables must be set")
		os.Exit(1)
	}

	base, err := ota.New(ota.Options{
		ServerURL:         serverURL,
		APIKey:            apiKey,
		APIKeyID:          apiKeyID,
		ECRCredentialsURL: ecrCredentialsURL,
	})
	if err != nil {
		slog.Error("Failed to initialize OTA agent", "error", err)
		os.Exit(1)
	}

	a := agent.New(base, dockerStatusURL, apiKey, configSyncURL, configDir)

	base.WS.RegisterMessageCallback(base.OTAProcess)
	base.WS.Start()
	a.Start()

	select {}
}
