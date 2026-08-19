package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/OpenMind/OM1-OTA/internal/s3"
)

func newTestDownloader(t *testing.T) *s3.Downloader {
	t.Helper()
	d, err := s3.NewDownloader(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create downloader: %v", err)
	}
	return d
}

type fakeDownloader struct{}

func (fakeDownloader) DownloadFile(url, localPath string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	out, err := os.Create(localPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", err
	}
	return localPath, nil
}

func (fakeDownloader) VerifyChecksum(path, expected, _ string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == expected
}

func platformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func TestCheckAndApply_NoOpWhenNotNewer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"generated_at":"now","builds":{%q:{"build_timestamp":"100","url":"unused","checksum":"unused","checksum_algorithm":"sha256"}}}`, platformKey())
	}))
	defer server.Close()

	u := NewUpdater(server.URL, "100", newTestDownloader(t))
	exited := false
	u.exitFunc = func(int) { exited = true }

	if err := u.CheckAndApply(); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if exited {
		t.Fatal("must not restart when the manifest isn't newer")
	}
}

func TestCheckAndApply_NoOpWhenPlatformNotAdvertised(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"generated_at":"now","builds":{"some/other-platform":{"build_timestamp":"999"}}}`)
	}))
	defer server.Close()

	u := NewUpdater(server.URL, "0", newTestDownloader(t))
	exited := false
	u.exitFunc = func(int) { exited = true }

	if err := u.CheckAndApply(); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}
	if exited {
		t.Fatal("must not restart when this platform has no advertised build")
	}
}

func TestCheckAndApply_DownloadsVerifiesReplacesAndRestarts(t *testing.T) {
	newBinaryContent := []byte("pretend this is a new terminal binary")
	sum := sha256.Sum256(newBinaryContent)
	checksum := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(newBinaryContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"generated_at":"now","builds":{%q:{"build_timestamp":"200","url":%q,"checksum":%q,"checksum_algorithm":"sha256"}}}`,
			platformKey(), server.URL+"/binary", checksum)
	})

	selfPath := filepath.Join(t.TempDir(), "fake-terminal-binary")
	if err := os.WriteFile(selfPath, []byte("old binary content"), 0o755); err != nil {
		t.Fatalf("failed to seed fake self binary: %v", err)
	}

	u := newUpdater(server.URL+"/manifest", "100", fakeDownloader{})
	u.selfPathFunc = func() (string, error) { return selfPath, nil }

	var exitCode = -1
	u.exitFunc = func(code int) { exitCode = code }

	if err := u.CheckAndApply(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("expected exitFunc to be called with 0, got %d (called: %v)", exitCode, exitCode != -1)
	}

	replaced, err := os.ReadFile(selfPath)
	if err != nil {
		t.Fatalf("failed to read replaced binary: %v", err)
	}
	if string(replaced) != string(newBinaryContent) {
		t.Fatalf("binary was not replaced with the new content: got %q", replaced)
	}

	info, err := os.Stat(selfPath)
	if err != nil {
		t.Fatalf("failed to stat replaced binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("replaced binary is not executable: mode %v", info.Mode())
	}
}

func TestCheckAndApply_RefusesToReplaceOnChecksumMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/binary", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("some content"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"generated_at":"now","builds":{%q:{"build_timestamp":"200","url":%q,"checksum":"0000000000000000000000000000000000000000000000000000000000000000","checksum_algorithm":"sha256"}}}`,
			platformKey(), server.URL+"/binary")
	})

	selfPath := filepath.Join(t.TempDir(), "fake-terminal-binary")
	original := []byte("original binary content")
	if err := os.WriteFile(selfPath, original, 0o755); err != nil {
		t.Fatalf("failed to seed fake self binary: %v", err)
	}

	u := newUpdater(server.URL+"/manifest", "100", fakeDownloader{})
	u.selfPathFunc = func() (string, error) { return selfPath, nil }

	exited := false
	u.exitFunc = func(int) { exited = true }

	if err := u.CheckAndApply(); err == nil {
		t.Fatal("expected a checksum verification error, got nil")
	}
	if exited {
		t.Fatal("must not restart when checksum verification fails")
	}

	after, err := os.ReadFile(selfPath)
	if err != nil {
		t.Fatalf("failed to read binary after failed update: %v", err)
	}
	if string(after) != string(original) {
		t.Fatal("the live binary must be left untouched when checksum verification fails")
	}

	if _, err := os.Stat(selfPath + ".update"); !os.IsNotExist(err) {
		t.Fatal("the failed download temp file must be cleaned up")
	}
}
