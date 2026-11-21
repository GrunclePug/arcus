package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
)

func digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

// Every combination of record, file and self_update.
func TestDecide(t *testing.T) {
	pinned, old := digest("v2"), digest("v1")
	rec := func(sum string, selfUpdate bool) *state.AppImage {
		return &state.AppImage{SHA256: sum, SelfUpdate: selfUpdate}
	}
	tests := []struct {
		name       string
		file       string // "" for none
		rec        *state.AppImage
		selfUpdate bool
		want       Action
		touched    bool // the file kept its content but not its stamp
	}{
		{"missing, unrecorded", "", nil, false, Install, false},
		{"missing, recorded", "", rec(pinned, false), false, Install, false},
		{"missing, recorded, self-updating", "", rec(pinned, true), true, Install, false},
		{"pinned, unrecorded", "v2", nil, false, Adopt, false},
		{"pinned, unrecorded, self-updating", "v2", nil, true, Adopt, false},
		{"other, unrecorded", "other", nil, false, Replace, false},
		{"other, unrecorded, self-updating: as it is", "other", nil, true, Adopt, false},
		{"pinned, recorded", "v2", rec(pinned, false), false, Keep, false},
		{"pinned, recorded, touched", "v2", rec(pinned, false), false, Record, true},
		{"pin moved, file unchanged", "v1", rec(old, false), false, Update, false},
		{"pin moved, file modified", "other", rec(old, false), false, Replace, false},
		{"pinned but modified", "other", rec(pinned, false), false, Replace, false},
		{"pin moved, file already the new one", "v2", rec(old, false), false, Record, false},
		{"self-updating, changed by the app", "other", rec(old, true), true, Keep, false},
		{"self-updating, pin moved", "v1", rec(old, true), true, Keep, false},
		{"self_update turned on", "other", rec(old, false), true, Record, false},
		{"self_update turned off, pinned", "v2", rec(old, true), false, Record, false},
		{"self_update turned off, unchanged", "v1", rec(old, true), false, Update, false},
		{"self_update turned off, the app updated it", "other", rec(old, true), false, Replace, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "Applications", "a.AppImage")
			rec := tt.rec
			if tt.file != "" {
				write(t, path, tt.file)
				// A record of this very content has the file's stamp: arcus
				// took it when it installed the file.
				if rec != nil && rec.SHA256 == digest(tt.file) && !tt.touched {
					f, err := Sum(path, nil)
					if err != nil {
						t.Fatal(err)
					}
					rec = &state.AppImage{SHA256: rec.SHA256, SelfUpdate: rec.SelfUpdate, Stamp: f.Stamp}
				}
			}
			d := module.AppImage{URL: "https://x.org/a", SHA256: pinned, Path: "Applications/a.AppImage", SelfUpdate: tt.selfUpdate}
			got, f, err := Decide(path, d, rec)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			if tt.file != "" && f.SHA256 != "" && f.SHA256 != digest(tt.file) {
				t.Errorf("sha256 = %s", f.SHA256)
			}
			if tt.rec != nil && tt.selfUpdate && f.SHA256 != "" {
				t.Error("a recorded self-updating AppImage must not be read")
			}
		})
	}
}

func TestDecideRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.AppImage")
	write(t, filepath.Join(dir, "real"), "v2")
	if err := os.Symlink(filepath.Join(dir, "real"), path); err != nil {
		t.Fatal(err)
	}
	d := module.AppImage{SHA256: digest("v2")}
	if _, _, err := Decide(path, d, nil); err == nil || !strings.Contains(err.Error(), "isn't a regular file") {
		t.Errorf("symlink: %v", err)
	}
}

// Sum trusts the record while the stamp matches and reads the file once
// anything about it changed.
func TestSumCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a")
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil || f.SHA256 != digest("v1") || f.Stamp.Inode == 0 || f.Stamp.Size != 2 {
		t.Fatalf("Sum = %+v, %v", f, err)
	}
	// A record with this stamp is believed, even when it is wrong; that is
	// what saves reading a large file.
	cached := &state.AppImage{SHA256: "cached", Stamp: f.Stamp}
	if got, _ := Sum(path, cached); got.SHA256 != "cached" {
		t.Errorf("cache not used: %s", got.SHA256)
	}
	// Same size, new content and mtime: read again.
	write(t, path, "v2")
	if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := Sum(path, cached); got.SHA256 != digest("v2") {
		t.Errorf("stale cache used: %s", got.SHA256)
	}
}

