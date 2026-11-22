package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	iofs "io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grunclepug/arcus/internal/appimage"
)

// appImageServer is an https server for AppImages: each path serves its bytes,
// and it counts the downloads.
type appImageServer struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string]string
	gets  int
}

func (e *env) serveAppImages(files map[string]string) *appImageServer {
	r := &appImageServer{files: files}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.gets++
		body, ok := r.files[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	e.t.Cleanup(r.Close)
	saved := appimage.Client
	appimage.Client = r.Client()
	e.t.Cleanup(func() { appimage.Client = saved })
	return r
}

func (r *appImageServer) downloads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// appImageModule writes module desk with one AppImage, app, served from
// rel at the given content's digest.
func (e *env) appImageModule(r *appImageServer, rel, content, extra string) {
	e.t.Helper()
	e.module("desk", fmt.Sprintf("[appimages.app]\nurl = %q\nsha256 = %q\n%s\n", r.URL+rel, sha(content), extra), nil)
}

func (e *env) plans(want ...string) {
	e.t.Helper()
	must(e.t, e.run("plan"))
	for _, w := range want {
		if !strings.Contains(e.out.String(), w) {
			e.t.Errorf("plan lacks %q:\n%s", w, e.out.String())
		}
	}
}

const (
	appRel  = "Applications/app.AppImage"
	appPath = "home/u/" + appRel
)

func (e *env) appMode() iofs.FileMode {
	e.t.Helper()
	info, err := os.Lstat(filepath.Join(e.root, appPath))
	must(e.t, err)
	return info.Mode()
}

func (e *env) writeApp(content string, mode os.FileMode) {
	e.t.Helper()
	must(e.t, os.MkdirAll(filepath.Join(e.home, "Applications"), 0o755))
	must(e.t, os.WriteFile(filepath.Join(e.root, appPath), []byte(content), mode))
	must(e.t, os.Chmod(filepath.Join(e.root, appPath), mode))
}

// A pinned AppImage: installed executable, updated when the pin moves while
// it is still what arcus installed, left alone once it changed, and removed
// on off only while unchanged.
func TestPinnedAppImage(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/v1/app": "app v1", "/v2/app": "app v2", "/v3/app": "app v3"})
	e.appImageModule(r, "/v1/app", "app v1", "")

	must(t, e.run("plan", "desk"))
	for _, want := range []string{"user  download the pinned AppImage for desk: " + appRel, "from " + r.URL + "/v1/app"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, e.out.String())
		}
	}
	must(t, e.run("on", "-y", "desk"))
	if e.read(appPath) != "app v1" || e.appMode() != 0o755 {
		t.Fatalf("installed %q with mode %v", e.read(appPath), e.appMode())
	}
	if e.sudos != 0 {
		t.Errorf("sudo ran %d times for an AppImage", e.sudos)
	}
	if got := e.state(false).AppImages[appRel]; got != sha("app v1") {
		t.Fatalf("record = %q", got)
	}
	e.plans("Nothing to do.")

	// The pin moves: the unchanged file is replaced.
	e.appImageModule(r, "/v2/app", "app v2", "")
	e.plans("download the pinned AppImage for desk: "+appRel, "from "+r.URL+"/v2/app")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v2" {
		t.Fatalf("after the pin moved: %q", e.read(appPath))
	}

	// The app updates itself: it is the app's now. A pin bump leaves it,
	// with a note, and doctor finds nothing wrong.
	e.writeApp("app v2.1 from the app", 0o755)
	e.appImageModule(r, "/v3/app", "app v3", "")
	downloads := r.downloads()
	e.plans("Nothing to do.", "= ~/"+appRel+": changed since arcus installed it, so it stays; remove it to get the pinned version")
	must(t, e.run("apply", "-y"))
	must(t, e.run("doctor"))
	if e.read(appPath) != "app v2.1 from the app" || r.downloads() != downloads {
		t.Fatalf("a changed AppImage was touched: %q after %d downloads", e.read(appPath), r.downloads()-downloads)
	}

	// Removing it gets the pin back.
	must(t, os.Remove(filepath.Join(e.root, appPath)))
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v3" {
		t.Fatal("a removed AppImage was not installed again")
	}

	// Off removes it while it is unchanged.
	must(t, e.run("off", "-y", "desk"))
	if e.read(appPath) != "<missing>" {
		t.Error("off left the unchanged AppImage")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}

	// A changed one stays, is named, and is forgotten.
	must(t, e.run("on", "-y", "desk"))
	e.writeApp("mine now", 0o755)
	must(t, e.run("off", "-n", "desk"))
	if want := "= ~/" + appRel + ": changed since arcus installed it, so it stays"; !strings.Contains(e.out.String(), want) {
		t.Errorf("plan lacks %q:\n%s", want, e.out.String())
	}
	must(t, e.run("off", "-y", "desk"))
	if e.read(appPath) != "mine now" {
		t.Error("off removed a changed AppImage")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}
}

