// Package s3 downloads OTA artifacts (compose YAML, schema JSON) from S3 or
// HTTPS URLs, verifies checksums, and exposes schema-derived env helpers. It is
// a port of the Python S3FileDownloader.
package s3

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"gopkg.in/yaml.v3"
)

// schemaURLTemplate is the source for per-tag schema files.
const schemaURLTemplate = "https://assets.openmind.com/ota/%s/schema.json"

const httpTimeout = 30 * time.Second

// Downloader fetches and verifies OTA artifacts.
type Downloader struct {
	updatesDir string
	httpClient *http.Client
}

// schemaEntry is one service definition inside a schema.json file.
type schemaEntry struct {
	Image string         `json:"image"`
	Env   map[string]any `json:"env"`
}

// NewDownloader creates a Downloader writing cached artifacts into updatesDir
// (default ".ota" when empty).
func NewDownloader(updatesDir string) (*Downloader, error) {
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
	return &Downloader{
		updatesDir: abs,
		httpClient: &http.Client{Timeout: httpTimeout},
	}, nil
}

// DownloadFile downloads s3Url to localPath. If localPath is empty a temp file
// is created. It supports both s3:// and https:// URLs. Returns the local path.
func (d *Downloader) DownloadFile(s3URL, localPath string) (string, error) {
	switch {
	case strings.HasPrefix(s3URL, "s3://"):
		return d.downloadWithS3(s3URL, localPath)
	case strings.HasPrefix(s3URL, "https://"):
		return d.downloadWithHTTP(s3URL, localPath)
	default:
		return "", fmt.Errorf("unsupported S3 URL format: %s", s3URL)
	}
}

func (d *Downloader) downloadWithS3(s3URL, localPath string) (string, error) {
	u, err := url.Parse(s3URL)
	if err != nil {
		return "", fmt.Errorf("parse s3 url: %w", err)
	}
	bucket := u.Host
	key := strings.TrimPrefix(u.Path, "/")

	if localPath == "" {
		localPath, err = tempFile()
		if err != nil {
			return "", err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("load aws config: %w", err)
	}
	client := awss3.NewFromConfig(cfg)

	slog.Info("Downloading from S3", "bucket", bucket, "key", key, "dest", localPath)
	out, err := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		_ = os.Remove(localPath)
		return "", fmt.Errorf("s3 GetObject: %w", err)
	}
	defer out.Body.Close()

	if err := writeToFile(localPath, out.Body); err != nil {
		_ = os.Remove(localPath)
		return "", err
	}
	return localPath, nil
}

func (d *Downloader) downloadWithHTTP(s3URL, localPath string) (string, error) {
	var err error
	if localPath == "" {
		localPath, err = tempFile()
		if err != nil {
			return "", err
		}
	}

	slog.Info("Downloading via HTTP", "url", s3URL, "dest", localPath)
	resp, err := d.httpClient.Get(s3URL)
	if err != nil {
		_ = os.Remove(localPath)
		return "", fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = os.Remove(localPath)
		return "", fmt.Errorf("http download failed: status %d", resp.StatusCode)
	}

	if err := writeToFile(localPath, resp.Body); err != nil {
		_ = os.Remove(localPath)
		return "", err
	}
	return localPath, nil
}

// VerifyChecksum reports whether the file at path matches expected (case
// insensitive). algorithm is "sha256" (default) or "md5".
func (d *Downloader) VerifyChecksum(path, expected, algorithm string) bool {
	actual, err := fileChecksum(path, algorithm)
	if err != nil {
		slog.Error("Failed to calculate checksum", "path", path, "error", err)
		return false
	}
	if strings.EqualFold(actual, expected) {
		slog.Info("Checksum verification passed", "path", path)
		return true
	}
	slog.Error("Checksum verification failed", "path", path, "expected", expected, "actual", actual)
	return false
}

