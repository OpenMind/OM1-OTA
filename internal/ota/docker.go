package ota

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"gopkg.in/yaml.v3"
)

// Regexes for parsing docker-compose pull output.
var (
	rePulling      = regexp.MustCompile(`^Pulling\s+(\w+)`)
	rePullComplete = regexp.MustCompile(`^([a-f0-9]+)\s+Pull complete`)
	reDownloading  = regexp.MustCompile(`^([a-f0-9]+)\s+Downloading.*?(\d+(?:\.\d+)?[KMGT]?B)/(\d+(?:\.\d+)?[KMGT]?B)`)
	reExtracting   = regexp.MustCompile(`^([a-f0-9]+)\s+Extracting`)
	reReclaimed    = regexp.MustCompile(`Total reclaimed space:\s*([\d.]+\w+)`)
)

// DockerManager runs docker / docker-compose CLI commands for OTA operations.
type DockerManager struct {
	progress *ProgressReporter

	mu              sync.Mutex
	completedLayers map[string]struct{}
}

type cmdResult struct {
	stdout   string
	stderr   string
	code     int
	timedOut bool
}

// NewDockerManager creates a DockerManager that reports progress via the given
// reporter (which may be nil).
func NewDockerManager(progress *ProgressReporter) *DockerManager {
	return &DockerManager{
		progress:        progress,
		completedLayers: map[string]struct{}{},
	}
}

// LoginDockerECR logs in to a private ECR registry for pulling images.
func (d *DockerManager) LoginDockerECR(registry, username, password string) bool {
	if registry == "" || username == "" || password == "" {
		zap.S().Errorw("ECR login failed: missing required parameters")
		return false
	}
	res, err := runCommand(30*time.Second, password,
		"docker", "login", "--username", username, "--password-stdin", registry)
	if err != nil {
		zap.S().Errorw("ECR login error", "error", err)
		return false
	}
	if res.timedOut {
		zap.S().Errorw("ECR login timed out")
		return false
	}
	if res.code == 0 {
		zap.S().Infow("ECR login successful", "registry", registry)
		return true
	}
	zap.S().Errorw("ECR login failed", "registry", registry)
	return false
}

