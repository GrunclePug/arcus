package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	iofs "io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grunclepug/arcus/internal/state"
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
	e.client = r.Client()
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

const appPath = "home/u/Applications/app.AppImage"

func (e *env) appMode() iofs.FileMode {
	e.t.Helper()
	info, err := os.Lstat(filepath.Join(e.root, appPath))
	must(e.t, err)
	return info.Mode()
}

// A pinned AppImage: installed executable, updated when the pin moves,
// replaced (the changed file set aside) when edited, removed on off only
// while unchanged.
func TestPinnedAppImage(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/v1/app": "app v1", "/v2/app": "app v2"})
	e.appImageModule(r, "/v1/app", "app v1", "")

	must(t, e.run("plan", "desk"))
	for _, want := range []string{"user  install AppImage for desk: Applications/app.AppImage", "from " + r.URL + "/v1/app"} {
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
	rec := e.state(false).AppImage("Applications/app.AppImage")
	if rec == nil || rec.SHA256 != sha("app v1") || rec.SelfUpdate || e.state(false).Owner(state.HomeAppImages, "Applications/app.AppImage") != "desk" {
		t.Fatalf("record = %+v", rec)
	}
	e.plans("Nothing to do.")

	// The module moves the pin: the unchanged file is replaced, not kept.
	e.appImageModule(r, "/v2/app", "app v2", "")
	e.plans("update AppImage for desk to the pinned version: Applications/app.AppImage", "from "+r.URL+"/v2/app")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v2" {
		t.Fatalf("after the pin moved: %q", e.read(appPath))
	}
	if _, err := os.Lstat(filepath.Join(e.root, appPath+".arcus-foreign")); !errors.Is(err, iofs.ErrNotExist) {
		t.Error("arcus's own unchanged file must not be set aside")
	}

	// Edited by hand: doctor reports it, apply sets it aside and installs the pin.
	must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("patched"), 0o755))
	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), "AppImage ~/Applications/app.AppImage of desk doesn't match its pinned sha256") {
		t.Errorf("doctor on an edited AppImage (%v):\n%s", err, e.out.String())
	}
	e.plans("replace AppImage for desk with the pinned version: Applications/app.AppImage", "! Applications/app.AppImage: not the pinned version")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v2" || e.read(appPath+".arcus-foreign") != "patched" {
		t.Fatalf("file %q, set aside %q", e.read(appPath), e.read(appPath+".arcus-foreign"))
	}

	// Removed by hand: doctor reports it, apply downloads it again.
	must(t, os.Remove(filepath.Join(e.root, appPath)))
	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), "AppImage ~/Applications/app.AppImage of desk is missing") {
		t.Errorf("doctor on a missing AppImage (%v):\n%s", err, e.out.String())
	}
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v2" {
		t.Fatal("a missing AppImage was not installed again")
	}
	must(t, e.run("doctor"))

	// Off removes it while it is unchanged.
	must(t, e.run("off", "-y", "desk"))
	if e.read(appPath) != "<missing>" {
		t.Error("off left the unchanged AppImage")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}

	// An edited one stays, and is forgotten.
	must(t, e.run("on", "-y", "desk"))
	must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("mine now"), 0o755))
	must(t, e.run("off", "-n", "desk"))
	if want := "= Applications/app.AppImage: changed since arcus installed it, so it stays"; !strings.Contains(e.out.String(), want) {
		t.Errorf("plan lacks %q:\n%s", want, e.out.String())
	}
	must(t, e.run("off", "-y", "desk"))
	if e.read(appPath) != "mine now" {
		t.Error("off removed an edited AppImage")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}
}

// doctor -fix must not count a missing AppImage as fixed.
func TestDoctorFixLeavesAppImagesToApply(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")
	must(t, e.run("on", "-y", "desk"))
	must(t, os.Remove(filepath.Join(e.root, appPath)))
	if err := e.run("doctor", "-fix"); err == nil {
		t.Errorf("doctor -fix claims it fixed a missing AppImage:\n%s", e.out.String())
	}
	if e.read(appPath) != "<missing>" {
		t.Error("doctor -fix downloaded")
	}
}