// DownloadAndVerifyYAML downloads a YAML file, verifies its checksum, and parses
// it. It returns the parsed content and the local path on success. On failure
// the downloaded file is removed.
func (d *Downloader) DownloadAndVerifyYAML(s3URL, expectedChecksum, algorithm string) (map[string]any, string, error) {
	if algorithm == "" {
		algorithm = "sha256"
	}
	localPath, err := d.DownloadFile(s3URL, "")
	if err != nil {
		return nil, "", err
	}

	if !d.VerifyChecksum(localPath, expectedChecksum, algorithm) {
		_ = os.Remove(localPath)
		return nil, "", fmt.Errorf("checksum verification failed for %s", localPath)
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		_ = os.Remove(localPath)
		return nil, "", fmt.Errorf("read yaml: %w", err)
	}

	var content map[string]any
	if err := yaml.Unmarshal(data, &content); err != nil {
		_ = os.Remove(localPath)
		return nil, "", fmt.Errorf("parse yaml: %w", err)
	}

	slog.Info("Successfully downloaded and verified YAML", "url", s3URL)
	return content, localPath, nil
}

// DownloadSchema downloads the schema.json for tag and caches it to
// .ota/{tag}_schema.json.
func (d *Downloader) DownloadSchema(tag string) error {
	schemaURL := fmt.Sprintf(schemaURLTemplate, tag)
	cachePath := d.schemaCachePath(tag)

	if _, err := d.DownloadFile(schemaURL, cachePath); err != nil {
		return err
	}
	// Validate it parses, mirroring the Python behavior.
	if _, err := d.loadSchema(tag); err != nil {
		return err
	}
	slog.Info("Downloaded and cached schema", "path", cachePath)
	return nil
}

// GetDefaultEnv returns the schema-defined default env for a service/tag, or an
// empty map if unavailable.
func (d *Downloader) GetDefaultEnv(serviceName, tag string) map[string]string {
	schema, err := d.loadSchema(tag)
	if err != nil {
		return map[string]string{}
	}
	entry, ok := schema[serviceName]
	if !ok {
		return map[string]string{}
	}
	env := stringifyEnv(entry.Env)
	if len(env) > 0 {
		slog.Info("Using schema defaults", "service", serviceName, "tag", tag, "env", env)
	}
	return env
}

// GetSchemaEnvKeys returns the env keys declared for the service whose image
// matches imageName, or nil if none match.
func (d *Downloader) GetSchemaEnvKeys(tag, imageName string) []string {
	schema, err := d.loadSchema(tag)
	if err != nil {
		return nil
	}
	for _, entry := range schema {
		if entry.Image == imageName {
			keys := make([]string, 0, len(entry.Env))
			for k := range entry.Env {
				keys = append(keys, k)
			}
			return keys
		}
	}
	return nil
}

func (d *Downloader) schemaCachePath(tag string) string {
	return filepath.Join(d.updatesDir, tag+"_schema.json")
}

func (d *Downloader) loadSchema(tag string) (map[string]schemaEntry, error) {
	data, err := os.ReadFile(d.schemaCachePath(tag))
	if err != nil {
		return nil, err
	}
	var schema map[string]schemaEntry
	if err := json.Unmarshal(data, &schema); err != nil {
		slog.Error("Failed to read schema cache", "error", err)
		return nil, err
	}
	return schema, nil
}

// --- helpers ---

func fileChecksum(path, algorithm string) (string, error) {
	var h hash.Hash
	switch strings.ToLower(algorithm) {
	case "", "sha256":
		h = sha256.New()
	case "md5":
		h = md5.New()
	default:
		return "", fmt.Errorf("unsupported algorithm: %s", algorithm)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeToFile(path string, r io.Reader) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

func tempFile() (string, error) {
	f, err := os.CreateTemp("", "ota-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return name, nil
}

// stringifyEnv converts schema env values to strings, matching Python's str().
func stringifyEnv(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
