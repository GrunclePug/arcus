package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releaseServer serves a latest release tag with an arcus binary and its
// sha256sums.txt; sums overrides the checksum file when set.
func releaseServer(t *testing.T, tag, bin, sums string) *httptest.Server {
	t.Helper()
	if sums == "" {
		h := sha256.Sum256([]byte(bin))
		sums = hex.EncodeToString(h[:]) + "  arcus\n" + strings.Repeat("0", 64) + "  arcus-install\n"
	}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q, "assets": [
			{"name": "arcus", "browser_download_url": "%s/arcus"},
			{"name": "sha256sums.txt", "browser_download_url": "%s/sums"}]}`, tag, srv.URL, srv.URL)
	})
	mux.HandleFunc("GET /arcus", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, bin) })
	mux.HandleFunc("GET /sums", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sums) })
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// updateEnv is an env whose running arcus is version at a writable path.
func updateEnv(t *testing.T, version string, srv *httptest.Server) *env {
	e := newEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	e.exe = filepath.Join(e.root, "usr/local/bin/arcus")
	must(t, os.MkdirAll(filepath.Dir(e.exe), 0o755))
	must(t, os.WriteFile(e.exe, []byte("old"), 0o755))
	e.version = version
	if srv != nil {
		e.releases = srv.URL + "/latest"
	}
	return e
}

func TestUpdate(t *testing.T) {
	tests := []struct {
		name, version, tag, sums string
		args                     []string
		want                     string // the binary afterwards
		out, err                 string
	}{
		{name: "newer", version: "0.1.0", tag: "v0.2.0", args: []string{"-y"}, want: "new", out: "Updated to arcus 0.2.0."},
		{name: "minor beats patch", version: "0.1.9", tag: "v0.2.0", args: []string{"-y"}, want: "new", out: "Updated"},
		{name: "up to date", version: "0.2.0", tag: "v0.2.0", args: []string{"-y"}, want: "old", out: "arcus 0.2.0 is up to date."},
		{name: "ahead of the release", version: "0.3.0", tag: "v0.2.0", args: []string{"-y"}, want: "old", out: "up to date"},
		{name: "plan only", version: "0.1.0", tag: "v0.2.0", args: []string{"-n"}, want: "old", out: "from 0.1.0 to 0.2.0"},
		{name: "declined", version: "0.1.0", tag: "v0.2.0", want: "old", err: "aborted"},
		{name: "dev build", version: "dev", tag: "v0.2.0", args: []string{"-y"}, want: "old", err: "update it from its clone"},
		{name: "bad checksum", version: "0.1.0", tag: "v0.2.0", sums: strings.Repeat("a", 64) + "  arcus\n", args: []string{"-y"}, want: "old", err: "doesn't match"},
		{name: "no checksum", version: "0.1.0", tag: "v0.2.0", sums: "\n", args: []string{"-y"}, want: "old", err: "lists no arcus"},
		{name: "odd tag", version: "0.1.0", tag: "latest", args: []string{"-y"}, want: "old", err: "unexpected tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := updateEnv(t, tt.version, releaseServer(t, tt.tag, "new", tt.sums))
			e.out.Reset()
			err := e.app("n\n").run(append([]string{"update"}, tt.args...))
			switch {
			case tt.err == "" && err != nil:
				t.Fatalf("update: %v\n%s", err, e.out.String())
			case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
				t.Fatalf("update: got %v, want %q", err, tt.err)
			}
			if !strings.Contains(e.out.String(), tt.out) {
				t.Errorf("output %q lacks %q", e.out.String(), tt.out)
			}
			if got := e.read("usr/local/bin/arcus"); got != tt.want {
				t.Errorf("binary = %q, want %q", got, tt.want)
			}
			if e.sudos != 0 {
				t.Errorf("sudo ran %d times for a writable directory", e.sudos)
			}
		})
	}
}

// A binary in a directory the user can't write goes through exec-system,
// which installs the file only while it has the verified hash.
func TestUpdateThroughSudo(t *testing.T) {
	e := updateEnv(t, "0.1.0", releaseServer(t, "v0.2.0", "new", ""))
	dir := filepath.Dir(e.exe)
	must(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	a := e.app("")
	var got []string
	a.sudo = func(args []string) error {
		got = args
		must(t, os.Chmod(dir, 0o755)) // root can write there
		return e.app("").sudo(args)
	}
	must(t, a.run([]string{"update", "-y"}))
	if len(got) != 4 || got[0] != "-update" || got[1] != "-hash" {
		t.Fatalf("sudo args = %q", got)
	}
	if b := e.read("usr/local/bin/arcus"); b != "new" {
		t.Errorf("binary = %q, want new", b)
	}

	// The root side refuses a file whose content changed after the check.
	file := filepath.Join(t.TempDir(), "arcus")
	must(t, os.WriteFile(file, []byte("swapped"), 0o644))
	if err := e.app("").sudo([]string{"-update", "-hash", got[2], file}); err == nil || !strings.Contains(err.Error(), "changed after it was verified") {
		t.Errorf("exec-system -update with a changed file: %v", err)
	}
	if b := e.read("usr/local/bin/arcus"); b != "new" {
		t.Errorf("binary = %q after a refused update", b)
	}
}

// doctor notes a newer release and stays quiet when GitHub can't be reached.
func TestDoctorVersion(t *testing.T) {
	srv := releaseServer(t, "v0.2.0", "new", "")
	e := updateEnv(t, "0.1.0", srv)
	e.wlBase()
	must(t, e.run("on", "-y", "wl-base"))
	must(t, e.run("doctor"))
	if !strings.Contains(e.out.String(), "arcus 0.2.0 is out; this is 0.1.0") {
		t.Errorf("doctor output lacks the release note:\n%s", e.out.String())
	}
	if !strings.Contains(e.out.String(), "No problems found.") {
		t.Errorf("a newer release counted as a problem:\n%s", e.out.String())
	}

	srv.Close()
	must(t, e.run("doctor"))
	if strings.Contains(e.out.String(), "is out") {
		t.Errorf("doctor offline:\n%s", e.out.String())
	}
}