// StopDockerServices stops and removes the containers defined in the compose
// config. Returns nil on success.
func (d *DockerManager) StopDockerServices(compose map[string]any) error {
	services := servicesMap(compose)
	if len(services) == 0 {
		zap.S().Warnw("No services defined in update YAML")
		return nil
	}

	var stopped, failed []string
	for serviceName, cfg := range services {
		cn := containerName(serviceName, cfg)
		if err := d.stopOne(serviceName, cn, &stopped); err != nil {
			zap.S().Errorw("Error stopping service", "service", serviceName, "error", err)
			failed = append(failed, serviceName)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to stop services: %s", strings.Join(failed, ", "))
	}
	zap.S().Infow("Stopped services", "count", len(stopped))
	return nil
}

// stopOne stops/removes a single container, escalating from graceful to forced operations.
func (d *DockerManager) stopOne(serviceName, cn string, stopped *[]string) error {
	running, err := d.containerMatch(10*time.Second, "docker", "ps", "-q", "--filter", "name="+cn)
	if err != nil {
		return err
	}

	if running {
		stopRes, err := runCommand(30*time.Second, "", "docker", "stop", cn)
		if err != nil {
			return err
		}
		if stopRes.code == 0 {
			zap.S().Infow("Stopped container", "container", cn)
			rmRes, err := runCommand(10*time.Second, "", "docker", "rm", cn)
			if err != nil {
				return err
			}
			if rmRes.code == 0 {
				zap.S().Infow("Removed container", "container", cn)
				*stopped = append(*stopped, cn)
				return nil
			}
			zap.S().Warnw("Normal remove failed, trying force remove", "container", cn)
			frRes, err := runCommand(10*time.Second, "", "docker", "rm", "-f", cn)
			if err != nil {
				return err
			}
			if frRes.code == 0 {
				zap.S().Infow("Force removed container", "container", cn)
				*stopped = append(*stopped, cn)
				return nil
			}
			return fmt.Errorf("failed to remove container %s: %s", cn, frRes.stderr)
		}

		zap.S().Warnw("Normal stop failed, trying force stop", "container", cn)
		killRes, err := runCommand(10*time.Second, "", "docker", "kill", cn)
		if err != nil {
			return err
		}
		if killRes.code == 0 {
			zap.S().Infow("Force stopped container", "container", cn)
			rmRes, err := runCommand(10*time.Second, "", "docker", "rm", "-f", cn)
			if err != nil {
				return err
			}
			if rmRes.code == 0 {
				zap.S().Infow("Removed container after force stop", "container", cn)
				*stopped = append(*stopped, cn)
				return nil
			}
			return fmt.Errorf("failed to remove container after force stop %s: %s", cn, rmRes.stderr)
		}
		if strings.Contains(killRes.stderr, "No such container") {
			zap.S().Infow("Container no longer exists, considering as stopped", "container", cn)
			*stopped = append(*stopped, cn)
			return nil
		}
		return fmt.Errorf("failed to force stop container %s: %s", cn, killRes.stderr)
	}

	exists, err := d.containerMatch(10*time.Second, "docker", "ps", "-a", "-q", "--filter", "name="+cn)
	if err != nil {
		return err
	}
	if exists {
		rmRes, err := runCommand(10*time.Second, "", "docker", "rm", "-f", cn)
		if err != nil {
			return err
		}
		if rmRes.code == 0 {
			zap.S().Infow("Removed stopped container", "container", cn)
		} else {
			zap.S().Warnw("Failed to remove stopped container", "container", cn, "stderr", rmRes.stderr)
		}
	} else {
		zap.S().Infow("Container not found, considering as already stopped", "container", cn)
		*stopped = append(*stopped, serviceName)
	}
	return nil
}

// StartDockerServices pulls images and starts the services via docker-compose.
func (d *DockerManager) StartDockerServices(compose map[string]any) error {
	services := servicesMap(compose)
	if len(services) == 0 {
		zap.S().Warnw("No services defined in update YAML")
		return nil
	}

	tempCompose, err := d.createTempComposeFile(compose)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.Remove(tempCompose); rmErr != nil && !os.IsNotExist(rmErr) {
			zap.S().Warnw("Failed to clean up temporary compose file", "error", rmErr)
		}
	}()

	zap.S().Infow("Pulling Docker images...")
	d.sendProgress("pulling", "Starting to pull Docker images", 30)

	if err := d.pullImagesWithProgress(tempCompose); err != nil {
		return fmt.Errorf("docker-compose pull failed: %w", err)
	}

	zap.S().Infow("Successfully pulled Docker images")
	d.sendProgress("pulled", "Successfully pulled Docker images", 70)

	zap.S().Infow("Starting Docker services...")
	d.sendProgress("starting_services", "Starting Docker services", 80)

	upRes, err := runCommand(120*time.Second, "",
		"docker-compose", "-f", tempCompose, "up", "-d", "--no-build")
	if err != nil {
		return err
	}
	if upRes.timedOut {
		zap.S().Errorw("Timeout starting Docker services")
		return errors.New("timeout starting services")
	}
	if upRes.code != 0 {
		zap.S().Errorw("Failed to start services", "stderr", upRes.stderr)
		return fmt.Errorf("docker-compose up failed: %s", upRes.stderr)
	}

	zap.S().Infow("Successfully started services with docker-compose")
	d.cleanupOldImages()
	return nil
}

// RecreateDockerServices recreates the services via docker-compose, without pulling.
func (d *DockerManager) RecreateDockerServices(compose map[string]any) error {
	services := servicesMap(compose)
	if len(services) == 0 {
		zap.S().Warnw("No services defined in update YAML")
		return nil
	}

	tempCompose, err := d.createTempComposeFile(compose)
	if err != nil {
		return err
	}
	defer func() {
		if rmErr := os.Remove(tempCompose); rmErr != nil && !os.IsNotExist(rmErr) {
			zap.S().Warnw("Failed to clean up temporary compose file", "error", rmErr)
		}
	}()

	zap.S().Infow("Recreating Docker services with updated configuration...")
	d.sendProgress("starting_services", "Recreating Docker services", 80)

	upRes, err := runCommand(120*time.Second, "",
		"docker-compose", "-f", tempCompose, "up", "-d", "--no-build", "--force-recreate")
	if err != nil {
		return err
	}
	if upRes.timedOut {
		zap.S().Errorw("Timeout recreating Docker services")
		return errors.New("timeout recreating services")
	}
	if upRes.code != 0 {
		zap.S().Errorw("Failed to recreate services", "stderr", upRes.stderr)
		return fmt.Errorf("docker-compose up failed: %s", upRes.stderr)
	}

	zap.S().Infow("Successfully recreated services with docker-compose")
	return nil
}