// A self-updating AppImage is adopted as it is, never downloaded while it
// is there, and removed on off even after the app replaced it.
func TestSelfUpdatingAppImage(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", `path = "Applications/T3.AppImage"`+"\nself_update = true")
	t3 := "home/u/Applications/T3.AppImage"
	must(t, os.MkdirAll(filepath.Join(e.home, "Applications"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.root, t3), []byte("app v3, updated by itself"), 0o755))

	must(t, e.run("plan", "desk"))
	for _, want := range []string{"take over AppImages for desk (already there): Applications/T3.AppImage", "= Applications/T3.AppImage: not the pinned version; self_update keeps it as it is"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, e.out.String())
		}
	}
	must(t, e.run("on", "-y", "desk"))
	if e.read(t3) != "app v3, updated by itself" || r.downloads() != 0 {
		t.Fatalf("file %q after %d downloads", e.read(t3), r.downloads())
	}
	if rec := e.state(false).AppImage("Applications/T3.AppImage"); rec == nil || !rec.SelfUpdate || rec.SHA256 != sha("app v3, updated by itself") {
		t.Fatalf("record = %+v", rec)
	}

	// The app updates itself: nothing to do, doctor is content.
	must(t, os.WriteFile(filepath.Join(e.root, t3), []byte("app v4"), 0o755))
	e.plans("Nothing to do.")
	must(t, e.run("doctor"))

	// Gone: installed from the pin again.
	must(t, os.Remove(filepath.Join(e.root, t3)))
	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), "AppImage ~/Applications/T3.AppImage of desk is missing") {
		t.Errorf("doctor on a missing AppImage (%v):\n%s", err, e.out.String())
	}
	must(t, e.run("apply", "-y"))
	if e.read(t3) != "app v1" || r.downloads() != 1 {
		t.Fatalf("file %q after %d downloads", e.read(t3), r.downloads())
	}

	must(t, os.WriteFile(filepath.Join(e.root, t3), []byte("app v5"), 0o755))
	must(t, e.run("off", "-y", "desk"))
	if e.read(t3) != "<missing>" {
		t.Error("off left a self-updating AppImage")
	}
	if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
		t.Errorf("records left: %+v", st)
	}
}

// A file already there is taken over when it is the pinned one; a
// different one, without self_update, is set aside for the pinned one.
func TestAppImageAlreadyThere(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", "")
	must(t, os.MkdirAll(filepath.Join(e.home, "Applications"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("app v1"), 0o700))

	must(t, e.run("on", "-y", "desk"))
	if r.downloads() != 0 || e.state(false).AppImage("Applications/app.AppImage") == nil {
		t.Fatalf("the pinned file was not taken over (%d downloads)", r.downloads())
	}
	must(t, e.run("off", "-y", "desk"))

	must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("hand-placed v0"), 0o755))
	must(t, e.run("on", "-y", "desk"))
	if e.read(appPath) != "app v1" || e.read(appPath+".arcus-foreign") != "hand-placed v0" {
		t.Fatalf("file %q, set aside %q", e.read(appPath), e.read(appPath+".arcus-foreign"))
	}
}

