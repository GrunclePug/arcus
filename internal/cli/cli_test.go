package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system/fake"
)

// env is a sandboxed machine: a root prefix with /etc, a $HOME inside it,
// fake system tools on PATH and an in-process sudo that runs exec-system as
// "root". Nothing outside t.TempDir() is touched.
type env struct {
	t     *testing.T
	sys   *fake.System
	root  string
	home  string
	out   bytes.Buffer
	sudos int
	ctx   context.Context
	// What update sees: the running binary, its version and where the
	// latest release is described (empty: doctor doesn't look).
	exe      string
	version  string
	releases string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, sys: fake.New(t), root: t.TempDir(), ctx: context.Background()}
	e.home = filepath.Join(e.root, "home", "u")
	for _, dir := range []string{e.home, filepath.Join(e.root, "etc")} {
		must(t, os.MkdirAll(dir, 0o755))
	}
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) app(stdin string) *app {
	a := &app{
		ctx: e.ctx, stdin: strings.NewReader(stdin), stdout: &e.out, stderr: &e.out,
		root: e.root, euid: 1000, getenv: os.Getenv,
		hostname: func() (string, error) { return "testhost", nil },
		exe:      func() (string, error) { return e.exe, nil },
		version:  e.version, releases: e.releases,
	}
	a.sudo = func(args []string) error {
		e.sudos++
		r := &app{
			ctx: a.ctx, stdin: a.stdin, stdout: a.stdout, stderr: a.stderr, root: e.root, euid: 0,
			getenv: func(k string) string {
				if k == "SUDO_USER" {
					return "u"
				}
				return ""
			},
			hostname: a.hostname, exe: a.exe, version: a.version,
		}
		return r.run(append([]string{"exec-system"}, args...))
	}
	return a
}

func (e *env) run(args ...string) error {
	e.t.Helper()
	e.out.Reset()
	return e.app("").run(args)
}

// module writes a user module: its manifest and files keyed by
// "home/<rel>" or "system/<rel>".
func (e *env) module(name, manifest string, files map[string]string) string {
	e.t.Helper()
	dir := filepath.Join(e.home, ".config", "arcus", "modules", name)
	must(e.t, os.MkdirAll(dir, 0o755))
	must(e.t, os.WriteFile(filepath.Join(dir, "module.toml"), []byte(manifest), 0o644))
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		must(e.t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(e.t, os.WriteFile(p, []byte(content), 0o644))
	}
	return dir
}

// hostPath returns the host file's path, creating its directory.
func (e *env) hostPath() string {
	e.t.Helper()
	path := filepath.Join(e.home, ".local", "state", "arcus", "host.toml")
	must(e.t, os.MkdirAll(filepath.Dir(path), 0o755))
	return path
}

func (e *env) hostModules() []string {
	e.t.Helper()
	f, err := host.Load(e.hostPath())
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	must(e.t, err)
	return f.Modules
}

func (e *env) state(system bool) *state.State {
	e.t.Helper()
	path := filepath.Join(e.home, ".local", "state", "arcus", "state.json")
	if system {
		path = filepath.Join(e.root, "var", "lib", "arcus", "state.json")
	}
	st, err := state.Load(path)
	must(e.t, err)
	return st
}

// read returns a file below the root prefix, or "<missing>".
func (e *env) read(rel string) string {
	data, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

// snapshot captures /etc and $HOME, minus arcus's own config and state.
func (e *env) snapshot() map[string]string {
	e.t.Helper()
	snap := map[string]string{}
	skip := []string{filepath.Join(e.home, ".config", "arcus"), filepath.Join(e.home, ".local")}
	for _, top := range []string{filepath.Join(e.root, "etc"), e.home} {
		must(e.t, filepath.WalkDir(top, func(p string, d iofs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			for _, s := range skip {
				if strings.HasPrefix(p, s) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, _ := os.Readlink(p)
				snap[p] = "link:" + target
			case info.IsDir():
				snap[p] = "dir"
			default:
				data, _ := os.ReadFile(p)
				snap[p] = fmt.Sprintf("file:%v:%s", info.Mode().Perm(), data)
			}
			return nil
		}))
	}
	return snap
}

func assertSame(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, got[k])
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected leftover %s (%s)", k, v)
		}
	}
}

func contains(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%v does not contain %q", got, w)
		}
	}
}

func lacks(t *testing.T, got []string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if slices.Contains(got, u) {
			t.Errorf("%v contains %q", got, u)
		}
	}
}

// wlBase sets up a base module and a wl-base module that uses every
// resource kind.
func (e *env) wlBase() {
	e.module("base", "[packages]\npacman = [\"base-tools\"]\n", nil)
	e.module("wl-base", `
requires = ["base"]
groups   = ["video"]

[packages]
pacman = ["foot", "wmenu"]
aur    = ["wlr-randr-git"]

[units]
system = ["seatd.service"]
user   = ["mako.service"]
now    = true
`, map[string]string{
		"home/.config/foot/foot.ini": "font=mono",
		"system/etc/wl-base.conf":    "seat=1",
	})
}

func TestOnOffRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.sys.SetPackages("glibc")
	e.sys.SetGroups("wheel")
	e.wlBase()
	must(t, os.WriteFile(filepath.Join(e.home, ".bashrc"), []byte("mine"), 0o644))
	before := e.snapshot()

	must(t, e.run("on", "-y", "wl-base"))
	if got := e.hostModules(); !slices.Equal(got, []string{"wl-base"}) {
		t.Fatalf("host file = %v", got)
	}
	contains(t, e.sys.Packages(), "base-tools", "foot", "wmenu", "wlr-randr-git")
	contains(t, e.sys.Units(false), "seatd.service")
	contains(t, e.sys.Units(true), "mako.service")
	contains(t, e.sys.Groups(), "video")
	if e.read("etc/wl-base.conf") != "seat=1" {
		t.Errorf("system file = %q", e.read("etc/wl-base.conf"))
	}
	if e.read("home/u/.config/foot/foot.ini") != "font=mono" {
		t.Errorf("home file = %q", e.read("home/u/.config/foot/foot.ini"))
	}
	if info, err := os.Lstat(filepath.Join(e.root, "etc/wl-base.conf")); err != nil || !info.Mode().IsRegular() {
		t.Error("system files must be copies")
	}
	// Root stages: packages, system setup. Nothing else asks for sudo.
	if e.sudos != 2 {
		t.Errorf("sudo ran %d times, want 2", e.sudos)
	}
	if !strings.Contains(strings.Join(e.sys.Log(), "\n"), "systemctl enable --now seatd.service") {
		t.Error("units with now = true must be started")
	}

	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after on:\n%s", e.out.String())
	}

	must(t, e.run("off", "-y", "wl-base"))
	if got := e.hostModules(); len(got) != 0 {
		t.Fatalf("host file = %v", got)
	}
	if got := e.sys.Packages(); !slices.Equal(got, []string{"glibc"}) {
		t.Errorf("packages = %v, want only what was there before", got)
	}
	if got := e.sys.Groups(); !slices.Equal(got, []string{"wheel"}) {
		t.Errorf("groups = %v", got)
	}
	if len(e.sys.Units(false))+len(e.sys.Units(true)) != 0 {
		t.Errorf("units left enabled: %v %v", e.sys.Units(false), e.sys.Units(true))
	}
	assertSame(t, before, e.snapshot())
	for _, system := range []bool{false, true} {
		if st := e.state(system); len(st.Modules) != 0 || len(st.Owned) != 0 {
			t.Errorf("state not empty: %+v", st)
		}
	}
}

