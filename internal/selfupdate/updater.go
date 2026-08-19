package selfupdate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/OpenMind/OM1-OTA/internal/s3"
)

const httpTimeout = 30 * time.Second

type buildEntry struct {
	BuildTimestamp    string `json:"build_timestamp"`
	URL               string `json:"url"`
	Checksum          string `json:"checksum"`
	ChecksumAlgorithm string `json:"checksum_algorithm"`
}

type manifest struct {
	GeneratedAt string                `json:"generated_at"`
	Builds      map[string]buildEntry `json:"builds"`
}

type fileDownloader interface {
	DownloadFile(url, localPath string) (string, error)
	VerifyChecksum(path, expected, algorithm string) bool
}

type Updater struct {
	manifestURL    string
	currentVersion string
	downloader     fileDownloader
	httpClient     *http.Client

	exitFunc func(code int)

	selfPathFunc func() (string, error)
}

func NewUpdater(manifestURL, currentVersion string, downloader *s3.Downloader) *Updater {
	return newUpdater(manifestURL, currentVersion, downloader)
}

func newUpdater(manifestURL, currentVersion string, downloader fileDownloader) *Updater {
	return &Updater{
		manifestURL:    manifestURL,
		currentVersion: currentVersion,
		downloader:     downloader,
		httpClient:     &http.Client{Timeout: httpTimeout},
		exitFunc:       os.Exit,
		selfPathFunc:   os.Executable,
	}
}

func (u *Updater) CheckAndApply() error {
	m, err := u.fetchManifest()
	if err != nil {
		return fmt.Errorf("fetch update manifest: %w", err)
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	entry, ok := m.Builds[platform]
	if !ok {
		zap.S().Debugw("No build advertised for this platform", "platform", platform)
		return nil
	}

	isNewer, err := isNewerVersion(entry.BuildTimestamp, u.currentVersion)
	if err != nil {
		return fmt.Errorf("compare build timestamps: %w", err)
	}
	if !isNewer {
		return nil
	}

	zap.S().Infow("Newer terminal build available, updating",
		"current", u.currentVersion, "available", entry.BuildTimestamp, "url", entry.URL)

	selfPath, err := u.selfPathFunc()
	if err != nil {
		return fmt.Errorf("resolve own executable path: %w", err)
	}

	downloadPath := selfPath + ".update"
	if _, err := u.downloader.DownloadFile(entry.URL, downloadPath); err != nil {
		return fmt.Errorf("download update: %w", err)
	}

	if !u.downloader.VerifyChecksum(downloadPath, entry.Checksum, entry.ChecksumAlgorithm) {
		_ = os.Remove(downloadPath)
		return fmt.Errorf("checksum verification failed for update at %s", entry.URL)
	}

	if err := os.Chmod(downloadPath, 0o755); err != nil {
		_ = os.Remove(downloadPath)
		return fmt.Errorf("chmod update: %w", err)
	}

	if err := os.Rename(downloadPath, selfPath); err != nil {
		_ = os.Remove(downloadPath)
		return fmt.Errorf("replace running binary: %w", err)
	}

	zap.S().Infow("Replaced running binary, restarting", "path", selfPath, "build_timestamp", entry.BuildTimestamp)
	u.exitFunc(0)
	return nil
}

func (u *Updater) fetchManifest() (*manifest, error) {
	resp, err := u.httpClient.Get(u.manifestURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

func isNewerVersion(candidate, current string) (bool, error) {
	c, err := strconv.ParseInt(candidate, 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse candidate timestamp %q: %w", candidate, err)
	}
	cur, err := strconv.ParseInt(current, 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse current timestamp %q: %w", current, err)
	}
	return c > cur, nil
}