// The pinned file already there is taken over without a download, and made
// executable; any other file is refused until moved away.
func TestAppImageAlreadyThere(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")

	e.writeApp("something else", 0o755)
	if err := e.run("on", "-y", "desk"); err == nil || !strings.Contains(err.Error(), "isn't the pinned AppImage: move it away") {
		t.Fatalf("on over another file: %v", err)
	}
	if e.read(appPath) != "something else" {
		t.Fatal("a file arcus didn't install was touched")
	}

	e.writeApp("app", 0o644) // as a browser saves it
	must(t, e.run("on", "-y", "desk"))
	if r.downloads() != 0 || e.appMode() != 0o744 {
		t.Errorf("take-over: %d downloads, mode %v", r.downloads(), e.appMode())
	}
	if e.state(false).AppImages[appRel] != sha("app") {
		t.Error("the taken-over file was not recorded")
	}

	// Made not executable again, it is fixed without a download, and a
	// private file stays private.
	must(t, os.Chmod(filepath.Join(e.root, appPath), 0o600))
	must(t, e.run("apply", "-y"))
	if r.downloads() != 0 || e.appMode() != 0o700 {
		t.Errorf("re-take-over: %d downloads, mode %v", r.downloads(), e.appMode())
	}
}

// A failed download changes nothing: no file, no temp file, no record, and
// the module isn't turned on.
func TestAppImageDownloadFails(t *testing.T) {
	for name, served := range map[string]map[string]string{
		"wrong content": {"/app": "evil"},
		"not found":     {},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			r := e.serveAppImages(served)
			e.appImageModule(r, "/app", "app", "")
			if err := e.run("on", "-y", "desk"); err == nil {
				t.Fatal("on succeeded")
			}
			for _, p := range []string{appPath, appPath + ".arcus-tmp"} {
				if _, err := os.Lstat(filepath.Join(e.root, p)); err == nil {
					t.Errorf("%s left", p)
				}
			}
			if len(e.state(false).AppImages) != 0 || len(e.hostModules()) != 0 {
				t.Error("a failed download was recorded")
			}
		})
	}
}

// confirmAfter answers the confirmation with yes, after running change: what
// someone does to the system while the plan is on screen.
type confirmAfter struct {
	change func()
	answer *strings.Reader
}

func (c *confirmAfter) Read(p []byte) (int, error) {
	if c.answer == nil {
		c.change()
		c.answer = strings.NewReader("y\n")
	}
	return c.answer.Read(p)
}

// A file that turns up between the plan and the confirmation is left
// alone: the step no longer matches what was confirmed.
func TestAppImageChangedAfterThePlan(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")
	a := e.app("")
	a.stdin = &confirmAfter{change: func() { e.writeApp("mine", 0o755) }}
	err := a.run([]string{"on", "desk"})
	if err == nil || !strings.Contains(err.Error(), "isn't the pinned AppImage") {
		t.Fatalf("got %v", err)
	}
	if e.read(appPath) != "mine" || r.downloads() != 0 {
		t.Errorf("file %q after %d downloads", e.read(appPath), r.downloads())
	}
}

// Off never removes through a symlinked parent: the file stays, named.
func TestAppImageBelowASymlinkedParentIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", "")
	must(t, e.run("on", "-y", "desk"))
	apps := filepath.Join(e.home, "Applications")
	checkout := filepath.Join(e.root, "checkout")
	must(t, os.Rename(apps, checkout))
	must(t, os.Symlink(checkout, apps))

	must(t, e.run("off", "-y", "desk"))
	if !strings.Contains(e.out.String(), "parent "+apps+" is a symlink, so it stays") {
		t.Errorf("off output:\n%s", e.out.String())
	}
	if got, err := os.ReadFile(filepath.Join(checkout, "app.AppImage")); err != nil || string(got) != "app v1" {
		t.Errorf("off removed the file through the symlink: %q, %v", got, err)
	}
}

// A home link above the AppImage is refused at plan time, before the link
// is made: the download would land in the link's target.
func TestAppImageBelowALinkOfTheSameRun(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	target := filepath.Join(e.home, "Sync", "apps")
	must(t, os.MkdirAll(target, 0o755))
	e.module("desk", fmt.Sprintf("links = { \"Applications\" = \"Sync/apps\" }\n[appimages.app]\nurl = %q\nsha256 = %q\n", r.URL+"/app", sha("app")), nil)
	if err := e.run("on", "-y", "desk"); err == nil || !strings.Contains(err.Error(), "module desk links ~/Applications") {
		t.Fatalf("on: %v", err)
	}
	if r.downloads() != 0 {
		t.Error("downloaded into the link's target")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the link's target got %v", entries)
	}
}

// A home link replaced by an AppImage at the same path in one apply: the
// link goes first, so the path counts as free.
func TestAppImageReplacesAHomeLink(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	must(t, os.MkdirAll(filepath.Join(e.home, "Sync"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.home, "Sync", "app"), []byte("old"), 0o755))
	e.module("desk", "links = { \"Applications/app.AppImage\" = \"Sync/app\" }\n", nil)
	must(t, e.run("on", "-y", "desk"))
	e.appImageModule(r, "/app", "app", "")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app" || e.appMode()&os.ModeSymlink != 0 {
		t.Errorf("after apply: %q, mode %v", e.read(appPath), e.appMode())
	}
}

// Off never stops on a file it can't remove: it says so, keeps the file,
// and forgets the record.
func TestAppImageOffWhenTheRemoveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes regardless of the directory's mode")
	}
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")
	must(t, e.run("on", "-y", "desk"))
	apps := filepath.Join(e.home, "Applications")
	must(t, os.Chmod(apps, 0o555))
	t.Cleanup(func() { os.Chmod(apps, 0o755) })

	must(t, e.run("off", "-y", "desk"))
	if e.read(appPath) != "app" {
		t.Error("the file should still be there")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}
}