// Apply installs packages added to a module that's already on,
// and deploys or removes files added to or removed from it.
func TestApplyPicksUpModuleChanges(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	must(t, e.run("on", "-y", "wl-base"))
	dir := filepath.Join(e.home, ".config", "arcus", "modules", "wl-base")

	manifest, err := os.ReadFile(filepath.Join(dir, "module.toml"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "module.toml"),
		[]byte(strings.Replace(string(manifest), `"wmenu"]`, `"wmenu", "mako"]`, 1)), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "home/.config/new.conf"), []byte("new"), 0o644))
	must(t, os.Remove(filepath.Join(dir, "home/.config/foot/foot.ini")))
	must(t, os.WriteFile(filepath.Join(dir, "system/etc/wl-base.conf"), []byte("seat=2"), 0o644))

	must(t, e.run("plan"))
	out := e.out.String()
	for _, want := range []string{"install packages for wl-base: mako", "+ " + filepath.Join(e.home, ".config/new.conf"),
		"- " + filepath.Join(e.home, ".config/foot/foot.ini"), "~ " + filepath.Join(e.root, "etc/wl-base.conf"), "- seat=1", "+ seat=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}

	must(t, e.run("apply", "-y"))
	contains(t, e.sys.Packages(), "mako")
	if e.read("home/u/.config/new.conf") != "new" || e.read("home/u/.config/foot/foot.ini") != "<missing>" || e.read("etc/wl-base.conf") != "seat=2" {
		t.Errorf("files not synced: new=%q foot=%q etc=%q",
			e.read("home/u/.config/new.conf"), e.read("home/u/.config/foot/foot.ini"), e.read("etc/wl-base.conf"))
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after apply:\n%s", e.out.String())
	}

	// Removing a package from the module uninstalls it.
	must(t, os.WriteFile(filepath.Join(dir, "module.toml"), manifest, 0o644))
	must(t, e.run("apply", "-y"))
	lacks(t, e.sys.Packages(), "mako")
}

// What a module lists and was already there is taken over, so off removes
// it like anything arcus installed; only what another module that's on
// lists, or what is protected, stays.
func TestOffKeepsSharedAndProtected(t *testing.T) {
	e := newEnv(t)
	e.sys.SetPackages("foot")
	e.sys.SetUnits(false, "fstrim.timer")
	e.sys.SetGroups("input")
	e.module("a", `
groups = ["input", "video"]
[packages]
pacman = ["foot", "river", "sudo", "only-a"]
[units]
system = ["fstrim.timer", "seatd.service"]
`, nil)
	e.module("b", `
groups = ["video"]
[packages]
pacman = ["river"]
[units]
system = ["seatd.service"]
`, nil)

	must(t, e.run("plan", "a"))
	for _, want := range []string{"take over packages for a (already there): foot", "take over system units for a (already there): fstrim.timer", "take over groups for a (already there): input"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, e.out.String())
		}
	}
	must(t, e.run("on", "-y", "a"))
	must(t, e.run("on", "-y", "b"))
	must(t, e.run("off", "-y", "a"))

	pkgs := e.sys.Packages()
	contains(t, pkgs, "river", "sudo") // shared, protected
	lacks(t, pkgs, "foot", "only-a")   // taken over, installed
	if got := e.sys.Units(false); !slices.Equal(got, []string{"seatd.service"}) {
		t.Errorf("units = %v", got)
	}
	if got := e.sys.Groups(); !slices.Equal(got, []string{"video"}) {
		t.Errorf("groups = %v", got)
	}

	must(t, e.run("off", "-y", "b"))
	if pkgs := e.sys.Packages(); !slices.Equal(pkgs, []string{"sudo"}) {
		t.Errorf("packages = %v", pkgs)
	}
	if units, groups := e.sys.Units(false), e.sys.Groups(); len(units)+len(groups) != 0 {
		t.Errorf("units %v, groups %v", units, groups)
	}
}

func TestRequiresConflictsAndCascade(t *testing.T) {
	e := newEnv(t)
	e.module("wl-base", "", map[string]string{"home/.wl": "x"})
	e.module("river-classic", `requires = ["wl-base"]`, map[string]string{"home/.river": "x"})
	e.module("wm", "requires = [\"wl-base\"]\nconflicts = [\"river-classic\"]", nil)

	must(t, e.run("on", "-y", "river-classic"))
	must(t, e.run("on", "-y", "wm")) // swaps river-classic out
	if got := e.hostModules(); !slices.Equal(got, []string{"wm"}) || e.read("home/u/.river") != "<missing>" {
		t.Fatalf("on wm must swap river-classic out: %v", got)
	}
	must(t, e.run("on", "-y", "river-classic"))

	if err := e.run("off", "-y", "wl-base"); err == nil || !strings.Contains(err.Error(), "use -f") {
		t.Fatalf("off of a requirement must be refused without -f: %v", err)
	}
	must(t, e.run("off", "-f", "-y", "wl-base"))
	if got := e.hostModules(); len(got) != 0 {
		t.Fatalf("cascade left %v", got)
	}
	if e.read("home/u/.river") != "<missing>" || e.read("home/u/.wl") != "<missing>" {
		t.Error("cascade left files behind")
	}
	if err := e.run("off", "-y", "wm"); err == nil || !strings.Contains(err.Error(), "not on") {
		t.Fatalf("off of a module that is off: %v", err)
	}
}

func TestHomeOnlyModuleNeedsNoSudo(t *testing.T) {
	e := newEnv(t)
	e.module("shell", "", map[string]string{"home/.bashrc": "alias ll='ls -l'"})
	must(t, e.run("on", "-y", "shell"))
	must(t, e.run("off", "-y", "shell"))
	if e.sudos != 0 {
		t.Errorf("sudo ran %d times for a module with only home files", e.sudos)
	}
}

