package ota

import (
	"fmt"

	"go.uber.org/zap"

	"github.com/OpenMind/OM1-OTA/internal/s3"
)

// ActionHandlers implements the OTA action types (upgrade/start/stop/...).
type ActionHandlers struct {
	docker     *DockerManager
	progress   *ProgressReporter
	files      *FileManager
	ecr        *ECRHandler
	downloader *s3.Downloader
}

// NewActionHandlers wires the handlers with their collaborators.
func NewActionHandlers(docker *DockerManager, progress *ProgressReporter, files *FileManager, ecr *ECRHandler, downloader *s3.Downloader) *ActionHandlers {
	return &ActionHandlers{
		docker:     docker,
		progress:   progress,
		files:      files,
		ecr:        ecr,
		downloader: downloader,
	}
}

// HandleUpgrade downloads, verifies and applies an upgrade described by data.
func (a *ActionHandlers) HandleUpgrade(data map[string]any, serviceName string) {
	tag := getString(data, "tag")
	s3URL := getString(data, "s3_url")
	checksum := getString(data, "checksum")

	if tag == "" || s3URL == "" || checksum == "" {
		zap.S().Errorw("Invalid upgrade message: missing required fields (tag, s3_url, checksum)")
		a.progress.SendProgressUpdate("error", "Missing required fields for upgrade action", 0)
		return
	}

	zap.S().Infow("OTA upgrade details", "tag", tag, "s3_url", s3URL, "checksum", checksum, "service", serviceName)

	yamlContent, localPath, err := a.downloader.DownloadAndVerifyYAML(s3URL, checksum, "sha256")
	if err != nil {
		zap.S().Errorw("Failed to download or verify YAML file from S3", "error", err)
		a.progress.SendProgressUpdate("download_error", "Failed to download or verify YAML file from S3", 0)
		return
	}

	zap.S().Infow("Successfully downloaded and verified YAML file", "path", localPath)

	if err := a.downloader.DownloadSchema(tag); err != nil {
		zap.S().Warnw("Failed to download schema", "error", err)
	}

	env := a.resolveEnv(data, serviceName, tag)
	if err := a.files.UpdateEnvFile(serviceName, tag, env); err != nil {
		zap.S().Warnw("Failed to update env file", "error", err)
	}

	a.applyOTAUpdate(serviceName, yamlContent, localPath, tag)

	a.files.CleanupTempFile(localPath)
}

// HandleStop stops and removes a single service's container.
func (a *ActionHandlers) HandleStop(data map[string]any, serviceName string) {
	zap.S().Infow("Stopping service", "service", serviceName)
	a.progress.SendProgressUpdate("stopping", "Stopping service "+serviceName, 10)

	cn := getString(data, "container_name")
	if cn == "" {
		cn = serviceName
	}
	servicesConfig := map[string]any{
		"services": map[string]any{
			serviceName: map[string]any{"container_name": cn},
		},
	}

	if err := a.docker.StopDockerServices(servicesConfig); err != nil {
		msg := fmt.Sprintf("Failed to stop service %s: %v", serviceName, err)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, 10)
		return
	}
	zap.S().Infow("Successfully stopped service", "service", serviceName)
	a.progress.SendProgressUpdate("completed", "Successfully stopped service "+serviceName, 100)
}

// HandleStart starts a service from provided or stored config.
func (a *ActionHandlers) HandleStart(data map[string]any, serviceName string) {
	zap.S().Infow("Starting service", "service", serviceName)
	a.progress.SendProgressUpdate("starting", "Starting service "+serviceName, 10)

	yamlContent, ok := a.resolveYAML(data, serviceName)
	if !ok {
		return
	}

	tag := extractTagFromYAML(yamlContent)
	if err := a.downloader.DownloadSchema(tag); err != nil {
		zap.S().Warnw("Failed to download schema", "error", err)
	}
	env := a.resolveEnv(data, serviceName, tag)
	if err := a.files.UpdateEnvFile(serviceName, tag, env); err != nil {
		zap.S().Warnw("Failed to update env file", "error", err)
	}

	if ecrImage := a.ecr.CheckImagePrivacy(yamlContent); ecrImage != "" {
		if !a.ecr.LoginWithCredentials(ecrImage) {
			return
		}
	}

	if err := a.docker.StartDockerServices(yamlContent); err != nil {
		msg := fmt.Sprintf("Failed to start service %s: %v", serviceName, err)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, 80)
		return
	}
	zap.S().Infow("Successfully started service", "service", serviceName)
	a.progress.SendProgressUpdate("completed", "Successfully started service "+serviceName, 100)
}

// HandlePause pauses a service.
func (a *ActionHandlers) HandlePause(data map[string]any, serviceName string) {
	a.simpleAction(data, serviceName, "pausing", "Pausing", "paused", 10, a.docker.PauseDockerServices)
}

