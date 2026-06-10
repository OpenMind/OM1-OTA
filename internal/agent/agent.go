// Package agent implements the device-side OTA agent: it manages container
// updates (via the embedded BaseOTA engine) and periodically reports container
// status to the server. It is a port of the Python AgentOTA.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/OpenMind/OM1-OTA/internal/configsync"
	"github.com/OpenMind/OM1-OTA/internal/ota"
)

const (
	infoFetchInterval    = 30 * time.Second
	statusReportInterval = 5 * time.Second
	configSyncInterval   = 5 * time.Minute
	configSyncTimeout    = 2 * time.Minute
	httpTimeout          = 10 * time.Second
	dockerCmdTimeout     = 30 * time.Second
)

var reSHA256 = regexp.MustCompile(`@sha256:([a-f0-9]{64})`)

// defaultContainerDescriptions is the built-in set of managed containers.
var defaultContainerDescriptions = map[string]string{
	"om1":                 "OM1 main container running the robot OS",
	"om1_sensor":          "OM1 sensor processing container handling sensor data",
	"orchestrator":        "ROS2 Orchestrator service container managing ROS2 nodes",
	"watchdog":            "ROS2 Watchdog service container monitoring system health",
	"zenoh_bridge":        "Zenoh Bridge service container serving the communication between OM1 and ROS2",
	"om1_avatar":          "OM1 Avatar container managing avatar functionalities",
	"om1_video_processor": "OM1 Video Processor container handling video streams",
	"ota_agent":           "OM1 OTA Agent container for over-the-air updates",
	"ota_updater":         "OM1 OTA Updater container for managing the OTA Agent updates",
	"qwen30b_quantized":   "Qwen 30B quantized model container for local AI processing",
	"kokoro_tts":          "Kokoro TTS container for text-to-speech functionalities",
	"riva_speech":         "NVIDIA Riva Speech container for advanced speech processing",
	"person_following":    "Person Following container for managing person following capabilities",
	"text_embedding":      "Text Embedding container for generating text embeddings for AI processing",
	"functiongemma":       "FunctionGemma container for executing function calls in the local environment",
	"om1_telemetry":       "OM1 Telemetry container for collecting telemetry data",
	"grafana":             "Grafana container for visualizing telemetry data",
	"prometheus":          "Prometheus container for data collection and monitoring",
}

// Agent wraps a BaseOTA with container status reporting.
type Agent struct {
	base      *ota.BaseOTA
	apiKey    string
	infoURL   string
	statusURL string

	mu           sync.RWMutex
	descriptions map[string]string

	httpClient *http.Client

	// configSyncer keeps the local config directory in sync with S3. nil when
	// config sync is not configured.
	configSyncer *configsync.Syncer
}

// containerStatus is the per-container record reported to the server.
type containerStatus struct {
	Description  string            `json:"description"`
	State        string            `json:"state"`
	Status       string            `json:"status"`
	Image        string            `json:"image"`
	ImageSHA256  string            `json:"image_sha256"`
	Ports        string            `json:"ports"`
	Created      string            `json:"created"`
	Command      string            `json:"command"`
	ID           string            `json:"id"`
	Present      bool              `json:"present"`
	EnvVariables map[string]string `json:"env_variables"`
}

// psLine is one entry from `docker ps -a --format json`.
type psLine struct {
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	Ports     string `json:"Ports"`
	CreatedAt string `json:"CreatedAt"`
	Command   string `json:"Command"`
	ID        string `json:"ID"`
}

// New creates an Agent that reports to dockerStatusURL using apiKey.
func New(base *ota.BaseOTA, dockerStatusURL, apiKey, configManifestURL, configDir string) *Agent {
	a := &Agent{
		base:         base,
		apiKey:       apiKey,
		infoURL:      dockerStatusURL + "/info",
		statusURL:    dockerStatusURL + "/status",
		descriptions: cloneMap(defaultContainerDescriptions),
		httpClient:   &http.Client{Timeout: httpTimeout},
	}
	if configManifestURL != "" && configDir != "" {
		a.configSyncer = configsync.New(configManifestURL, apiKey, configDir)
	}
	base.SetProcessCallback(a.reportStatusOnce)
	return a
}

