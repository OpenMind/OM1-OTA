// Command updater runs the OTA updater: it self-updates the OTA agent by
// processing update commands from the updater server.
package main

import (
	"log/slog"
	"os"

	"github.com/OpenMind/OM1-OTA/internal/config"
	"github.com/OpenMind/OM1-OTA/internal/ota"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelInfo)

	serverURL := config.GetEnv("OTA_UPDATER_SERVER_URL", "wss://api.openmind.com/api/core/ota/updater")
	ecrCredentialsURL := config.GetEnv("ECR_CREDENTIALS_URL", "https://api.openmind.com/api/core/ota/ecr/credentials")
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
		slog.Error("Failed to initialize OTA updater", "error", err)
		os.Exit(1)
	}

	base.WS.RegisterMessageCallback(base.OTAProcess)
	base.WS.Start()

	// Block forever; background goroutines do the work.
	select {}
}
