package configsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeServer serves a manifest plus the file bodies it references.
type fakeServer struct {
	srv    *httptest.Server
	files  map[string][]byte // key -> body
	etags  map[string]string // key -> etag
	status int               // status for the manifest endpoint (0 => 200)
}

func newFakeServer() *fakeServer {
	f := &fakeServer{files: map[string][]byte{}, etags: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		var man manifest
		for key := range f.files {
			man.Files = append(man.Files, manifestFile{
				Key:  key,
				ETag: f.etags[key],
				Size: int64(len(f.files[key])),
				URL:  f.srv.URL + "/blob?key=" + key,
			})
		}
		_ = json.NewEncoder(w).Encode(man)
	})
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		body, ok := f.files[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeServer) put(key, etag, body string) {
	f.files[key] = []byte(body)
	f.etags[key] = etag
}

func (f *fakeServer) close() { f.srv.Close() }

func newSyncer(f *fakeServer, dir string) *Syncer {
	return New(f.srv.URL+"/manifest", "test-key", dir)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestSync_DownloadsNewFiles(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.put("robot.yaml", "etag-1", "name: r1")
	f.put("nested/conf.json", "etag-2", "{}")

	dir := t.TempDir()
	s := newSyncer(f, dir)

	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "robot.yaml")); got != "name: r1" {
		t.Errorf("robot.yaml = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "nested/conf.json")); got != "{}" {
		t.Errorf("nested/conf.json = %q", got)
	}
}

func TestSync_SkipsUnchangedAndUpdatesChanged(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.put("a.txt", "v1", "first")

	dir := t.TempDir()
	s := newSyncer(f, dir)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}

	// Change the upstream content + etag; a second sync should pull the update.
	f.put("a.txt", "v2", "second")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "second" {
		t.Errorf("a.txt = %q, want updated content", got)
	}
}

func TestSync_PreservesUserFiles(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.put("managed.txt", "v1", "managed")

	dir := t.TempDir()
	// A file the user dropped in the same directory; not in any manifest.
	userFile := filepath.Join(dir, "user_notes.txt")
	if err := os.WriteFile(userFile, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSyncer(f, dir)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}

	// Upstream removes the managed file entirely.
	delete(f.files, "managed.txt")
	delete(f.etags, "managed.txt")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}

	// Managed file should be gone...
	if _, err := os.Stat(filepath.Join(dir, "managed.txt")); !os.IsNotExist(err) {
		t.Errorf("managed.txt should have been deleted, stat err = %v", err)
	}
	// ...but the user's file must remain untouched.
	if got := readFile(t, userFile); got != "do not touch" {
		t.Errorf("user file = %q, want preserved", got)
	}
}

func TestSync_ReDownloadsLocallyDeletedFile(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.put("a.txt", "v1", "hello")

	dir := t.TempDir()
	s := newSyncer(f, dir)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}

	// User/process deletes the managed file locally; etag unchanged upstream.
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "a.txt")); got != "hello" {
		t.Errorf("a.txt = %q, want re-downloaded", got)
	}
}

func TestSync_Forbidden(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.status = http.StatusForbidden

	dir := t.TempDir()
	s := newSyncer(f, dir)
	if err := s.Sync(context.Background()); err != ErrForbidden {
		t.Fatalf("sync err = %v, want ErrForbidden", err)
	}
}

func TestSync_RejectsPathTraversal(t *testing.T) {
	f := newFakeServer()
	defer f.close()
	f.put("../escape.txt", "v1", "evil")

	dir := t.TempDir()
	s := newSyncer(f, dir)
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// The traversal key is anchored under dir, never written to the parent.
	parent := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("escape.txt should not exist in parent dir, err = %v", err)
	}
}