func TestRemovable(t *testing.T) {
	home := t.TempDir()
	st := &state.State{}
	add := func(p, content string, selfUpdate bool) {
		write(t, filepath.Join(home, p), content)
		f, err := Sum(filepath.Join(home, p), nil)
		if err != nil {
			t.Fatal(err)
		}
		st.SetAppImage("m", p, state.AppImage{SHA256: f.SHA256, SelfUpdate: selfUpdate, Stamp: f.Stamp})
	}
	add("unchanged", "v1", false)
	add("modified", "v1", false)
	add("updated-by-app", "v1", true)
	add("gone", "v1", false)
	add("dir", "v1", true)
	add("linked/app", "v1", true)
	write(t, filepath.Join(home, "modified"), "mine")
	write(t, filepath.Join(home, "updated-by-app"), "v2")
	for _, p := range []string{"gone", "dir"} {
		if err := os.Remove(filepath.Join(home, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(home, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	linked := func(path string) string {
		if strings.HasPrefix(path, filepath.Join(home, "linked")+"/") {
			return filepath.Join(home, "linked")
		}
		return ""
	}
	got, err := Removable(home, st, linked)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"unchanged": true, "modified": false, "updated-by-app": true}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// serve answers every request with body.
func serve(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// entries lists a directory, so a test sees leftover temp files.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestFetch(t *testing.T) {
	srv := serve(t, "#!/bin/sh\nexit 1\n")
	home := t.TempDir()
	path := filepath.Join(home, "Applications", "sub", "a.AppImage")
	stamp, err := Fetch(context.Background(), srv.Client(), srv.URL+"/a", digest("#!/bin/sh\nexit 1\n"), path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != 0o755 {
		t.Fatalf("installed file: %v, %v", info, err)
	}
	if read(t, path) != "#!/bin/sh\nexit 1\n" {
		t.Errorf("content = %q", read(t, path))
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 {
		t.Errorf("leftovers next to the file: %v", got)
	}
	if f, err := Sum(path, nil); err != nil || f.Stamp != stamp {
		t.Errorf("returned stamp %+v, file has %+v (%v)", stamp, f.Stamp, err)
	}
}

// A replacement only happens once the download is verified: aside sees
// the old file in place and nothing half-written, and the new file
// is linked into place.
func TestFetchReplaces(t *testing.T) {
	srv := serve(t, "v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v1")
	moved := filepath.Join(t.TempDir(), "aside")
	aside := func() error {
		if read(t, path) != "v1" {
			t.Errorf("the old file changed before aside: %q", read(t, path))
		}
		return os.Rename(path, moved)
	}
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, aside); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "v2" || read(t, moved) != "v1" {
		t.Errorf("file = %q, set aside = %q", read(t, path), read(t, moved))
	}
}

// A wrong digest, a failed request or a cancelled one leave everything as
// it was: no temp file, no new directory, the old file untouched, aside
// never called.
func TestFetchFailsCleanly(t *testing.T) {
	ok := serve(t, "tampered")
	missing := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)
	offline := httptest.NewTLSServer(http.NotFoundHandler())
	offlineURL, offlineClient := offline.URL, offline.Client()
	offline.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		client  *http.Client
		url     string
		wantErr []string
	}{
		{"digest mismatch", context.Background(), ok.Client(), ok.URL + "/a.AppImage",
			[]string{ok.URL + "/a.AppImage", "expected " + digest("v2"), "got " + digest("tampered"), "nothing was installed"}},
		{"not found", context.Background(), missing.Client(), missing.URL, []string{"404", "nothing was installed"}},
		{"offline", context.Background(), offlineClient, offlineURL, []string{"downloading " + offlineURL, "nothing was installed"}},
		{"cancelled", cancelled, ok.Client(), ok.URL, []string{"context canceled"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, "old.AppImage"), "v1")
			for _, path := range []string{filepath.Join(home, "old.AppImage"), filepath.Join(home, "New", "Dir", "a.AppImage")} {
				aside := func() error { t.Error("aside ran"); return nil }
				_, err := Fetch(tt.ctx, tt.client, tt.url, digest("v2"), path, nil, aside)
				for _, want := range tt.wantErr {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("error %v lacks %q", err, want)
					}
				}
			}
			if got := entries(t, home); len(got) != 1 || got[0] != "old.AppImage" {
				t.Errorf("home holds %v", got)
			}
			if read(t, filepath.Join(home, "old.AppImage")) != "v1" {
				t.Error("the old file changed")
			}
		})
	}
}

