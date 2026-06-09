package ota

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileManager persists OTA state (compose YAML + env files) under .ota.
type FileManager struct {
	updatesDir string
}

// NewFileManager creates a FileManager rooted at updatesDir (default ".ota").
func NewFileManager(updatesDir string) (*FileManager, error) {
	if updatesDir == "" {
		updatesDir = ".ota"
	}
	abs, err := filepath.Abs(updatesDir)
	if err != nil {
		return nil, fmt.Errorf("resolve updates dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create updates dir: %w", err)
	}
	return &FileManager{updatesDir: abs}, nil
}

// StoreUpdateFiles copies the temp YAML to both the version-tagged and the
// "latest" stored paths.
func (m *FileManager) StoreUpdateFiles(serviceName, tag, tempYAMLPath string) error {
	versionPath := filepath.Join(m.updatesDir, fmt.Sprintf("%s_%s.yaml", serviceName, tag))
	latestPath := filepath.Join(m.updatesDir, fmt.Sprintf("%s_latest.yaml", serviceName))

	data, err := os.ReadFile(tempYAMLPath)
	if err != nil {
		err = fmt.Errorf("failed to store OTA update file: %w", err)
		slog.Error(err.Error())
		return err
	}
	for _, dst := range []string{versionPath, latestPath} {
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			err = fmt.Errorf("failed to store OTA update file: %w", err)
			slog.Error(err.Error())
			return err
		}
	}
	slog.Info("Stored OTA update files", "version", versionPath, "latest", latestPath)
	return nil
}

// LoadLatestConfig loads the most recently stored compose config for a service.
func (m *FileManager) LoadLatestConfig(serviceName string) (map[string]any, string, error) {
	latestPath := filepath.Join(m.updatesDir, fmt.Sprintf("%s_latest.yaml", serviceName))

	data, err := os.ReadFile(latestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("no stored configuration found for service %s", serviceName)
		}
		err = fmt.Errorf("failed to load latest configuration: %w", err)
		slog.Error(err.Error())
		return nil, "", err
	}

	var content map[string]any
	if err := yaml.Unmarshal(data, &content); err != nil {
		err = fmt.Errorf("failed to load latest configuration: %w", err)
		slog.Error(err.Error())
		return nil, "", err
	}
	slog.Info("Loaded latest configuration", "path", latestPath)
	return content, latestPath, nil
}

// CleanupTempFile removes a temporary file, returning true on success (or if the
// file does not exist).
func (m *FileManager) CleanupTempFile(path string) bool {
	if path == "" {
		return true
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return true
		}
		slog.Warn("Failed to clean up file", "path", path, "error", err)
		return false
	}
	slog.Info("Cleaned up temporary file", "path", path)
	return true
}

// UpdateEnvFile writes the given variables to {service}_{tag}.env as KEY=VALUE
// lines.
func (m *FileManager) UpdateEnvFile(serviceName, tag string, variables map[string]string) error {
	envPath := filepath.Join(m.updatesDir, fmt.Sprintf("%s_%s.env", serviceName, tag))

	var b strings.Builder
	for k, v := range variables {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	if err := os.WriteFile(envPath, []byte(b.String()), 0o644); err != nil {
		err = fmt.Errorf("failed to write env file %s: %w", envPath, err)
		slog.Error(err.Error())
		return err
	}
	slog.Info("Wrote env file", "service", serviceName, "tag", tag, "path", envPath)
	return nil
}

// ReadEnvFile reads env vars from {service}_{tag}.env.
func (m *FileManager) ReadEnvFile(serviceName, tag string) map[string]string {
	envPath := filepath.Join(m.updatesDir, fmt.Sprintf("%s_%s.env", serviceName, tag))
	return parseEnvFile(envPath)
}

// parseEnvFile parses KEY=VALUE lines from an env file, returning an empty map
// if the file is missing or unreadable.
func parseEnvFile(path string) map[string]string {
	result := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return result
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		result[key] = value
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("Failed to parse env file", "path", path, "error", err)
	}
	return result
}