func TestPlanAndDryRunChangeNothing(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	before := e.snapshot()
	must(t, e.run("plan", "wl-base"))
	if !strings.Contains(e.out.String(), "install packages for base: base-tools") {
		t.Errorf("plan output:\n%s", e.out.String())
	}
	must(t, e.run("on", "-n", "wl-base"))
	assertSame(t, before, e.snapshot())
	if e.hostModules() != nil || len(e.sys.Packages()) != 0 || e.sudos != 0 {
		t.Error("plan or -n changed something")
	}
	if err := e.run("apply", "-y"); err == nil || !strings.Contains(err.Error(), "no host file") {
		t.Fatalf("apply without a host file must refuse, got %v", err)
	}
}

func TestConfirmationCanCancel(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	err := e.app("n\n").run([]string{"on", "wl-base"})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("got %v", err)
	}
	if e.hostModules() != nil || len(e.sys.Packages()) != 0 {
		t.Error("cancel changed something")
	}
}

// exec-system refuses when not run through sudo and on a plan
// hash mismatch.
func TestExecSystemRefuses(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	a := e.app("")
	if err := a.run([]string{"exec-system", "-stage", strconv.Itoa(int(plan.Packages))}); err == nil || !strings.Contains(err.Error(), "through sudo") {
		t.Fatalf("not root: %v", err)
	}

	a.euid = 0
	if err := a.run([]string{"exec-system", "-stage", strconv.Itoa(int(plan.Packages))}); err == nil || !strings.Contains(err.Error(), "through sudo") {
		t.Fatalf("root without SUDO_USER: %v", err)
	}

	err := a.sudo([]string{"-stage", strconv.Itoa(int(plan.Packages)), "-hash", "0000", "-home", e.home,
		"-config", filepath.Join(e.home, ".config", "arcus"), "--", "base", "wl-base"})
	if err == nil || !strings.Contains(err.Error(), "changed since the plan was shown") {
		t.Fatalf("hash mismatch: %v", err)
	}
	if len(e.sys.Packages()) != 0 {
		t.Error("a refused exec-system installed packages")
	}

	if err := e.app("").run([]string{"exec-system"}); err == nil {
		t.Fatal("exec-system must refuse without sudo")
	}
}

func TestRefusesToRunAsRoot(t *testing.T) {
	e := newEnv(t)
	a := e.app("")
	a.euid = 0
	if err := a.run([]string{"ls"}); err == nil || !strings.Contains(err.Error(), "as root") {
		t.Fatalf("got %v", err)
	}
}

// Ctrl-C during a package transaction lets the package manager finish,
// records what it installed, and stops before the next stage.
func TestInterruptLetsPackageManagerFinish(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	e.sys.Slow("0.3")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.ctx = ctx
	// Interrupt once the transaction is under way, however slow the machine.
	go func() {
		for deadline := time.Now().Add(10 * time.Second); !e.sys.Started() && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()

	err := e.run("on", "-y", "wl-base")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !e.sys.Done() {
		t.Fatal("the package transaction was killed instead of finishing")
	}
	if got := e.state(true).Owned[state.Pacman]["wl-base"]; !slices.Contains(got, "foot") {
		t.Errorf("installed packages not recorded: %v", got)
	}
	if e.read("etc/wl-base.conf") != "<missing>" || e.hostModules() != nil {
		t.Error("stages after the interrupt must not run, and the host file stays unchanged")
	}

	// A later apply of the same change converges.
	e.sys.SetPackages(e.sys.Packages()...)
	must(t, os.Remove(filepath.Join(e.sys.Dir, "slow")))
	e.ctx = context.Background()
	must(t, e.run("on", "-y", "wl-base"))
	must(t, e.run("off", "-y", "wl-base"))
	if got := e.sys.Packages(); len(got) != 0 {
		t.Errorf("packages left after off: %v", got)
	}
}

func TestEditedSystemFileBlocksTeardown(t *testing.T) {
	e := newEnv(t)
	e.module("p", "", map[string]string{"system/etc/p.conf": "AAAA"})
	must(t, e.run("on", "-y", "p"))
	must(t, os.WriteFile(filepath.Join(e.root, "etc/p.conf"), []byte("BBBB"), 0o644))

	if err := e.run("off", "-y", "p"); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("want a foreign-modification error, got %v", err)
	}
	if e.read("etc/p.conf") != "BBBB" || !slices.Equal(e.hostModules(), []string{"p"}) {
		t.Fatal("a refused off must change nothing")
	}
	must(t, e.run("off", "-f", "-y", "p"))
	if e.read("etc/p.conf.arcus-foreign") != "BBBB" || e.read("etc/p.conf") != "<missing>" {
		t.Errorf("forced off: conf=%q aside=%q", e.read("etc/p.conf"), e.read("etc/p.conf.arcus-foreign"))
	}
}

func TestDoctor(t *testing.T) {
	e := newEnv(t)
	e.wlBase()
	must(t, e.run("on", "-y", "wl-base"))
	must(t, e.run("doctor"))
	if !strings.Contains(e.out.String(), "No problems found.") {
		t.Fatalf("doctor on a clean system:\n%s", e.out.String())
	}

	// An interrupted turn-on of the home layer, a package removed by hand,
	// and a host file that no longer matches.
	e.module("orphan", "", map[string]string{"home/.orphan": "x"})
	dirs := map[string]string{"orphan": filepath.Join(e.home, ".config", "arcus", "modules", "orphan")}
	must(t, layer.NewHome(e.home, filepath.Join(e.home, ".local", "state", "arcus"), dirs).Activate(context.Background(), "orphan", nil))
	e.sys.SetPackages(slices.DeleteFunc(e.sys.Packages(), func(p string) bool { return p == "wmenu" })...)

	err := e.run("doctor")
	out := e.out.String()
	if err == nil || !strings.Contains(out, "orphan has a activating ledger") && !strings.Contains(out, "orphan has a active ledger") {
		t.Fatalf("doctor missed the orphan ledger (%v):\n%s", err, out)
	}
	for _, want := range []string{"removed outside arcus): wmenu", "'arcus plan' shows"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}

	_ = e.run("doctor", "-fix")
	if e.read("home/u/.orphan") != "<missing>" {
		t.Error("doctor -fix did not roll back the orphan layer")
	}
	if got := e.state(true).Owned[state.Pacman]["wl-base"]; slices.Contains(got, "wmenu") {
		t.Errorf("doctor -fix kept the record of a removed package: %v", got)
	}
	must(t, e.run("apply", "-y"))
	must(t, e.run("doctor"))
}