func TestFetchStalls(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("v"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	defer func(d time.Duration) { StallTimeout = d }(StallTimeout)
	StallTimeout = 100 * time.Millisecond

	path := filepath.Join(t.TempDir(), "a.AppImage")
	_, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing arrived for 100ms") {
		t.Fatalf("stalled download: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a stalled download left a file")
	}
}

// A file that turns up at the path, or changes there, while the download
// runs is never overwritten.
func TestFetchNeverOverwritesWhatChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.AppImage")
	var meanwhile func()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meanwhile()
		_, _ = w.Write([]byte("v2"))
	}))
	t.Cleanup(srv.Close)

	// Install: nothing was there when apply decided.
	meanwhile = func() { write(t, path, "saved by the browser") }
	_, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "appeared while it was downloaded") || read(t, path) != "saved by the browser" {
		t.Errorf("install over a new file: %v, file %q", err, read(t, path))
	}

	// Update: arcus's file changed after apply decided to replace it.
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	meanwhile = func() {
		write(t, path, "v1 patched")
		if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
			t.Error(err)
		}
	}
	_, err = Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while the new version was downloaded") || read(t, path) != "v1 patched" {
		t.Errorf("update over a changed file: %v, file %q", err, read(t, path))
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 {
		t.Errorf("leftovers: %v", got)
	}

	// Update of the file as it was decided.
	meanwhile = func() {}
	f, err = Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil); err != nil || read(t, path) != "v2" {
		t.Errorf("update: %v, file %q", err, read(t, path))
	}
}

// A change that lands after the last check but before the swap is not
// overwritten either: the old file is checked once it is out of the way.
func TestFetchKeepsALastMomentChange(t *testing.T) {
	srv := serve(t, "v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeReplace = func() {
		write(t, path, "v1 patched")
		if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeReplace = func() {} })
	_, err = Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while the new version was downloaded") || read(t, path) != "v1 patched" {
		t.Errorf("last-moment change: %v, file %q", err, read(t, path))
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 {
		t.Errorf("leftovers: %v", got)
	}
}

// An edit after the last check that keeps the old file's size and mtime is
// caught by its content once the old file is out of the way.
func TestFetchKeepsALastMomentChangeWithTheSameStamp(t *testing.T) {
	srv := serve(t, "v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeReplace = func() {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		g, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.WriteAt([]byte("V1"), 0); err != nil {
			t.Error(err)
		}
		g.Close()
		if err := os.Chtimes(path, time.Time{}, info.ModTime()); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeReplace = func() {} })
	_, err = Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil)
	if err == nil || !strings.Contains(err.Error(), "changed while the new version was downloaded") || read(t, path) != "V1" {
		t.Errorf("same-stamp change: %v, file %q", err, read(t, path))
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 1 {
		t.Errorf("leftovers: %v", got)
	}
}

// A redirect away from https is refused, so the pinned URL's transport
// security can't be downgraded on the way.
func TestFetchRefusesRedirectToHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("v2"))
	}))
	t.Cleanup(plain.Close)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = HTTPSOnly
	path := filepath.Join(t.TempDir(), "a.AppImage")
	_, err := Fetch(context.Background(), client, srv.URL, digest("v2"), path, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not https") || read(t, path) != "<missing>" {
		t.Errorf("redirect to http: %v, file %q", err, read(t, path))
	}
}