// Base returns the embedded BaseOTA engine.
func (a *Agent) Base() *ota.BaseOTA {
	return a.base
}

// Start launches the background info-fetch and status-report loops.
func (a *Agent) Start() {
	go a.fetchInfoLoop()
	go a.reportStatusLoop()
	slog.Info("Started periodic Docker container info fetching", "interval", infoFetchInterval)
	slog.Info("Started periodic Docker container status reporting", "interval", statusReportInterval)
	if a.configSyncer != nil {
		go a.syncConfigLoop()
		slog.Info("Started periodic config sync", "interval", configSyncInterval)
	}
}

// syncConfigLoop syncs config files immediately and then on a fixed interval.
func (a *Agent) syncConfigLoop() {
	if !a.syncConfigOnce() {
		return
	}
	ticker := time.NewTicker(configSyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !a.syncConfigOnce() {
			return
		}
	}
}

// syncConfigOnce runs a single sync pass.
func (a *Agent) syncConfigOnce() bool {
	ctx, cancel := context.WithTimeout(context.Background(), configSyncTimeout)
	defer cancel()

	err := a.configSyncer.Sync(ctx)
	switch {
	case errors.Is(err, configsync.ErrForbidden):
		slog.Info("Config sync not enabled for this account; stopping config sync loop")
		return false
	case err != nil:
		slog.Error("Config sync failed", "error", err)
	}
	return true
}

// fetchInfoLoop periodically refreshes the managed-container descriptions.
//
// Note: the Python implementation only fetched once (a missing loop); this
// implements the clearly-intended periodic refresh described by its log message.
func (a *Agent) fetchInfoLoop() {
	a.fetchDockerInfo()
	ticker := time.NewTicker(infoFetchInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.fetchDockerInfo()
	}
}