func TestNewMvRm(t *testing.T) {
	e := newEnv(t)
	must(t, e.run("new", "mine"))
	dir := filepath.Join(e.home, ".config", "arcus", "modules", "mine")
	if _, err := os.Stat(filepath.Join(dir, "module.toml")); err != nil {
		t.Fatal(err)
	}
	if err := e.run("new", "mine"); err == nil {
		t.Fatal("new over an existing module must fail")
	}
	must(t, os.MkdirAll(filepath.Join(dir, "home"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "home/.minerc"), []byte("x"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "system/etc"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "system/etc/mine.conf"), []byte("x"), 0o644))
	e.module("user-of-mine", `requires = ["mine"]`, nil)

	must(t, e.run("on", "-y", "mine"))
	if err := e.run("mv", "mine", "ours"); err == nil || !strings.Contains(err.Error(), "user-of-mine") {
		t.Fatalf("mv must refuse while other modules refer to it: %v", err)
	}
	must(t, os.RemoveAll(filepath.Join(e.home, ".config", "arcus", "modules", "user-of-mine")))

	must(t, e.run("mv", "mine", "ours"))
	if got := e.hostModules(); !slices.Equal(got, []string{"ours"}) {
		t.Errorf("host file = %v", got)
	}
	target, err := os.Readlink(filepath.Join(e.home, ".minerc"))
	if err != nil || !strings.Contains(target, "/modules/ours/") {
		t.Errorf("home link not moved: %q, %v", target, err)
	}
	for _, system := range []bool{false, true} {
		if st := e.state(system); !slices.Equal(st.Modules, []string{"ours"}) {
			t.Errorf("state not renamed: %+v", st)
		}
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after mv:\n%s", e.out.String())
	}

	if err := e.run("rm", "-y", "ours"); err == nil || !strings.Contains(err.Error(), "turn it off first") {
		t.Fatalf("rm of a module that is on: %v", err)
	}
	must(t, e.run("off", "-y", "ours"))
	must(t, e.run("rm", "-y", "ours"))
	if _, err := os.Stat(filepath.Join(e.home, ".config", "arcus", "modules", "ours")); !errors.Is(err, iofs.ErrNotExist) {
		t.Error("rm left the module directory")
	}
}

func TestLs(t *testing.T) {
	e := newEnv(t)
	e.module("base", "", nil)
	e.module("wl-base", "category = \"desktop\"\nrequires = [\"base\"]", map[string]string{"home/.x": "x"})
	must(t, e.run("on", "-y", "wl-base"))

	must(t, e.run("ls"))
	lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
	want := [][]string{
		{"CATEGORY", "MODULE", "STATUS", "RANK", "VIA", "FILES", "REQUIRES"},
		{"desktop", "wl-base", "on", "2", "host", "home", "base"},
		{"-", "base", "on", "1", "requires", "-", "-"},
	}
	if len(lines) != len(want) {
		t.Fatalf("ls:\n%s", e.out.String())
	}
	for i, fields := range want {
		if got := strings.Fields(lines[i]); !slices.Equal(got, fields) {
			t.Errorf("line %d = %v, want %v", i, got, fields)
		}
	}
}

func TestPlanShowsReplacedFilesAndRestoresThem(t *testing.T) {
	e := newEnv(t)
	must(t, os.WriteFile(filepath.Join(e.home, ".bashrc"), []byte("old"), 0o644))
	e.module("shell", "", map[string]string{"home/.bashrc": "new"})

	must(t, e.run("plan", "shell"))
	if want := "! " + filepath.Join(e.home, ".bashrc") + " (kept as .bashrc.orig)"; !strings.Contains(e.out.String(), want) {
		t.Fatalf("plan lacks %q:\n%s", want, e.out.String())
	}
	must(t, e.run("on", "-y", "shell"))
	if e.read("home/u/.bashrc") != "new" || e.read("home/u/.bashrc.orig") != "old" {
		t.Fatal("on did not replace and keep the original")
	}
	must(t, e.run("off", "-y", "shell"))
	if e.read("home/u/.bashrc") != "old" || e.read("home/u/.bashrc.orig") != "<missing>" {
		t.Fatal("off did not restore the original")
	}
}

func (e *env) profileFile(name, content string) {
	e.t.Helper()
	dir := filepath.Join(e.home, ".config", "arcus", "profiles")
	must(e.t, os.MkdirAll(dir, 0o755))
	must(e.t, os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644))
}

func (e *env) hostFile() *host.File {
	e.t.Helper()
	f, err := host.Load(e.hostPath())
	must(e.t, err)
	return f
}