// A download that fails or doesn't match its pin fails the command and
// leaves nothing behind: no file, no temp file, no new directory, no
// record.
func TestAppImageDownloadFails(t *testing.T) {
	for _, tt := range []struct {
		name, served string
		closed       bool
		want         []string
	}{
		{"tampered", "evil", false, []string{"doesn't match the pinned sha256", "expected " + sha("app"), "got " + sha("evil")}},
		{"offline", "app", true, []string{"downloading https://", "nothing was installed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			r := e.serveAppImages(map[string]string{"/app": tt.served})
			e.appImageModule(r, "/app", "app", "")
			if tt.closed {
				r.Close()
			}
			before := e.snapshot()
			err := e.run("on", "-y", "desk")
			for _, want := range append(tt.want, r.URL+"/app") {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error %v lacks %q", err, want)
				}
			}
			assertSame(t, before, e.snapshot())
			if st := e.state(false); len(st.AppImages) != 0 || len(st.Owned) != 0 {
				t.Errorf("records: %+v", st)
			}
			if len(e.hostModules()) != 0 {
				t.Error("a failed on must not record the module")
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
	a.stdin = &confirmAfter{change: func() {
		must(t, os.MkdirAll(filepath.Join(e.home, "Applications"), 0o755))
		must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("mine"), 0o755))
	}}
	err := a.run([]string{"on", "desk"})
	if err == nil || !strings.Contains(err.Error(), "~/Applications/app.AppImage changed since the plan was shown") {
		t.Fatalf("got %v", err)
	}
	if e.read(appPath) != "mine" || r.downloads() != 0 {
		t.Errorf("file %q after %d downloads", e.read(appPath), r.downloads())
	}
}

// Turning self_update on or off only updates the record while the file is
// the pinned one; with it on, the app's own updates are left alone.
func TestAppImageSelfUpdateToggled(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")
	must(t, e.run("on", "-y", "desk"))

	e.appImageModule(r, "/app", "app", "self_update = true")
	e.plans("update the record of AppImage for desk: Applications/app.AppImage", "self_update = true")
	must(t, e.run("apply", "-y"))
	if rec := e.state(false).AppImage("Applications/app.AppImage"); rec == nil || !rec.SelfUpdate {
		t.Fatalf("record = %+v", rec)
	}
	must(t, os.WriteFile(filepath.Join(e.root, appPath), []byte("app, updated by itself"), 0o755))
	e.plans("Nothing to do.")

	// Off again: the app's version is set aside for the pinned one.
	e.appImageModule(r, "/app", "app", "self_update = false")
	e.plans("replace AppImage for desk with the pinned version: Applications/app.AppImage")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app" || e.read(appPath+".arcus-foreign") != "app, updated by itself" || r.downloads() != 2 {
		t.Errorf("file %q, set aside %q, %d downloads", e.read(appPath), e.read(appPath+".arcus-foreign"), r.downloads())
	}
}

// doctor reports the temp file a killed download left, and -fix removes it.
func TestDoctorRemovesDownloadLeftovers(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app"})
	e.appImageModule(r, "/app", "app", "")
	must(t, e.run("on", "-y", "desk"))
	leftover := filepath.Join(e.home, "Applications", ".app.AppImage.12345.tmp")
	must(t, os.WriteFile(leftover, []byte("partial"), 0o600))

	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), leftover+" was left by an interrupted download") {
		t.Fatalf("doctor (%v):\n%s", err, e.out.String())
	}
	must(t, e.run("doctor", "-fix"))
	if _, err := os.Lstat(leftover); !errors.Is(err, iofs.ErrNotExist) {
		t.Error("doctor -fix left the temp file")
	}
	must(t, e.run("doctor"))
}

// An update interrupted mid-swap can leave the old file at a .swap name:
// doctor reports it, and neither -fix nor the next download removes it.
func TestDoctorReportsAnInterruptedSwap(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/v1/app": "app v1", "/v2/app": "app v2"})
	e.appImageModule(r, "/v1/app", "app v1", "")
	must(t, e.run("on", "-y", "desk"))
	swap := filepath.Join(e.home, "Applications", ".app.AppImage.12345.swap")
	must(t, os.WriteFile(swap, []byte("old, edited"), 0o755))

	for _, args := range [][]string{{"doctor"}, {"doctor", "-fix"}} {
		if err := e.run(args...); err == nil || !strings.Contains(e.out.String(), swap+" was left by an interrupted update") {
			t.Errorf("%v (%v):\n%s", args, err, e.out.String())
		}
	}
	e.appImageModule(r, "/v2/app", "app v2", "")
	must(t, e.run("apply", "-y"))
	if e.read(appPath) != "app v2" {
		t.Errorf("AppImage = %q", e.read(appPath))
	}
	if data, err := os.ReadFile(swap); err != nil || string(data) != "old, edited" {
		t.Errorf("swap file = %q, %v", data, err)
	}
}

