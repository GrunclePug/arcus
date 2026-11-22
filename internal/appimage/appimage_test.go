package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads and writes regardless of mode")
	}
}

func TestDecide(t *testing.T) {
	pin, old := sum("v2"), sum("v1")
	tests := []struct {
		name    string
		setup   func(path string)
		rec     string
		want    Action
		wantErr string
	}{
		{"nothing there", func(string) {}, "", Install, ""},
		{"nothing there, recorded", func(string) {}, old, Install, ""},
		{"the pin, unrecorded", func(p string) { write(t, p, "v2", 0o644) }, "", Adopt, ""},
		{"the pin, recorded as another", func(p string) { write(t, p, "v2", 0o755) }, old, Adopt, ""},
		{"the recorded pin, executable", func(p string) { write(t, p, "anything", 0o755) }, pin, Keep, ""},
		{"the recorded pin, not executable", func(p string) { write(t, p, "v2", 0o644) }, pin, Adopt, ""},
		{"recorded as the pin, changed, not executable", func(p string) { write(t, p, "mine", 0o644) }, pin, Theirs, ""},
		{"the recorded file, pin moved", func(p string) { write(t, p, "v1", 0o755) }, old, Update, ""},
		{"changed since recorded", func(p string) { write(t, p, "v3", 0o755) }, old, Theirs, ""},
		{"another file, unrecorded", func(p string) { write(t, p, "mine", 0o755) }, "", 0, "isn't the pinned AppImage"},
		{"a directory", func(p string) { os.MkdirAll(p, 0o755) }, old, 0, "isn't a regular file"},
		{"a symlink", func(p string) { write(t, p+".real", "v2", 0o755); os.Symlink(p+".real", p) }, "", 0, "isn't a regular file"},
		{"a leftover download", func(p string) { write(t, p+".arcus-tmp", "half", 0o600) }, "", 0, "left by an interrupted download"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Applications", "a.AppImage")
			tt.setup(path)
			got, err := Decide(path, pin, tt.rec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Decide = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// A file arcus can't read can't be proven its own: recorded, it is left;
// unrecorded, it is refused like any other file.
func TestDecideUnreadable(t *testing.T) {
	skipAsRoot(t)
	pin := sum("v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v2", 0o000)
	if got, err := Decide(path, pin, pin); err != nil || got != Theirs {
		t.Errorf("recorded: %v, %v; want Theirs", got, err)
	}
	if _, err := Decide(path, pin, ""); err == nil || !strings.Contains(err.Error(), "isn't the pinned AppImage") {
		t.Errorf("unrecorded: %v", err)
	}
}

func TestRemovable(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "mine")
	write(t, mine, "v1", 0o755)
	if !Removable(mine, sum("v1")) {
		t.Error("the recorded file")
	}
	if Removable(mine, sum("v2")) {
		t.Error("a changed file")
	}
	if Removable(filepath.Join(dir, "none"), sum("v1")) {
		t.Error("a missing file")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(mine, link); err != nil {
		t.Fatal(err)
	}
	if Removable(link, sum("v1")) {
		t.Error("a symlink to the recorded file")
	}
}

// serve starts a TLS server for body at /a and points Client at it.
func serve(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	saved := Client
	Client = srv.Client()
	t.Cleanup(func() { Client = saved })
	return srv.URL + "/a"
}

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(s)) }
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestFetchInstall(t *testing.T) {
	url := serve(t, body("v1"))
	dir := filepath.Join(t.TempDir(), "Applications")
	path := filepath.Join(dir, "a.AppImage")
	placed, err := Fetch(context.Background(), url, sum("v1"), path, "")
	if err != nil || !placed {
		t.Fatalf("Fetch = %v, %v", placed, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed: %v, %v", info, err)
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("left beside it: %v", got)
	}
}

func TestFetchUpdate(t *testing.T) {
	url := serve(t, body("v2"))
	dir := t.TempDir()
	path := filepath.Join(dir, "a.AppImage")
	write(t, path, "v1", 0o755)
	if placed, err := Fetch(context.Background(), url, sum("v2"), path, sum("v1")); err != nil || !placed {
		t.Fatalf("Fetch = %v, %v", placed, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2" {
		t.Errorf("content = %q", data)
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("left beside it: %v", got)
	}
}

// Every failure before placing leaves the path as it was and nothing of the
// download behind.
func TestFetchFailuresLeaveNothing(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		ctx     context.Context
		before  string // file at the path before, "" for none
		old     string
		during  func(path string)
		wantErr string
	}{
		{"wrong content", body("evil"), context.Background(), "", "", nil, "doesn't match the pinned sha256"},
		{"not found", http.NotFound, context.Background(), "", "", nil, "404"},
		{"cancelled", body("v2"), cancelled, "", "", nil, "context canceled"},
		{"appeared during the download", body("v2"), context.Background(), "", "", func(p string) { write(t, p, "user's", 0o644) }, "appeared while it was downloaded"},
		{"changed during the update", body("v2"), context.Background(), "v1", sum("v1"), func(p string) { write(t, p, "updated by the app", 0o755) }, "changed while the new version was downloaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.AppImage")
			if tt.before != "" {
				write(t, path, tt.before, 0o755)
			}
			handler := tt.handler
			if tt.during != nil {
				handler = func(w http.ResponseWriter, r *http.Request) { tt.during(path); tt.handler(w, r) }
			}
			url := serve(t, handler)
			placed, err := Fetch(tt.ctx, url, sum("v2"), path, tt.old)
			if placed || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Fetch = %v, %v; want %q", placed, err, tt.wantErr)
			}
			if _, err := os.Stat(path + ".arcus-tmp"); !os.IsNotExist(err) {
				t.Errorf("temp file left: %v", err)
			}
			want := tt.before
			if tt.during != nil {
				data, _ := os.ReadFile(path)
				want = string(data)
			}
			if data, err := os.ReadFile(path); want != "" && string(data) != want || want == "" && err == nil {
				t.Errorf("path = %q, %v; want %q", data, err, want)
			}
		})
	}
}

// A temp file arcus didn't create in this run is refused and kept.
func TestFetchKeepsALeftover(t *testing.T) {
	url := serve(t, body("v2"))
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path+".arcus-tmp", "half", 0o600)
	if _, err := Fetch(context.Background(), url, sum("v2"), path, ""); err == nil || !strings.Contains(err.Error(), "interrupted download") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(path + ".arcus-tmp"); string(data) != "half" {
		t.Errorf("leftover = %q", data)
	}
}

// A directory that can't be opened for the final sync still has the new
// file: Fetch says it was placed, so the caller records it.
func TestFetchPlacedThenSyncFails(t *testing.T) {
	skipAsRoot(t)
	url := serve(t, body("v2"))
	dir := filepath.Join(t.TempDir(), "Applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "a.AppImage")
	write(t, path, "v1", 0o755)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	placed, err := Fetch(context.Background(), url, sum("v2"), path, sum("v1"))
	if !placed || err == nil {
		t.Fatalf("Fetch = %v, %v; want placed with an error", placed, err)
	}
	os.Chmod(dir, 0o755)
	if data, _ := os.ReadFile(path); string(data) != "v2" {
		t.Errorf("content = %q", data)
	}
}