// Profiles group modules; the host file lists profiles plus its own modules.
func TestProfiles(t *testing.T) {
	e := newEnv(t)
	e.module("shell", "", map[string]string{"home/.bashrc": "shell"})
	e.module("nvim", "", map[string]string{"home/.config/nvim/init.lua": "nvim"})
	e.module("laptop", "", map[string]string{"home/.local/bin/battery_monitor": "laptop"})
	e.profileFile("main", "# every machine\nmodules = [\"shell\", \"nvim\"]\n")

	must(t, e.run("on", "-y", "main"))
	must(t, e.run("on", "-y", "laptop"))
	if f := e.hostFile(); !slices.Equal(f.Profiles, []string{"main"}) || !slices.Equal(f.Modules, []string{"laptop"}) {
		t.Fatalf("host file = %+v", f)
	}
	for rel, want := range map[string]string{".bashrc": "shell", ".config/nvim/init.lua": "nvim", ".local/bin/battery_monitor": "laptop"} {
		if got := e.read("home/u/" + rel); got != want {
			t.Errorf("%s = %q", rel, got)
		}
	}

	must(t, e.run("ls"))
	for _, want := range []string{"main     on      shell,nvim", "nvim on 2 main", "laptop on 3 host"} {
		if !strings.Contains(strings.Join(strings.Fields(e.out.String()), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("ls lacks %q:\n%s", want, e.out.String())
		}
	}

	if err := e.run("off", "-y", "nvim"); err == nil || !strings.Contains(err.Error(), `comes from profile "main"`) {
		t.Fatalf("off of a module from a profile: %v", err)
	}
	if err := e.run("rm", "-y", "nvim"); err == nil || !strings.Contains(err.Error(), "profile main") {
		t.Fatalf("rm of a module a profile lists: %v", err)
	}

	must(t, e.run("off", "-y", "main"))
	if e.read("home/u/.bashrc") != "<missing>" || e.read("home/u/.local/bin/battery_monitor") != "laptop" {
		t.Error("off main must remove its modules and keep the host's own")
	}
	if f := e.hostFile(); len(f.Profiles) != 0 || !slices.Equal(f.Modules, []string{"laptop"}) {
		t.Fatalf("host file = %+v", f)
	}

	e.profileFile("shell", "modules = []")
	if err := e.run("ls"); err == nil || !strings.Contains(err.Error(), "both a module") {
		t.Fatalf("name clash: %v", err)
	}
}

func (e *env) count(prefix string) int {
	n := 0
	for _, line := range e.sys.Log() {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// Own builds come from a recipe in the module, are built as the user,
// installed with pacman -U, and rebuilt only when the recipe changes or a
// rebuild is asked for.
func TestOwnBuilds(t *testing.T) {
	e := newEnv(t)
	t.Setenv("XDG_CACHE_HOME", "")
	dir := e.module("wl-base", "[packages]\nlocal = [\"waylock-arcus\"]\n", map[string]string{
		"pkg/waylock-arcus/PKGBUILD":           "pkgname=waylock-arcus\npkgver=1.6.0\npkgrel=1\n",
		"pkg/waylock-arcus/bordercolors.patch": "v1",
	})
	recipe := filepath.Join(dir, "pkg", "waylock-arcus")

	must(t, e.run("plan", "wl-base"))
	for _, want := range []string{"user  build waylock-arcus from wl-base/pkg/waylock-arcus", "root  install own build for wl-base: waylock-arcus"} {
		if !strings.Contains(e.out.String(), want) {
			t.Fatalf("plan lacks %q:\n%s", want, e.out.String())
		}
	}

	must(t, e.run("on", "-y", "wl-base"))
	contains(t, e.sys.Packages(), "waylock-arcus")
	if e.count("makepkg") != 1 || e.count("pacman -U") != 1 {
		t.Fatalf("log:\n%s", strings.Join(e.sys.Log(), "\n"))
	}
	st := e.state(true)
	if st.Built["waylock-arcus"] == "" || !slices.Contains(st.Owned[state.Local]["wl-base"], "waylock-arcus") {
		t.Fatalf("state = %+v", st)
	}
	if entries, _ := os.ReadDir(recipe); len(entries) != 2 {
		t.Errorf("the build wrote into the recipe directory: %v", entries)
	}

	must(t, e.run("apply", "-y"))
	if e.count("makepkg") != 1 {
		t.Error("an unchanged recipe was rebuilt")
	}

	must(t, os.WriteFile(filepath.Join(recipe, "bordercolors.patch"), []byte("v2"), 0o644))
	must(t, e.run("apply", "-y"))
	if e.count("makepkg") != 2 || e.count("pacman -U") != 2 {
		t.Error("a changed recipe was not rebuilt and reinstalled")
	}

	must(t, e.run("rebuild", "-y", "waylock-arcus"))
	if e.count("makepkg") != 3 || e.count("pacman -U") != 3 {
		t.Error("rebuild did not build and reinstall")
	}
	if err := e.run("rebuild", "-y", "dwm-arcus"); err == nil || !strings.Contains(err.Error(), "not an own build") {
		t.Fatalf("rebuild of an unknown build: %v", err)
	}

	must(t, e.run("off", "-y", "wl-base"))
	lacks(t, e.sys.Packages(), "waylock-arcus")
	if st := e.state(true); len(st.Built) != 0 || len(st.Owned) != 0 {
		t.Fatalf("state after off = %+v", st)
	}
}

// Repository packages come before the build, so they stay, recorded; the
// failed build installs nothing and leaves the host file alone.
func TestFailedBuildInstallsNoOwnBuild(t *testing.T) {
	e := newEnv(t)
	t.Setenv("XDG_CACHE_HOME", "")
	e.module("x11", "[packages]\npacman = [\"libx11\"]\nlocal = [\"dwm-arcus\"]\n", map[string]string{
		"pkg/dwm-arcus/PKGBUILD": "pkgname=dwm-arcus\npkgver=6.8\npkgrel=1\n",
	})
	e.sys.Fail("makepkg")
	if err := e.run("on", "-y", "x11"); err == nil || !strings.Contains(err.Error(), "building dwm-arcus") {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(e.sys.Packages(), []string{"libx11"}) || e.hostModules() != nil {
		t.Errorf("packages %v: a failed build must install no own build and leave the host file alone", e.sys.Packages())
	}
	if got := e.state(true).Owned[state.Pacman]["x11"]; !slices.Equal(got, []string{"libx11"}) {
		t.Errorf("libx11 not recorded: %v", got)
	}
}

// Installed packages that the packages to install conflict with are shown
// in the plan and removed right before the install that replaces them: the
// repository ones before the build, which then finds its dependencies.
func TestConflictingPackagesAreReplaced(t *testing.T) {
	e := newEnv(t)
	t.Setenv("XDG_CACHE_HOME", "")
	e.module("stream", "[packages]\npacman = [\"river\"]\nlocal = [\"waylock-arcus\"]\n", map[string]string{
		"pkg/waylock-arcus/PKGBUILD": "pkgname=waylock-arcus\npkgver=1.6.0\npkgrel=1\n",
	})
	e.sys.SetPackages("river-classic", "waylock")
	e.sys.SetMeta("river conflicts river-classic", "waylock-arcus provides waylock", "waylock-arcus conflicts waylock")

	must(t, e.run("plan", "stream"))
	for _, want := range []string{
		"root  remove packages that conflict with what is installed next: river-classic\n        x river-classic: conflicts with river\nroot  install packages for stream: river",
		"root  remove packages that conflict with what is installed next: waylock\n        x waylock: conflicts with waylock-arcus\nroot  install own build for stream: waylock-arcus",
	} {
		if !strings.Contains(e.out.String(), want) {
			t.Fatalf("plan lacks %q:\n%s", want, e.out.String())
		}
	}

	must(t, e.run("on", "-y", "stream"))
	if got := e.sys.Packages(); !slices.Equal(got, []string{"river", "waylock-arcus"}) {
		t.Errorf("packages = %v", got)
	}
	var order []string
	for _, line := range e.sys.Log() {
		for _, call := range []string{"pacman -Rdd", "pacman -S ", "makepkg", "pacman -U"} {
			if strings.HasPrefix(line, call) {
				order = append(order, line)
			}
		}
	}
	if len(order) != 5 || order[0] != "pacman -Rdd --noconfirm river-classic" || !strings.HasPrefix(order[1], "pacman -S ") ||
		!strings.HasPrefix(order[2], "makepkg") || order[3] != "pacman -Rdd --noconfirm waylock" || !strings.HasPrefix(order[4], "pacman -U") {
		t.Errorf("calls:\n%s", strings.Join(order, "\n"))
	}
	st := e.state(true)
	if !slices.Equal(st.Owned[state.Pacman]["stream"], []string{"river"}) || !slices.Equal(st.Owned[state.Local]["stream"], []string{"waylock-arcus"}) {
		t.Errorf("owned = %v", st.Owned)
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("not converged:\n%s", e.out.String())
	}
}

// Regrouping modules moves files between them: a module that is gone and a
// file dropped from a restacked one free their paths before the new module
// deploys there, so neither the plan nor apply keeps a backup. A path whose
// original file comes back when the old module goes off is still replaced.
func TestRegroupMovesFilesWithoutBackups(t *testing.T) {
	e := newEnv(t)
	must(t, os.WriteFile(filepath.Join(e.home, ".inputrc"), []byte("original"), 0o644))
	e.module("shell", "", map[string]string{"home/.bashrc": "bash", "home/.config/tmux/tmux.conf": "tmux"})
	e.module("nvim", "", map[string]string{"home/.config/nvim/init.lua": "nvim", "home/.inputrc": "nvim"})
	must(t, e.run("on", "-y", "shell"))
	must(t, e.run("on", "-y", "nvim"))

	modules := filepath.Join(e.home, ".config", "arcus", "modules")
	must(t, os.RemoveAll(filepath.Join(modules, "nvim")))
	must(t, os.Remove(filepath.Join(modules, "shell", "home/.config/tmux/tmux.conf")))
	e.module("dev", "", map[string]string{"home/.config/nvim/init.lua": "nvim", "home/.config/tmux/tmux.conf": "tmux", "home/.inputrc": "nvim"})
	must(t, os.WriteFile(e.hostPath(),
		[]byte("modules = [\n  \"shell\",\n  \"dev\",\n]\n"), 0o644))

	must(t, e.run("plan"))
	if out := e.out.String(); strings.Count(out, "kept as") != 1 || !strings.Contains(out, "kept as .inputrc.orig") {
		t.Errorf("plan must keep a backup of .inputrc only:\n%s", out)
	}
	must(t, e.run("apply", "-y"))
	if e.read("home/u/.inputrc.orig") != "original" {
		t.Error("the original .inputrc was not kept")
	}
	for rel, want := range map[string]string{".config/nvim/init.lua": "dev", ".config/tmux/tmux.conf": "dev", ".bashrc": "shell", ".inputrc": "dev"} {
		target, err := os.Readlink(filepath.Join(e.home, rel))
		if err != nil || !strings.Contains(target, "/modules/"+want+"/") {
			t.Errorf("%s -> %q (%v), want a link into %s", rel, target, err, want)
		}
	}
	for _, rel := range []string{".config/nvim/init.lua.orig", ".config/nvim/init.lua.nvim", ".config/tmux/tmux.conf.orig", ".config/tmux/tmux.conf.shell"} {
		if _, err := os.Lstat(filepath.Join(e.home, rel)); err == nil {
			t.Errorf("backup %s left behind", rel)
		}
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after apply:\n%s", e.out.String())
	}
}

// The deployed stack follows the desired order: a module inserted in the
// middle goes in below the ones listed after it, and reordering the host
// file restacks.
func TestStackFollowsDesiredOrder(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"a", "b", "c"} {
		e.module(name, "", map[string]string{"home/.rc": name})
	}
	hostFile := e.hostPath()
	order := func(names ...string) {
		t.Helper()
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = strconv.Quote(n)
		}
		must(t, os.WriteFile(hostFile, []byte("modules = ["+strings.Join(quoted, ", ")+"]\n"), 0o644))
		must(t, e.run("apply", "-y"))
		must(t, e.run("plan"))
		if !strings.Contains(e.out.String(), "Nothing to do.") {
			t.Fatalf("plan after apply:\n%s", e.out.String())
		}
	}

	order("a", "c")
	order("a", "b", "c")
	if got := e.read("home/u/.rc"); got != "c" {
		t.Errorf("with a b c, .rc = %q, want c", got)
	}
	if st := e.state(false); !slices.Equal(st.Modules, []string{"a", "b", "c"}) {
		t.Errorf("stack = %v", st.Modules)
	}
	order("c", "b", "a")
	if got := e.read("home/u/.rc"); got != "a" {
		t.Errorf("with c b a, .rc = %q, want a", got)
	}
	order()
	if got := e.read("home/u/.rc"); got != "<missing>" {
		t.Errorf("with nothing on, .rc = %q", got)
	}
}

// A package that moves to another module without being removed is recorded
// under the new module, so turning that one off removes it.
func TestHandOverRecordsTheNewOwner(t *testing.T) {
	e := newEnv(t)
	e.module("old", "[packages]\npacman = [\"foot\"]\n", nil)
	must(t, e.run("on", "-y", "old"))
	modules := filepath.Join(e.home, ".config", "arcus", "modules")
	must(t, os.RemoveAll(filepath.Join(modules, "old")))
	e.module("new", "[packages]\npacman = [\"foot\"]\n", nil)
	must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"new\"]\n"), 0o644))

	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "foot: old → new") {
		t.Fatalf("plan lacks the hand-over:\n%s", e.out.String())
	}
	must(t, e.run("apply", "-y"))
	if got := e.state(true).Owned[state.Pacman]; !slices.Equal(got["new"], []string{"foot"}) || got["old"] != nil {
		t.Fatalf("owned = %v", got)
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Fatalf("plan after apply:\n%s", e.out.String())
	}
	must(t, e.run("off", "-y", "new"))
	lacks(t, e.sys.Packages(), "foot")
}

