package ota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ECRHandler requests short-lived ECR credentials and performs docker login for
// private images.
type ECRHandler struct {
	docker         *DockerManager
	progress       *ProgressReporter
	credentialsURL string
	apiKey         string
	httpClient     *http.Client
}

// ecrCredentials is the response from the credentials endpoint.
type ecrCredentials struct {
	Registry  string `json:"registry"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	ExpiresAt string `json:"expires_at"`
}

// NewECRHandler creates an ECRHandler.
func NewECRHandler(docker *DockerManager, progress *ProgressReporter, credentialsURL, apiKey string) *ECRHandler {
	return &ECRHandler{
		docker:         docker,
		progress:       progress,
		credentialsURL: credentialsURL,
		apiKey:         apiKey,
		httpClient:     &http.Client{Timeout: 30 * time.Second},
	}
}

// CheckImagePrivacy returns the ECR repository name if any service uses a
// private ECR image, or "" otherwise.
func (h *ECRHandler) CheckImagePrivacy(compose map[string]any) string {
	for _, cfg := range servicesMap(compose) {
		m, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		image, _ := m["image"].(string)
		if !strings.Contains(image, ".dkr.ecr.") {
			continue
		}
		repo := image
		if slash := strings.Index(repo, "/"); slash > 0 && strings.Contains(repo[:slash], ".") {
			repo = repo[slash+1:]
		}
		if colon := strings.Index(repo, ":"); colon > 0 {
			repo = repo[:colon]
		}
		return repo
	}
	return ""
}

// LoginWithCredentials fetches ECR credentials for image and performs docker
// login. Returns true on success.
func (h *ECRHandler) LoginWithCredentials(image string) bool {
	if h.credentialsURL == "" || h.apiKey == "" {
		slog.Error("ECR_CREDENTIALS_URL or OM_API_KEY not configured")
		h.progress.SendProgressUpdate("error", "ECR credentials endpoint not configured", 15)
		return false
	}

	h.progress.SendProgressUpdate("authenticating", "Requesting ECR credentials", 15)

	body, _ := json.Marshal(map[string]string{"image": image})
	req, err := http.NewRequest(http.MethodPost, h.credentialsURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("ECR credentials request failed", "error", err)
		h.progress.SendProgressUpdate("error", fmt.Sprintf("ECR credentials request failed: %v", err), 15)
		return false
	}
	req.Header.Set("x-api-key", h.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		slog.Error("ECR credentials request failed", "error", err)
		h.progress.SendProgressUpdate("error", fmt.Sprintf("ECR credentials request failed: %v", err), 15)
		return false
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := extractErrorDetail(respBody)
		slog.Error("ECR credentials error", "status", resp.StatusCode, "detail", detail)
		h.progress.SendProgressUpdate("error", "ECR credentials error: "+detail, 15)
		return false
	}

	var creds ecrCredentials
	if err := json.Unmarshal(respBody, &creds); err != nil {
		slog.Error("Failed to parse ECR credentials", "error", err)
		h.progress.SendProgressUpdate("error", "Failed to parse ECR credentials", 15)
		return false
	}

	if !h.docker.LoginDockerECR(creds.Registry, creds.Username, creds.Password) {
		slog.Error("Docker ECR login failed")
		h.progress.SendProgressUpdate("error", "Docker ECR login failed", 15)
		return false
	}

	slog.Info("ECR login succeeded", "expires_at", creds.ExpiresAt)
	return true
}

// extractErrorDetail pulls an "error" field from a JSON body, falling back to
// the raw body text.
func extractErrorDetail(body []byte) string {
	if len(body) == 0 {
		return "unknown"
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err == nil {
		if msg, ok := parsed["error"].(string); ok {
			return msg
		}
	}
	return string(body)
}