// HandleUnpause unpauses a service.
func (a *ActionHandlers) HandleUnpause(data map[string]any, serviceName string) {
	a.simpleAction(data, serviceName, "unpausing", "Unpausing", "unpaused", 10, a.docker.UnpauseDockerServices)
}

// HandleRestart restarts a service.
func (a *ActionHandlers) HandleRestart(data map[string]any, serviceName string) {
	a.simpleAction(data, serviceName, "restarting", "Restarting", "restarted", 50, a.docker.RestartDockerServices)
}

// simpleAction is the shared flow for pause/unpause/restart.
func (a *ActionHandlers) simpleAction(
	data map[string]any,
	serviceName, statusVerb, gerund, pastTense string,
	errProgress int,
	op func(map[string]any) error,
) {
	zap.S().Infow(gerund+" service", "service", serviceName)
	a.progress.SendProgressUpdate(statusVerb, fmt.Sprintf("%s service %s", gerund, serviceName), 10)

	yamlContent, ok := a.resolveYAML(data, serviceName)
	if !ok {
		return
	}

	if err := op(yamlContent); err != nil {
		msg := fmt.Sprintf("%s service %s failed: %v", gerund, serviceName, err)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, errProgress)
		return
	}
	zap.S().Infow(fmt.Sprintf("Successfully %s service", pastTense), "service", serviceName)
	a.progress.SendProgressUpdate("completed", fmt.Sprintf("Successfully %s service %s", pastTense, serviceName), 100)
}

// applyOTAUpdate performs the stop -> ecr login -> store -> start sequence.
func (a *ActionHandlers) applyOTAUpdate(serviceName string, yamlContent map[string]any, tempYAMLPath, tag string) bool {
	zap.S().Infow("Applying OTA update", "tag", tag)
	a.progress.SendProgressUpdate("starting", "Starting OTA update "+tag, 0)

	zap.S().Infow("Stopping current Docker services...")
	a.progress.SendProgressUpdate("stopping", "Stopping current Docker services", 10)
	if err := a.docker.StopDockerServices(yamlContent); err != nil {
		msg := fmt.Sprintf("Failed to stop Docker services: %v", err)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, 10)
		return false
	}

	if ecrImage := a.ecr.CheckImagePrivacy(yamlContent); ecrImage != "" {
		if !a.ecr.LoginWithCredentials(ecrImage) {
			return false
		}
	}

	a.progress.SendProgressUpdate("storing", "Storing update files", 20)
	if err := a.files.StoreUpdateFiles(serviceName, tag, tempYAMLPath); err != nil {
		zap.S().Errorw(err.Error())
		a.progress.SendProgressUpdate("error", err.Error(), 20)
		return false
	}

	zap.S().Infow("Starting updated Docker services...")
	if err := a.docker.StartDockerServices(yamlContent); err != nil {
		msg := fmt.Sprintf("Failed to start Docker services: %v", err)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, 80)
		return false
	}

	zap.S().Infow("Successfully applied OTA update", "tag", tag)
	a.progress.SendProgressUpdate("completed", "Successfully applied OTA update "+tag, 100)
	return true
}

// resolveYAML returns the compose config from data["yaml_content"], or falls
// back to the stored latest config.
func (a *ActionHandlers) resolveYAML(data map[string]any, serviceName string) (map[string]any, bool) {
	if m, ok := getMap(data, "yaml_content"); ok && len(m) > 0 {
		return m, true
	}

	content, path, err := a.files.LoadLatestConfig(serviceName)
	if err != nil {
		msg := fmt.Sprintf("No YAML content provided and no stored configuration found for service %s", serviceName)
		zap.S().Errorw(msg)
		a.progress.SendProgressUpdate("error", msg, 10)
		return nil, false
	}
	zap.S().Infow("Loaded latest configuration", "path", path)
	return content, true
}

// resolveEnv returns env from data["env_variables"] when present, otherwise the
// schema defaults.
func (a *ActionHandlers) resolveEnv(data map[string]any, serviceName, tag string) map[string]string {
	if v, ok := data["env_variables"]; ok && v != nil {
		if m, ok := v.(map[string]any); ok {
			return stringifyMap(m)
		}
	}
	return a.downloader.GetDefaultEnv(serviceName, tag)
}

// extractTagFromYAML returns the tag from the first service's image, or "latest".
func extractTagFromYAML(compose map[string]any) string {
	for _, cfg := range servicesMap(compose) {
		m, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		image, _ := m["image"].(string)
		if idx := lastColon(image); idx >= 0 {
			return image[idx+1:]
		}
		return "latest"
	}
	return "latest"
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

func getString(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func getMap(data map[string]any, key string) (map[string]any, bool) {
	if v, ok := data[key].(map[string]any); ok {
		return v, true
	}
	return nil, false
}

func stringifyMap(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