// A package that a module releases but another installed package still
// needs stays installed, marked as a dependency, instead of failing the
// whole removal; pacman takes it out with the last package that needs it.
func TestOffKeepsPackagesOthersNeed(t *testing.T) {
	e := newEnv(t)
	e.module("x11", "[packages]\npacman = [\"xrandr\", \"picom\"]\naur = [\"applet\", \"helper\"]\n", nil)
	must(t, e.run("on", "-y", "x11"))
	e.sys.SetPackages(append(e.sys.Packages(), "steam", "tray")...)
	e.sys.SetRequires("steam xrandr", "tray helper")

	must(t, e.run("off", "-n", "x11"))
	out := e.out.String()
	for _, want := range []string{
		"remove packages: picom; keep xrandr, which other packages need, as dependencies",
		"= xrandr: needed by steam",
		"remove AUR packages: applet; keep helper, which other packages need, as dependencies",
		"= helper: needed by tray",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("off -n lacks %q:\n%s", want, out)
		}
	}

	must(t, e.run("off", "-y", "x11"))
	contains(t, e.sys.Packages(), "xrandr", "helper", "steam", "tray")
	lacks(t, e.sys.Packages(), "picom", "applet")
	contains(t, e.sys.Deps(), "xrandr", "helper")
	if owned := e.state(true).Owned[state.Pacman]; len(owned["x11"]) != 0 {
		t.Errorf("x11 still owns %v", owned["x11"])
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after off:\n%s", e.out.String())
	}
}

