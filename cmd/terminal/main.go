package main

import (
	"encoding/json"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/OpenMind/OM1-OTA/internal/config"
	"github.com/OpenMind/OM1-OTA/internal/logging"
	"github.com/OpenMind/OM1-OTA/internal/s3"
	"github.com/OpenMind/OM1-OTA/internal/selfupdate"
	"github.com/OpenMind/OM1-OTA/internal/terminal"
	"github.com/OpenMind/OM1-OTA/internal/version"
	"github.com/OpenMind/OM1-OTA/internal/ws"
)

const updateCheckInterval = 10 * time.Minute

func main() {
	logger := logging.Init()
	defer func() { _ = logger.Sync() }()

	triggerServerURL := config.GetEnv("TERMINAL_TRIGGER_SERVER_URL", "wss://api.openmind.com/api/core/ota/updater")
	terminalWSURL := config.GetEnv("TERMINAL_WS_URL", "wss://api.openmind.com/api/core/v1/terminal/ws")
	terminalShell := config.GetEnv("TERMINAL_SHELL", "/bin/bash")
	manifestURL := config.GetEnv("TERMINAL_UPDATE_MANIFEST_URL", "https://assets.openmind.com/ota/terminal/schema.json")
	updatesDir := config.GetEnv("TERMINAL_UPDATES_DIR", "/var/lib/om1-terminal/updates")
	apiKey := os.Getenv("OM_API_KEY")
	apiKeyID := os.Getenv("OM_API_KEY_ID")

	if apiKey == "" || apiKeyID == "" {
		zap.S().Errorw("OM_API_KEY and OM_API_KEY_ID environment variables must be set")
		os.Exit(1)
	}

	terminalMgr := terminal.NewManager(terminalWSURL, apiKey, terminalShell)

	wsURL := triggerServerURL + "?api_key_id=" + apiKeyID + "&api_key=" + apiKey
	client := ws.NewClient(wsURL)
	client.RegisterMessageCallback(func(message []byte) {
		var data map[string]any
		if err := json.Unmarshal(message, &data); err != nil {
			return
		}
		if action, _ := data["action"].(string); action == "terminal_start" {
			terminalMgr.HandleStart(data)
		}
	})
	client.Start()

	downloader, err := s3.NewDownloader(updatesDir)
	if err != nil {
		zap.S().Errorw("Failed to initialize update downloader; self-update disabled", "error", err)
	} else {
		updater := selfupdate.NewUpdater(manifestURL, version.BuildTimestamp, downloader)
		go runUpdateLoop(updater)
	}

	select {}
}

func runUpdateLoop(updater *selfupdate.Updater) {
	checkOnce(updater)
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		checkOnce(updater)
	}
}

func checkOnce(updater *selfupdate.Updater) {
	if err := updater.CheckAndApply(); err != nil {
		zap.S().Warnw("Self-update check failed", "error", err)
	}
}