// pullImagesWithProgress runs `docker-compose -f <file> pull`, streaming output
// and emitting progress updates parsed from the log lines.
func (d *DockerManager) pullImagesWithProgress(composeFile string) error {
	d.mu.Lock()
	d.completedLayers = map[string]struct{}{}
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker-compose", "-f", composeFile, "pull")

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("start pull: %w", err)
	}
	pw.Close()

	var lines []string
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lines = append(lines, line)
		if line != "" {
			zap.S().Infow("Docker pull output", "line", line)
			d.parsePullLine(line)
		}
	}
	pr.Close()

	waitErr := cmd.Wait()
	stdoutText := strings.Join(lines, "\n")

	if ctx.Err() == context.DeadlineExceeded {
		zap.S().Errorw("Docker pull operation timed out")
		return errors.New("Docker pull operation timed out")
	}

	if waitErr == nil {
		zap.S().Infow("Successfully pulled all Docker images")
		return nil
	}

	var msg string
	if strings.Contains(stdoutText, "pull access denied") && strings.Contains(stdoutText, "authorization failed") {
		msg = "This service requires an Enterprise plan for private image access. Please upgrade your plan at https://portal.openmind.com"
	} else {
		msg = fmt.Sprintf("Pull failed: %s", stdoutText)
	}
	zap.S().Errorw(msg)
	return errors.New(msg)
}

// parsePullLine emits progress updates for a single line of pull output.
func (d *DockerManager) parsePullLine(line string) {
	switch {
	case strings.HasPrefix(line, "Pulling "):
		if m := rePulling.FindStringSubmatch(line); m != nil {
			d.sendProgress("pulling_service", "Pulling "+m[1], 30)
		}
	case strings.Contains(line, "Pull complete"):
		if m := rePullComplete.FindStringSubmatch(line); m != nil {
			d.mu.Lock()
			d.completedLayers[m[1]] = struct{}{}
			n := len(d.completedLayers)
			d.mu.Unlock()
			progress := 30 + min(n*5, 40)
			d.sendProgress("layer_complete", fmt.Sprintf("Completed layer %s...", truncate(m[1], 12)), progress)
		}
	case strings.Contains(line, "Downloading"):
		if m := reDownloading.FindStringSubmatch(line); m != nil {
			d.sendProgress("downloading",
				fmt.Sprintf("Downloading layer %s...: %s/%s", truncate(m[1], 12), m[2], m[3]), 35)
		}
	case strings.Contains(line, "Extracting"):
		if m := reExtracting.FindStringSubmatch(line); m != nil {
			d.sendProgress("extracting", fmt.Sprintf("Extracting layer %s...", truncate(m[1], 12)), 50)
		}
	}
}