// clean marks what modules list (and protected packages) as explicitly
// installed, marks other needed packages as dependencies and removes the
// rest with the dependencies nothing needs any more.
func TestClean(t *testing.T) {
	e := newEnv(t)
	e.sys.SetPackages("base", "foot", "lib", "tool", "tool-lib", "orphan", "pixman")
	e.sys.SetDeps("foot", "tool-lib", "orphan", "pixman")
	e.sys.SetRequires("foot lib", "foot pixman", "tool tool-lib")
	e.module("m", "[packages]\npacman = [\"foot\", \"go\"]\n", nil)
	must(t, e.run("on", "-y", "m"))
	before := e.sys.Packages()

	must(t, e.run("clean", "-n"))
	out := e.out.String()
	for _, want := range []string{
		"mark as explicitly installed (a module lists them or they are protected): foot",
		"mark as dependencies (no module lists them; other packages need them): lib",
		"remove packages no module lists, with the dependencies nothing else needs: orphan tool",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("clean -n lacks %q:\n%s", want, out)
		}
	}
	if !slices.Equal(e.sys.Packages(), before) {
		t.Fatal("clean -n changed packages")
	}

	must(t, e.run("doctor"))
	if out := e.out.String(); !strings.Contains(out, "nothing needs them: orphan tool") || !strings.Contains(out, "No problems found.") {
		t.Errorf("doctor:\n%s", out)
	}

	must(t, e.run("clean", "-y"))
	if got, want := e.sys.Packages(), []string{"base", "foot", "go", "lib", "pixman"}; !slices.Equal(got, want) {
		t.Errorf("packages = %v, want %v", got, want)
	}
	if got, want := e.sys.Deps(), []string{"lib", "pixman"}; !slices.Equal(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
	must(t, e.run("clean", "-n"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("clean after clean:\n%s", e.out.String())
	}

	// clean waits for apply.
	e.module("m", "[packages]\npacman = [\"foot\", \"go\", \"wmenu\"]\n", nil)
	if err := e.run("clean", "-y"); err == nil || !strings.Contains(err.Error(), "run 'arcus apply' first") {
		t.Fatalf("clean with apply pending: %v", err)
	}
}

// arcus machine writes the machine module from the running system; the
// module then works like any other, taking over what is already installed.
func TestMachine(t *testing.T) {
	e := newEnv(t)
	for path, content := range map[string]string{
		"proc/cpuinfo":     "vendor_id\t: GenuineIntel\n",
		"proc/self/mounts": "/dev/sda2 / ext4 rw 0 0\n/dev/sda1 /boot vfat rw 0 0\n",
	} {
		must(t, os.MkdirAll(filepath.Join(e.root, filepath.Dir(path)), 0o755))
		must(t, os.WriteFile(filepath.Join(e.root, path), []byte(content), 0o644))
	}
	must(t, os.MkdirAll(filepath.Join(e.root, "sys/firmware/efi"), 0o755))
	e.sys.SetPackages("linux", "linux-firmware", "grub", "efibootmgr", "zram-generator")

	e.profileFile("pc", "modules = [\"machine\"]\n")
	if err := e.run("plan", "pc"); err == nil || !strings.Contains(err.Error(), "'arcus machine' writes") {
		t.Errorf("plan before arcus machine: %v", err)
	}
	must(t, e.run("machine", "-n"))
	if out := e.out.String(); !strings.Contains(out, "+ module.toml") || !strings.Contains(out, "+ system/etc/systemd/zram-generator.conf") {
		t.Fatalf("machine -n:\n%s", out)
	}
	must(t, e.run("machine", "-y"))
	if !strings.Contains(e.out.String(), "Turn it on with 'arcus on machine'") {
		t.Errorf("machine -y, nothing selects it:\n%s", e.out.String())
	}
	must(t, os.RemoveAll(filepath.Join(e.root, "etc/arcus/modules/machine")))
	e.module("shell", "", nil)
	must(t, e.run("on", "-y", "shell"))
	must(t, os.WriteFile(e.hostPath(), []byte("profiles = [\"pc\"]\nmodules = [\"shell\"]\n"), 0o644))
	must(t, e.run("machine", "-y"))
	if !strings.Contains(e.out.String(), "Profile pc turns it on") {
		t.Errorf("machine -y, the pc profile selects it:\n%s", e.out.String())
	}
	must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"shell\"]\n"), 0o644))
	if !strings.Contains(e.read("etc/arcus/modules/machine/module.toml"), `"linux", "linux-firmware", "intel-ucode", "grub", "efibootmgr", "dosfstools", "e2fsprogs", "zram-generator"`) {
		t.Fatalf("module.toml:\n%s", e.read("etc/arcus/modules/machine/module.toml"))
	}
	must(t, e.run("machine"))
	if !strings.Contains(e.out.String(), "up to date") {
		t.Errorf("machine again:\n%s", e.out.String())
	}

	must(t, e.run("on", "-y", "machine"))
	contains(t, e.sys.Packages(), "intel-ucode", "dosfstools", "e2fsprogs")
	if e.read("etc/systemd/zram-generator.conf") == "<missing>" {
		t.Error("the zram config was not deployed")
	}
	if got := e.state(true).Owned[state.Pacman]["machine"]; !slices.Contains(got, "grub") || !slices.Contains(got, "intel-ucode") {
		t.Errorf("machine owns %v; grub was taken over, intel-ucode installed", got)
	}
	if err := e.run("rm", "-y", "machine"); err == nil || !strings.Contains(err.Error(), "machine-local") {
		t.Errorf("rm of the machine module: %v", err)
	}
}

// dirs makes sure home directories exist; off removes them only when empty.
func TestHomeDirs(t *testing.T) {
	e := newEnv(t)
	must(t, os.MkdirAll(filepath.Join(e.home, "Documents"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.home, "Documents", "notes.md"), []byte("mine"), 0o644))
	e.module("base", `dirs = ["code", "Documents", "Pictures/Screenshots"]`, nil)

	must(t, e.run("plan", "base"))
	for _, want := range []string{"take over home directories for base (already there): Documents", "create home directories for base: code Pictures/Screenshots"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, e.out.String())
		}
	}
	must(t, e.run("on", "-y", "base"))
	for _, d := range []string{"code", "Pictures/Screenshots"} {
		if info, err := os.Stat(filepath.Join(e.home, d)); err != nil || !info.IsDir() {
			t.Errorf("%s was not created", d)
		}
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after on:\n%s", e.out.String())
	}

	must(t, e.run("off", "-y", "base"))
	if _, err := os.Stat(filepath.Join(e.home, "code")); !errors.Is(err, iofs.ErrNotExist) {
		t.Error("the empty code directory must go")
	}
	if e.read("home/u/Documents/notes.md") != "mine" {
		t.Error("a directory with files in it must stay")
	}
	if got := e.state(false).Owned[state.HomeDirs]; len(got) != 0 {
		t.Errorf("still recorded: %v", got)
	}
}