// When swapping a last-moment change back fails, the new version stays at
// the path and the changed file at the .swap name, which neither Fetch's
// cleanup nor Leftovers removes.
func TestFetchKeepsAChangeItCannotSwapBack(t *testing.T) {
	srv := serve(t, "v2")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v1")
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeReplace = func() {
		write(t, path, "v1 patched")
		if err := os.Chtimes(path, time.Time{}, time.Now().Add(time.Hour)); err != nil {
			t.Error(err)
		}
	}
	swapBack = func(a, b string) error { return syscall.EIO }
	t.Cleanup(func() { beforeReplace, swapBack = func() {}, exchange })

	_, err = Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil)
	if err == nil || !strings.Contains(err.Error(), syscall.EIO.Error()) {
		t.Fatalf("Fetch: %v", err)
	}
	if read(t, path) != "v2" {
		t.Errorf("path holds %q, want the new version", read(t, path))
	}
	swaps, _ := Swaps(path)
	if len(swaps) != 1 || read(t, swaps[0]) != "v1 patched" || !strings.Contains(err.Error(), swaps[0]) {
		t.Fatalf("changed file kept at %v (%v)", swaps, err)
	}
	if l, err := Leftovers(path); err != nil || len(l) != 0 {
		t.Errorf("Leftovers = %v, %v; the kept file must not be one", l, err)
	}
	if got := entries(t, filepath.Dir(path)); len(got) != 2 {
		t.Errorf("entries: %v, want the AppImage and the kept file", got)
	}
}

// Where the kernel or filesystem can't exchange two files, an update is
// refused rather than renamed over a file that may have changed.
func TestFetchRefusesAnUpdateItCannotSwap(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSYS, syscall.EINVAL} {
		srv := serve(t, "v2")
		path := filepath.Join(t.TempDir(), "a.AppImage")
		write(t, path, "v1")
		f, err := Sum(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		swapIn = func(a, b string) error { return errno }
		t.Cleanup(func() { swapIn = exchange })

		_, err = Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, &f, nil)
		if err == nil || !strings.Contains(err.Error(), "can't swap two files atomically") {
			t.Errorf("%v: Fetch = %v", errno, err)
		}
		if read(t, path) != "v1" {
			t.Errorf("%v: path holds %q", errno, read(t, path))
		}
		if got := entries(t, filepath.Dir(path)); len(got) != 1 {
			t.Errorf("%v: leftovers: %v", errno, got)
		}
	}
}

// A crash during an update can leave the old file at the .swap name; a
// later download leaves it for the user.
func TestSwapsSurviveAFetch(t *testing.T) {
	srv := serve(t, "v3")
	path := filepath.Join(t.TempDir(), "a.AppImage")
	write(t, path, "v2")
	swap := filepath.Join(filepath.Dir(path), ".a.AppImage.123.swap")
	write(t, swap, "v1 patched")
	f, err := Sum(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v3"), path, &f, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "v3" || read(t, swap) != "v1 patched" {
		t.Errorf("path = %q, swap = %q", read(t, path), read(t, swap))
	}
	if got, err := Swaps(path); err != nil || len(got) != 1 || got[0] != swap {
		t.Errorf("Swaps = %v, %v", got, err)
	}
}

// Beside a path whose directory is a file there is nothing to find.
func TestNamedBelowAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "Applications")
	write(t, file, "not a directory")
	path := filepath.Join(file, "a.AppImage")
	if got, err := Swaps(path); err != nil || len(got) != 0 {
		t.Errorf("Swaps = %v, %v", got, err)
	}
	if got, err := Leftovers(path); err != nil || len(got) != 0 {
		t.Errorf("Leftovers = %v, %v", got, err)
	}
}

// Temp files a killed download left are found, and the next download
// removes them.
func TestLeftovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.AppImage")
	for _, name := range []string{".a.AppImage.123.tmp", ".a.AppImage.x.tmp", "123.tmp", ".b.AppImage.1.tmp", ".a.AppImage..tmp"} {
		write(t, filepath.Join(dir, name), "partial")
	}
	other := filepath.Join(dir, ".a.AppImage.456.tmp")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Leftovers(path)
	if err != nil || len(got) != 1 || got[0] != filepath.Join(dir, ".a.AppImage.123.tmp") {
		t.Fatalf("Leftovers = %v, %v", got, err)
	}
	srv := serve(t, "v2")
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, digest("v2"), path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := Leftovers(path); len(got) != 0 {
		t.Errorf("left %v", got)
	}
	if info, err := os.Stat(other); err != nil || !info.IsDir() {
		t.Errorf("unrelated directory was removed: %v", err)
	}
}
