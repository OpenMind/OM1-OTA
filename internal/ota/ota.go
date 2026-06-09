// Package ota implements the core OTA engine: it dispatches WebSocket commands
// to Docker operations and reports progress. It is a port of the Python BaseOTA
// and its collaborators.
package ota

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/OpenMind/OM1-OTA/internal/s3"
	"github.com/OpenMind/OM1-OTA/internal/ws"
)

// Options configures a BaseOTA instance.
type Options struct {
	ServerURL         string
	APIKey            string
	APIKeyID          string
	ECRCredentialsURL string
	UpdatesDir        string // defaults to ".ota"
}

// BaseOTA wires together the WebSocket client, progress reporter, file manager,
// Docker manager, and action handlers.
type BaseOTA struct {
	opts Options

	Progress *ProgressReporter
	Files    *FileManager
	Docker   *DockerManager
	Actions  *ActionHandlers
	WS       *ws.Client

	downloader      *s3.Downloader
	processCallback func(message []byte)
}

// New constructs a BaseOTA from opts.
func New(opts Options) (*BaseOTA, error) {
	if opts.ServerURL == "" || opts.APIKey == "" || opts.APIKeyID == "" {
		return nil, fmt.Errorf("OTA server URL and API keys must be provided")
	}

	downloader, err := s3.NewDownloader(opts.UpdatesDir)
	if err != nil {
		return nil, err
	}
	files, err := NewFileManager(opts.UpdatesDir)
	if err != nil {
		return nil, err
	}

	progress := NewProgressReporter(nil)
	docker := NewDockerManager(progress)
	ecr := NewECRHandler(docker, progress, opts.ECRCredentialsURL, opts.APIKey)
	actions := NewActionHandlers(docker, progress, files, ecr, downloader)

	wsClient := ws.NewClient(buildWSURL(opts))
	progress.SetWSClient(wsClient)

	return &BaseOTA{
		opts:       opts,
		Progress:   progress,
		Files:      files,
		Docker:     docker,
		Actions:    actions,
		WS:         wsClient,
		downloader: downloader,
	}, nil
}

// buildWSURL appends the API credentials as query parameters, matching the
// Python client's URL format.
func buildWSURL(opts Options) string {
	return fmt.Sprintf("%s?api_key_id=%s&api_key=%s", opts.ServerURL, opts.APIKeyID, opts.APIKey)
}

// SetProcessCallback registers a callback invoked after each processed message.
func (o *BaseOTA) SetProcessCallback(cb func(message []byte)) {
	o.processCallback = cb
}

// Downloader exposes the S3 downloader (used by the agent for schema lookups).
func (o *BaseOTA) Downloader() *s3.Downloader {
	return o.downloader
}

// OTAProcess parses and dispatches a single inbound WebSocket message.
func (o *BaseOTA) OTAProcess(message []byte) {
	slog.Info("Received OTA message", "message", string(message))

	var data map[string]any
	if err := json.Unmarshal(message, &data); err != nil {
		slog.Error("Failed to decode JSON message", "error", err)
		o.Progress.SendProgressUpdate("decode_error", "Failed to decode message", 0)
		o.runCallback(message)
		return
	}

	action := getString(data, "action")
	serviceName := getString(data, "service_name")

	if action == "" {
		slog.Error("Invalid OTA message: missing action")
		return
	}
	if serviceName == "" {
		slog.Error("Invalid OTA message: missing service_name")
		return
	}

	slog.Info("Processing action", "action", action, "service", serviceName)

	switch action {
	case "upgrade":
		o.Actions.HandleUpgrade(data, serviceName)
	case "stop":
		o.Actions.HandleStop(data, serviceName)
	case "start":
		o.Actions.HandleStart(data, serviceName)
	case "pause":
		o.Actions.HandlePause(data, serviceName)
	case "unpause":
		o.Actions.HandleUnpause(data, serviceName)
	case "restart":
		o.Actions.HandleRestart(data, serviceName)
	default:
		msg := fmt.Sprintf("Unknown action type: %s. Supported actions: upgrade, stop, start, pause, unpause, restart", action)
		slog.Error(msg)
		o.Progress.SendProgressUpdate("error", msg, 0)
	}

	o.runCallback(message)
}

func (o *BaseOTA) runCallback(message []byte) {
	if o.processCallback != nil {
		o.processCallback(message)
	}
}