// links owns symlinks in $HOME: created on, taken over when already right,
// moved when the target changes, and removed on off only while unchanged.
func TestHomeLinks(t *testing.T) {
	e := newEnv(t)
	e.module("sync", `links = { ".config/app/data" = "Sync/app/data", ".local/share/x" = "Sync/x" }`, nil)
	// One of them is already there and right.
	must(t, os.MkdirAll(filepath.Join(e.home, ".local/share"), 0o755))
	must(t, os.Symlink(filepath.Join(e.home, "Sync/x"), filepath.Join(e.home, ".local/share/x")))

	must(t, e.run("plan", "sync"))
	for _, want := range []string{"take over home links for sync (already there): .local/share/x -> Sync/x", "create home links for sync: .config/app/data -> Sync/app/data"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, e.out.String())
		}
	}
	must(t, e.run("on", "-y", "sync"))
	link := filepath.Join(e.home, ".config/app/data")
	if got, err := os.Readlink(link); err != nil || got != filepath.Join(e.home, "Sync/app/data") {
		t.Fatalf("link = %q, %v", got, err)
	}
	must(t, e.run("plan"))
	if !strings.Contains(e.out.String(), "Nothing to do.") {
		t.Errorf("plan after on:\n%s", e.out.String())
	}

	// A new target replaces the link.
	e.module("sync", `links = { ".config/app/data" = "Sync/other", ".local/share/x" = "Sync/x" }`, nil)
	must(t, e.run("apply", "-y"))
	if got, _ := os.Readlink(link); got != filepath.Join(e.home, "Sync/other") {
		t.Errorf("after the target changed, link = %q", got)
	}

	// A link the user pointed elsewhere stays on off.
	must(t, os.Remove(filepath.Join(e.home, ".local/share/x")))
	must(t, os.Symlink("/elsewhere", filepath.Join(e.home, ".local/share/x")))
	must(t, e.run("off", "-y", "sync"))
	if _, err := os.Lstat(link); !errors.Is(err, iofs.ErrNotExist) {
		t.Error("the unchanged link must go")
	}
	if got, _ := os.Readlink(filepath.Join(e.home, ".local/share/x")); got != "/elsewhere" {
		t.Errorf("a changed link must stay, got %q", got)
	}
	if got := e.state(false).Owned[state.HomeLinks]; len(got) != 0 {
		t.Errorf("still recorded: %v", got)
	}

	// Something else at the path is never replaced.
	must(t, os.MkdirAll(filepath.Dir(link), 0o755))
	must(t, os.WriteFile(link, []byte("mine"), 0o644))
	if err := e.run("on", "-y", "sync"); err == nil || !strings.Contains(err.Error(), "already exists and isn't a link") {
		t.Errorf("on over a file: %v", err)
	}
	if e.read("home/u/.config/app/data") != "mine" {
		t.Error("the file was replaced")
	}
}

// Turning on a module that conflicts with one the host file lists swaps
// them; one a profile brings in is refused.
func TestOnSwapsConflicts(t *testing.T) {
	e := newEnv(t)
	e.module("nebula", `category = "theme"`+"\n"+`conflicts = ["aurora"]`, map[string]string{"home/.config/theme/colors.sh": "nebula"})
	e.module("aurora", `category = "theme"`+"\n"+`conflicts = ["nebula"]`, map[string]string{"home/.config/theme/colors.sh": "aurora"})
	must(t, e.run("on", "-y", "nebula"))
	must(t, e.run("on", "-y", "aurora"))
	if !strings.Contains(e.out.String(), "Swapping nebula for aurora") {
		t.Errorf("on aurora:\n%s", e.out.String())
	}
	if got := e.read("home/u/.config/theme/colors.sh"); got != "aurora" {
		t.Errorf("colors.sh = %q", got)
	}
	if got := e.hostModules(); !slices.Equal(got, []string{"aurora"}) {
		t.Errorf("host modules = %v", got)
	}

	e.profileFile("look", "modules = [\"aurora\"]\n")
	must(t, os.WriteFile(e.hostPath(), []byte("profiles = [\"look\"]\n"), 0o644))
	if err := e.run("on", "-y", "nebula"); err == nil || !strings.Contains(err.Error(), "profile \"look\"") {
		t.Errorf("on nebula against a profile: %v", err)
	}
}

// A file added to a module that's on, where a file already is, shows in the
// plan as a replacement, and apply keeps the original.
func TestAddedFileReplacesOne(t *testing.T) {
	e := newEnv(t)
	dir := e.module("shell", "", map[string]string{"home/.bashrc": "bash"})
	must(t, e.run("on", "-y", "shell"))
	must(t, os.MkdirAll(filepath.Join(e.home, ".config/fastfetch"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.home, ".config/fastfetch/config.jsonc"), []byte("mine"), 0o644))
	must(t, os.MkdirAll(filepath.Join(dir, "home/.config/fastfetch"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "home/.config/fastfetch/config.jsonc"), []byte("module"), 0o644))

	must(t, e.run("plan"))
	if want := "! " + filepath.Join(e.home, ".config/fastfetch/config.jsonc") + " (the file there is kept as a backup)"; !strings.Contains(e.out.String(), want) {
		t.Errorf("plan lacks %q:\n%s", want, e.out.String())
	}
	must(t, e.run("apply", "-y"))
	if e.read("home/u/.config/fastfetch/config.jsonc") != "module" || e.read("home/u/.config/fastfetch/config.jsonc.orig") != "mine" {
		t.Error("apply must deploy the module's file and keep the original")
	}
}

// A host file left in the config directory by arcus before 0.2.0 is
// refused with the move command, so on doesn't start an empty one.
func TestOldHostFileRefused(t *testing.T) {
	e := newEnv(t)
	e.module("shell", "", nil)
	old := filepath.Join(e.home, ".config", "arcus", "hosts", "testhost.toml")
	must(t, os.MkdirAll(filepath.Dir(old), 0o755))
	must(t, os.WriteFile(old, []byte("modules = [\"shell\"]\n"), 0o644))

	for _, args := range [][]string{{"plan"}, {"on", "-y", "shell"}} {
		if err := e.run(args...); err == nil || !strings.Contains(err.Error(), "mv "+old+" ") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.home, ".local", "state", "arcus", "host.toml")); err == nil {
		t.Error("a new host file was started")
	}
}