// createTempComposeFile writes the compose config to a temp .yml file.
func (d *DockerManager) createTempComposeFile(compose map[string]any) (string, error) {
	data, err := yaml.Marshal(compose)
	if err != nil {
		return "", fmt.Errorf("marshal compose: %w", err)
	}
	f, err := os.CreateTemp("", "ota-compose-*.yml")
	if err != nil {
		return "", fmt.Errorf("create temp compose file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", fmt.Errorf("write temp compose file: %w", err)
	}
	zap.S().Infow("Created temporary docker-compose file", "path", f.Name())
	return f.Name(), nil
}

// cleanupOldImages prunes unused Docker resources to free disk space. Failures
// are logged but not fatal.
func (d *DockerManager) cleanupOldImages() {
	zap.S().Infow("Cleaning up old Docker images...")
	d.sendProgress("cleanup", "Cleaning up old Docker images", 90)

	cmds := []struct {
		name string
		args []string
	}{
		{"images", []string{"docker", "image", "prune", "-f"}},
		{"containers", []string{"docker", "container", "prune", "-f"}},
		{"system", []string{"docker", "system", "prune", "-f"}},
	}

	spaceFreed := "0B"
	anySuccess := false
	for _, c := range cmds {
		res, err := runCommand(60*time.Second, "", c.args[0], c.args[1:]...)
		if err != nil {
			zap.S().Warnw("Error during cleanup", "command", c.name, "error", err)
			continue
		}
		if res.timedOut {
			zap.S().Warnw("Timeout during cleanup", "command", c.name)
			continue
		}
		if res.code == 0 {
			anySuccess = true
			out := strings.TrimSpace(res.stdout)
			if m := reReclaimed.FindStringSubmatch(out); m != nil {
				spaceFreed = m[1]
			}
			zap.S().Infow("Successfully cleaned up", "command", c.name)
		} else {
			zap.S().Warnw("Failed to cleanup", "command", c.name, "stderr", res.stderr)
		}
	}

	if anySuccess {
		msg := fmt.Sprintf("Cleaned up Docker resources. Space freed: %s", spaceFreed)
		zap.S().Infow(msg)
		d.sendProgress("cleanup_complete", msg, 95)
	} else {
		zap.S().Infow("Cleanup completed but no space was freed")
	}
}

// PauseDockerServices pauses the running containers in the compose config.
func (d *DockerManager) PauseDockerServices(compose map[string]any) error {
	return d.simpleServiceOp(compose, "pause", func(cn string) []string {
		return []string{"docker", "ps", "-q", "--filter", "name=" + cn}
	}, []string{"docker", "pause"}, 30*time.Second, "is not running")
}

// UnpauseDockerServices unpauses paused containers in the compose config.
func (d *DockerManager) UnpauseDockerServices(compose map[string]any) error {
	return d.simpleServiceOp(compose, "unpause", func(cn string) []string {
		return []string{"docker", "ps", "-a", "-q", "--filter", "name=" + cn, "--filter", "status=paused"}
	}, []string{"docker", "unpause"}, 30*time.Second, "is not paused")
}

// RestartDockerServices restarts existing containers in the compose config.
func (d *DockerManager) RestartDockerServices(compose map[string]any) error {
	return d.simpleServiceOp(compose, "restart", func(cn string) []string {
		return []string{"docker", "ps", "-a", "-q", "--filter", "name=" + cn}
	}, []string{"docker", "restart"}, 60*time.Second, "not found")
}

// simpleServiceOp is the shared per-service loop for pause/unpause/restart: it
// checks existence with a filter command, then runs the action on matches.
func (d *DockerManager) simpleServiceOp(
	compose map[string]any,
	verb string,
	checkFn func(cn string) []string,
	action []string,
	actionTimeout time.Duration,
	skipMsg string,
) error {
	services := servicesMap(compose)
	if len(services) == 0 {
		zap.S().Warnw("No services defined in YAML")
		return nil
	}

	var done, failed []string
	for serviceName, cfg := range services {
		cn := containerName(serviceName, cfg)
		checkCmd := checkFn(cn)

		matched, err := d.containerMatch(10*time.Second, checkCmd[0], checkCmd[1:]...)
		if err != nil {
			zap.S().Errorw("Error during service op", "verb", verb, "service", serviceName, "error", err)
			failed = append(failed, serviceName)
			continue
		}
		if !matched {
			zap.S().Infow(fmt.Sprintf("Container %s %s", cn, skipMsg))
			continue
		}

		args := append(append([]string{}, action[1:]...), cn)
		res, err := runCommand(actionTimeout, "", action[0], args...)
		if err != nil {
			zap.S().Errorw("Error during service op", "verb", verb, "service", serviceName, "error", err)
			failed = append(failed, serviceName)
			continue
		}
		if res.timedOut {
			zap.S().Errorw("Timeout during service op", "verb", verb, "service", serviceName)
			failed = append(failed, serviceName)
			continue
		}
		if res.code == 0 {
			zap.S().Infow("Service op succeeded", "verb", verb, "container", cn)
			done = append(done, cn)
		} else {
			zap.S().Errorw("Service op failed", "verb", verb, "container", cn, "stderr", res.stderr)
			failed = append(failed, cn)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to %s services: %s", verb, strings.Join(failed, ", "))
	}
	zap.S().Infow("Service op completed", "verb", verb, "count", len(done))
	return nil
}

// containerMatch runs a `docker ps ... -q` style command and reports whether it
// returned any container IDs.
func (d *DockerManager) containerMatch(timeout time.Duration, name string, args ...string) (bool, error) {
	res, err := runCommand(timeout, "", name, args...)
	if err != nil {
		return false, err
	}
	if res.timedOut {
		return false, fmt.Errorf("timeout running %s", name)
	}
	return res.code == 0 && strings.TrimSpace(res.stdout) != "", nil
}

func (d *DockerManager) sendProgress(status, message string, progress int) {
	if d.progress != nil {
		d.progress.SendProgressUpdate(status, message, progress)
	}
}

// runCommand executes a command with a timeout and optional stdin.
func runCommand(timeout time.Duration, stdin, name string, args ...string) (cmdResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	res := cmdResult{stdout: outBuf.String(), stderr: errBuf.String()}

	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		return res, nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
			return res, nil
		}
		return res, err // e.g. binary not found
	}
	return res, nil
}

// servicesMap extracts the "services" map from a compose config.
func servicesMap(compose map[string]any) map[string]any {
	if compose == nil {
		return nil
	}
	s, ok := compose["services"].(map[string]any)
	if !ok {
		return nil
	}
	return s
}

// containerName returns the configured container_name for a service, falling
// back to the service name.
func containerName(serviceName string, cfg any) string {
	if m, ok := cfg.(map[string]any); ok {
		if cn, ok := m["container_name"].(string); ok && cn != "" {
			return cn
		}
	}
	return serviceName
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
