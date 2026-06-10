package configsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stateFileName is the local sync-state file stored inside the config dir.
const stateFileName = ".om1_sync.json"

const httpTimeout = 60 * time.Second

// ErrForbidden is returned when the server rejects the caller (HTTP 403).
var ErrForbidden = errors.New("config sync forbidden for this account")

// fileState is the recorded state of one managed file.
type fileState struct {
	ETag string `json:"etag"`
	Size int64  `json:"size"`
}

// syncState is the on-disk record of all files this syncer manages.
type syncState struct {
	Files map[string]fileState `json:"files"`
}

// manifestFile mirrors one entry of the server's manifest response.
type manifestFile struct {
	Key  string `json:"key"`
	ETag string `json:"etag"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// manifest mirrors the server's GET /ota/config/manifest response body.
type manifest struct {
	Files       []manifestFile `json:"files"`
	GeneratedAt string         `json:"generated_at"`
}

// Syncer downloads managed config files into a local directory.
type Syncer struct {
	manifestURL string
	apiKey      string
	configDir   string
	statePath   string
	httpClient  *http.Client
}

// New creates a Syncer that fetches the manifest from manifestURL and writes files into configDir.
func New(manifestURL, apiKey, configDir string) *Syncer {
	return &Syncer{
		manifestURL: manifestURL,
		apiKey:      apiKey,
		configDir:   configDir,
		statePath:   filepath.Join(configDir, stateFileName),
		httpClient:  &http.Client{Timeout: httpTimeout},
	}
}

// Sync performs one sync pass: fetch the manifest, download new/changed files,
// delete files removed upstream, and persist the updated state.
func (s *Syncer) Sync(ctx context.Context) error {
	if err := os.MkdirAll(s.configDir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	man, err := s.fetchManifest(ctx)
	if err != nil {
		return err
	}

	state := s.loadState()
	remote := make(map[string]manifestFile, len(man.Files))

	downloaded, deleted := 0, 0

	for _, f := range man.Files {
		remote[f.Key] = f
		dest, err := s.destPath(f.Key)
		if err != nil {
			slog.Warn("Skipping unsafe config key", "key", f.Key, "error", err)
			continue
		}

		prev, known := state.Files[f.Key]
		_, statErr := os.Stat(dest)
		upToDate := known && prev.ETag == f.ETag && statErr == nil
		if upToDate {
			continue
		}

		if err := s.download(ctx, f.URL, dest); err != nil {
			slog.Error("Failed to download config file", "key", f.Key, "error", err)
			continue
		}
		state.Files[f.Key] = fileState{ETag: f.ETag, Size: f.Size}
		downloaded++
		slog.Info("Synced config file", "key", f.Key)
	}

	for key := range state.Files {
		if _, ok := remote[key]; ok {
			continue
		}
		dest, err := s.destPath(key)
		if err != nil {
			delete(state.Files, key)
			continue
		}
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			slog.Warn("Failed to remove stale config file", "key", key, "error", err)
			continue
		}
		delete(state.Files, key)
		deleted++
		slog.Info("Removed stale config file", "key", key)
	}

	if err := s.saveState(state); err != nil {
		return fmt.Errorf("save sync state: %w", err)
	}

	if downloaded > 0 || deleted > 0 {
		slog.Info("Config sync complete", "downloaded", downloaded, "deleted", deleted, "total", len(man.Files))
	}
	return nil
}

// fetchManifest GETs and decodes the manifest, mapping 403 to ErrForbidden.
func (s *Syncer) fetchManifest(ctx context.Context) (*manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build manifest request: %w", err)
	}
	req.Header.Set("x-api-key", s.apiKey)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrForbidden
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("manifest request failed: status %d", resp.StatusCode)
	}

	var man manifest
	if err := json.NewDecoder(resp.Body).Decode(&man); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &man, nil
}

// download fetches url into dest atomically (temp file + rename).
func (s *Syncer) download(ctx context.Context, url, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".om1_dl_*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return nil
}

// destPath resolves a manifest key to an absolute path inside configDir,
// rejecting any key that would escape the directory (path traversal).
func (s *Syncer) destPath(key string) (string, error) {
	clean := filepath.Clean("/" + key) // anchor to root, collapses .. at the top
	dest := filepath.Join(s.configDir, clean)
	rel, err := filepath.Rel(s.configDir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("key escapes config dir")
	}
	return dest, nil
}

// loadState reads the sync-state file, returning an empty state if absent or
// unreadable (so a corrupt state simply triggers a full re-sync).
func (s *Syncer) loadState() syncState {
	state := syncState{Files: map[string]fileState{}}
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Files == nil {
		return syncState{Files: map[string]fileState{}}
	}
	return state
}

// saveState writes the sync-state file atomically.
func (s *Syncer) saveState(state syncState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}