func (a *Agent) fetchDockerInfo() {
	req, err := http.NewRequest(http.MethodGet, a.infoURL, nil)
	if err != nil {
		slog.Error("Failed to build Docker container info request", "error", err)
		return
	}
	req.Header.Set("x-api-key", a.apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		slog.Error("Failed to fetch Docker container info", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("Failed to fetch Docker container info", "status", resp.StatusCode)
		return
	}

	var payload struct {
		ContainerInfo map[string]string `json:"container_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		slog.Error("Failed to decode Docker container info", "error", err)
		return
	}
	if len(payload.ContainerInfo) > 0 {
		a.mu.Lock()
		a.descriptions = payload.ContainerInfo
		a.mu.Unlock()
	}
}

// reportStatusLoop reports container status immediately and then periodically.
func (a *Agent) reportStatusLoop() {
	a.reportStatus("periodic")
	ticker := time.NewTicker(statusReportInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.reportStatus("periodic")
	}
}

// reportStatusOnce is the post-OTA callback; the message argument is unused.
func (a *Agent) reportStatusOnce(_ []byte) {
	a.reportStatus("OTA callback")
}

func (a *Agent) reportStatus(context string) {
	status := a.readContainerStatus()
	if status == nil {
		return
	}
	a.sendStatusToServer(status, context)
}

// readContainerStatus reads local container state via the Docker CLI. Returns
// nil if the docker command itself fails.
func (a *Agent) readContainerStatus() map[string]containerStatus {
	out, err := runDocker(dockerCmdTimeout, "ps", "-a", "--format", "json")
	if err != nil {
		slog.Error("Failed to read local Docker container status", "error", err)
		return nil
	}

	descriptions := a.snapshotDescriptions()
	status := map[string]containerStatus{}
	found := map[string]bool{}

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var entry psLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			slog.Warn("Failed to parse container info", "error", err)
			continue
		}
		name := strings.TrimSpace(entry.Names)
		desc, ok := descriptions[name]
		if !ok {
			continue
		}
		found[name] = true

		status[name] = containerStatus{
			Description:  desc,
			State:        orUnknown(entry.State),
			Status:       orUnknown(entry.Status),
			Image:        orUnknown(entry.Image),
			ImageSHA256:  a.imageSHA256(entry.Image),
			Ports:        entry.Ports,
			Created:      entry.CreatedAt,
			Command:      entry.Command,
			ID:           entry.ID,
			Present:      true,
			EnvVariables: a.containerEnvVars(name, entry.Image),
		}
	}

	missing := 0
	for name, desc := range descriptions {
		if found[name] {
			continue
		}
		missing++
		status[name] = containerStatus{
			Description:  desc,
			State:        "missing",
			Status:       "missing",
			Image:        "unknown",
			ImageSHA256:  "unknown",
			Present:      false,
			EnvVariables: a.containerEnvVars(name, "latest"),
		}
		slog.Warn("Container is missing from local Docker", "container", name)
	}

	slog.Info("Fetched container status", "found", len(found), "missing", missing)
	return status
}

// imageSHA256 returns the content digest of an image, or "unknown".
func (a *Agent) imageSHA256(imageName string) string {
	out, err := runDocker(httpTimeout, "image", "inspect", imageName, "--format", "{{.RepoDigests}}")
	if err == nil {
		if m := reSHA256.FindStringSubmatch(strings.TrimSpace(out)); m != nil {
			return m[1]
		}
	}
	idOut, err := runDocker(httpTimeout, "image", "inspect", imageName, "--format", "{{.Id}}")
	if err == nil {
		id := strings.TrimSpace(idOut)
		if id != "" {
			return strings.TrimPrefix(id, "sha256:")
		}
	}
	if err != nil {
		slog.Warn("Failed to get SHA256 for image", "image", imageName, "error", err)
	}
	return "unknown"
}

// containerEnvVars merges schema-filtered live env (docker inspect) with the
// stored .env file. Live values take precedence.
func (a *Agent) containerEnvVars(containerName, imageName string) map[string]string {
	tag := imageTag(imageName)

	containerEnv := map[string]string{}
	out, err := runDocker(httpTimeout, "inspect", containerName, "--format", "{{json .Config.Env}}")
	if err == nil {
		var envList []string
		if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &envList); jsonErr == nil {
			for _, item := range envList {
				if k, v, ok := strings.Cut(item, "="); ok {
					containerEnv[k] = v
				}
			}
		} else {
			slog.Warn("Failed to parse env vars via docker inspect", "container", containerName, "error", jsonErr)
		}
	} else {
		slog.Warn("Failed to get env vars via docker inspect", "container", containerName, "error", err)
	}

	containerEnv = a.filterEnvBySchema(containerEnv, imageName)

	result := a.base.Files.ReadEnvFile(containerName, tag)
	for k, v := range containerEnv {
		result[k] = v
	}
	return result
}

// filterEnvBySchema keeps only env keys declared in the schema for imageName.
func (a *Agent) filterEnvBySchema(env map[string]string, imageName string) map[string]string {
	tag := imageTag(imageName)
	allowed := a.base.Downloader().GetSchemaEnvKeys(tag, imageName)
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		allowedSet[k] = struct{}{}
	}
	result := map[string]string{}
	for k, v := range env {
		if _, ok := allowedSet[k]; ok {
			result[k] = v
		}
	}
	return result
}

func (a *Agent) sendStatusToServer(status map[string]containerStatus, context string) {
	body, err := json.Marshal(map[string]any{"container_status": status})
	if err != nil {
		slog.Error("Failed to encode container status", "context", context, "error", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, a.statusURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("Failed to build container status request", "context", context, "error", err)
		return
	}
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		slog.Error("Failed to report Docker container status", "context", context, "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Error("Failed to report Docker container status", "context", context, "status", resp.StatusCode)
		return
	}
	slog.Info("Successfully reported Docker container status to server", "context", context)
}

func (a *Agent) snapshotDescriptions() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneMap(a.descriptions)
}

// --- helpers ---

// runDocker runs a docker subcommand with a timeout and returns combined stdout.
func runDocker(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	return string(out), err
}

func imageTag(imageName string) string {
	if idx := strings.LastIndex(imageName, ":"); idx >= 0 {
		return imageName[idx+1:]
	}
	return "latest"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