func TestAppImageReplaceLeavesFileChangedDuringDownload(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.root, appPath)
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte("hand-placed"), 0o755))

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(path, []byte("edited during download"), 0o755); err != nil {
			t.Error(err)
			http.Error(w, "could not edit test file", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("pinned"))
	}))
	t.Cleanup(srv.Close)
	e.client = srv.Client()
	e.appImageModule(&appImageServer{Server: srv}, "/app", "pinned", "")
	if err := e.run("on", "-y", "desk"); err == nil || !strings.Contains(err.Error(), "changed while the new version was downloaded") {
		t.Errorf("replacement should refuse a file changed during download: %v", err)
	}
	if got := e.read(appPath); got != "edited during download" {
		t.Errorf("AppImage = %q, want the concurrent edit", got)
	}
	if got := e.read("home/u/.local/state/arcus/foreign/Applications/app.AppImage"); got != "<missing>" {
		t.Errorf("concurrent edit was moved to foreign/: %q", got)
	}
}

// ~/Applications replaced with a symlink into a dotfiles checkout: neither
// off nor doctor -fix follows it to remove what is there.
func TestAppImageBelowASymlinkedParentIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", "self_update = true")
	must(t, e.run("on", "-y", "desk"))
	apps := filepath.Join(e.home, "Applications")
	checkout := filepath.Join(e.root, "checkout")
	must(t, os.Rename(apps, checkout))
	must(t, os.Symlink(checkout, apps))
	leftover := filepath.Join(checkout, ".app.AppImage.12345.tmp")
	must(t, os.WriteFile(leftover, []byte("not arcus's"), 0o600))

	if err := e.run("doctor", "-fix"); err == nil || !strings.Contains(e.out.String(), "parent "+apps+" is a symlink") {
		t.Errorf("doctor -fix (%v):\n%s", err, e.out.String())
	}
	if _, err := os.Lstat(leftover); err != nil {
		t.Errorf("doctor -fix removed %s through the symlink: %v", leftover, err)
	}

	must(t, e.run("off", "-y", "desk"))
	if !strings.Contains(e.out.String(), "parent "+apps+" is a symlink, so it stays") {
		t.Errorf("off output:\n%s", e.out.String())
	}
	if got, err := os.ReadFile(filepath.Join(checkout, "app.AppImage")); err != nil || string(got) != "app v1" {
		t.Errorf("off removed the file through the symlink: %q, %v", got, err)
	}
}

// Turning an AppImage off doesn't remove the .swap file an interrupted
// update left beside it; doctor still reports it.
func TestDoctorReportsASwapAfterOff(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", "")
	must(t, e.run("on", "-y", "desk"))
	swap := filepath.Join(e.home, "Applications", ".app.AppImage.12345.swap")
	must(t, os.WriteFile(swap, []byte("old, edited"), 0o755))
	must(t, e.run("off", "-y", "desk"))

	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), swap+" was left by an interrupted update") {
		t.Errorf("doctor (%v):\n%s", err, e.out.String())
	}
}

// An AppImage declared by a module that is off, below a path that is a
// file, has no .swap files beside it: doctor passes.
func TestDoctorSwapAuditBelowAFile(t *testing.T) {
	e := newEnv(t)
	r := e.serveAppImages(map[string]string{"/app": "app v1"})
	e.appImageModule(r, "/app", "app v1", "")
	e.module("base", "", nil)
	must(t, e.run("on", "-y", "base"))
	must(t, os.WriteFile(filepath.Join(e.home, "Applications"), []byte("a file"), 0o644))

	if err := e.run("doctor"); err != nil {
		t.Errorf("doctor (%v):\n%s", err, e.out.String())
	}
}
