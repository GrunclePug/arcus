package system

import (
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/system/fake"
)

func keys(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}

func TestCommandsAgainstFakes(t *testing.T) {
	sys := fake.New(t)
	sys.SetPackages("base", "go")
	sys.SetUnits(false, "fstrim.timer")
	sys.SetGroups("wheel")
	r := &Runner{Stdout: io.Discard, Stderr: io.Discard, NoConfirm: true}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if units, err := EnabledUnits(true); err != nil || len(units) != 0 {
		t.Fatalf("an empty query is an empty set: %v, %v", units, err)
	}
	must(r.PacmanInstall([]string{"foot", "wmenu"}))
	must(r.AURInstall("yay", []string{"wlr-randr-git"}))
	must(r.PacmanRemove([]string{"wmenu"}))
	must(r.Enable(false, []string{"bluetooth.service"}, true))
	must(r.Enable(true, []string{"mako.service"}, false))
	must(r.Disable(false, []string{"fstrim.timer"}, false))
	must(r.JoinGroup("u", "video"))
	must(r.LeaveGroup("u", "wheel"))

	installed, err := Installed()
	must(err)
	if got, want := keys(installed), []string{"base", "foot", "go", "wlr-randr-git"}; !slices.Equal(got, want) {
		t.Errorf("installed = %v, want %v", got, want)
	}
	units, err := EnabledUnits(false)
	must(err)
	if got := keys(units); !slices.Equal(got, []string{"bluetooth.service"}) {
		t.Errorf("system units = %v", got)
	}
	units, err = EnabledUnits(true)
	must(err)
	if got := keys(units); !slices.Equal(got, []string{"mako.service"}) {
		t.Errorf("user units = %v", got)
	}
	groups, err := Groups("u")
	must(err)
	if got := keys(groups); !slices.Equal(got, []string{"video"}) {
		t.Errorf("groups = %v", got)
	}

	log := strings.Join(sys.Log(), "\n")
	for _, call := range []string{
		"pacman -S --needed --noconfirm foot wmenu",
		"yay --removemake -S --needed --noconfirm wlr-randr-git",
		"pacman -Rns --noconfirm wmenu",
		"systemctl enable --now bluetooth.service",
		"systemctl --user --root=/ enable mako.service",
		"gpasswd -a u video",
	} {
		if !strings.Contains(log, call) {
			t.Errorf("missing call %q in log:\n%s", call, log)
		}
	}
}

// User units are enabled through their files, and started or stopped only
// when the user's manager is reachable.
func TestUserUnitsWithoutAManager(t *testing.T) {
	sys := fake.New(t)
	r := &Runner{Stdout: io.Discard, Stderr: io.Discard}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if err := r.Enable(true, []string{"syncthing.service"}, true); err != nil {
		t.Fatal(err)
	}
	runtime := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtime, "bus"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	if err := r.Disable(true, []string{"syncthing.service"}, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user --root=/ enable syncthing.service",
		"systemctl --user stop syncthing.service",
		"systemctl --user --root=/ disable syncthing.service",
	}
	if got := sys.Log(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestFailureIsReported(t *testing.T) {
	sys := fake.New(t)
	sys.Fail("pacman")
	r := &Runner{Stdout: io.Discard, Stderr: io.Discard}
	if err := r.PacmanInstall([]string{"foot"}); err == nil || !strings.Contains(err.Error(), "pacman -S --needed foot") {
		t.Fatalf("got %v", err)
	}
}

func TestParseInfo(t *testing.T) {
	info := `Name            : xorg-xrandr
Version         : 1.5.3-1
Provides        : xrandr=1.5.3
Required By     : steam  arandr
                  autorandr
Optional For    : None
Conflicts With  : None

Name            : waylock-arcus
Provides        : waylock
Conflicts With  : waylock  swaylock<2
Required By     : None
`
	got := parseInfo(info)
	want := map[string]Package{
		"xorg-xrandr":   {Provides: []string{"xrandr=1.5.3"}, RequiredBy: []string{"steam", "arandr", "autorandr"}},
		"waylock-arcus": {Provides: []string{"waylock"}, Conflicts: []string{"waylock", "swaylock<2"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseInfo = %v, want %v", got, want)
	}
}

// srcInfo is real makepkg --printsrcinfo output for a split PKGBUILD:
//
//	pkgbase=demo; pkgname=(demo-a demo-b demo-c demo-d pkgbase); arch=(x86_64)
//	depends=(glibc) depends_x86_64=(lib64) makedepends=(make)
//	makedepends_x86_64=(make64) checkdepends=(check) checkdepends_x86_64=(check64)
//	provides=(virt) conflicts=(old) conflicts_x86_64=(old64)
//	package_demo-a() { depends=(); provides=(); conflicts=(); depends_x86_64=(); }
//	package_demo-b() { depends=(only-b); }
//	package_demo-c() { depends_x86_64=(c64); provides_x86_64=(pc64); }
//	package_demo-d() { depends+=(extra); }
//	package_pkgbase() { depends=(pb); }
//
// An emptied list prints with a trailing space after "=".
const srcInfo = "pkgbase = demo\n" +
	"\tpkgver = 1\n\tpkgrel = 1\n\tarch = x86_64\n" +
	"\tcheckdepends = check\n\tmakedepends = make\n\tdepends = glibc\n" +
	"\tprovides = virt\n\tconflicts = old\n\tconflicts_x86_64 = old64\n" +
	"\tdepends_x86_64 = lib64\n\tmakedepends_x86_64 = make64\n\tcheckdepends_x86_64 = check64\n" +
	"\npkgname = demo-a\n\tdepends = \n\tprovides = \n\tconflicts = \n\tdepends_x86_64 = \n" +
	"\npkgname = demo-b\n\tdepends = only-b\n" +
	"\npkgname = demo-c\n\tprovides_x86_64 = pc64\n\tdepends_x86_64 = c64\n" +
	"\npkgname = demo-d\n\tdepends = glibc\n\tdepends = extra\n" +
	"\npkgname = pkgbase\n\tdepends = pb\n"

// parseSrcInfo builds each package's lists the way makepkg does: an own
// list replaces pkgbase's with its _<arch> variant, own _<arch> variants are
// added, an emptied list is empty, and what building needs comes from
// pkgbase.
func TestParseSrcInfo(t *testing.T) {
	build := []string{"glibc", "lib64", "make", "make64", "check", "check64"}
	for _, c := range []struct {
		pkg  string
		want Package
	}{
		{"demo-a", Package{}},
		{"demo-b", Package{Provides: []string{"virt"}, Conflicts: []string{"old", "old64"}, Depends: []string{"only-b"}}},
		{"demo-c", Package{Provides: []string{"virt", "pc64"}, Conflicts: []string{"old", "old64"}, Depends: []string{"glibc", "lib64", "c64"}}},
		// "depends+=(extra)" prints like an override; makepkg would keep lib64.
		{"demo-d", Package{Provides: []string{"virt"}, Conflicts: []string{"old", "old64"}, Depends: []string{"glibc", "extra"}}},
		// A package named pkgbase is a package section, not the pkgbase one.
		{"pkgbase", Package{Provides: []string{"virt"}, Conflicts: []string{"old", "old64"}, Depends: []string{"pb"}}},
	} {
		c.want.BuildDepends = build
		if got := parseSrcInfo(srcInfo, c.pkg, "x86_64"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s = %#v, want %#v", c.pkg, got, c.want)
		}
	}
	got := parseSrcInfo(srcInfo, "demo-a", "x86_64")
	if len(got.Depends) != 0 || len(got.Provides) != 0 || len(got.Conflicts) != 0 {
		t.Errorf("emptied lists = %v", got)
	}

	// For arch=(any), makepkg leaves makedepends_x86_64 out of .SRCINFO,
	// though the build still needs it.
	anyArch := "pkgbase = anyone\n\tpkgver = 1\n\tpkgrel = 1\n\tarch = any\n\tmakedepends = make\n\npkgname = anyone\n"
	if got := parseSrcInfo(anyArch, "anyone", "x86_64").BuildDepends; !reflect.DeepEqual(got, []string{"make"}) {
		t.Errorf("arch=(any) BuildDepends = %v", got)
	}
}

// SrcInfo reads the same lists from the real makepkg, when it is installed.
func TestSrcInfo(t *testing.T) {
	if _, err := exec.LookPath("makepkg"); err != nil {
		t.Skip("makepkg is not installed")
	}
	t.Setenv("CARCH", "x86_64")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	pkgbuild := `pkgbase=demo
pkgname=(demo-b demo-c)
pkgver=1
pkgrel=1
arch=(x86_64)
depends=(glibc)
depends_x86_64=(lib64)
makedepends=(make)
checkdepends=(check)
provides=(virt)
package_demo-b() { depends=(only-b); }
package_demo-c() { depends_x86_64=(c64); }
`
	if err := os.WriteFile(filepath.Join(dir, "PKGBUILD"), []byte(pkgbuild), 0o644); err != nil {
		t.Fatal(err)
	}
	for pkg, depends := range map[string][]string{"demo-b": {"only-b"}, "demo-c": {"glibc", "lib64", "c64"}} {
		p, err := SrcInfo(dir, pkg)
		if err != nil {
			t.Fatal(err)
		}
		want := Package{Provides: []string{"virt"}, Depends: depends, BuildDepends: []string{"glibc", "lib64", "make", "check"}}
		if !reflect.DeepEqual(p, want) {
			t.Errorf("%s = %#v, want %#v", pkg, p, want)
		}
	}
}

// carch prefers $CARCH and falls back to the machine.
func TestCarch(t *testing.T) {
	t.Setenv("CARCH", "aarch64")
	if got := carch(); got != "aarch64" {
		t.Errorf("carch with CARCH = %q", got)
	}
	t.Setenv("CARCH", "")
	if got := carch(); got == "" {
		t.Error("carch without CARCH is empty")
	}
}

func TestEnabledOffPreset(t *testing.T) {
	sys := fake.New(t)
	sys.SetUnits(false, "sshd.service", "getty@.service", "fstrim.timer")
	sys.SetPresets(false, "getty@.service")
	got, err := EnabledOffPreset(false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fstrim.timer", "sshd.service"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got, err := EnabledOffPreset(true); err != nil || len(got) != 0 {
		t.Errorf("no user units: %v, %v", got, err)
	}
}

func TestWithAlso(t *testing.T) {
	root := t.TempDir()
	dirs := UnitDirs(root, filepath.Join(root, "home/.config"), filepath.Join(root, "home/.local/share"), false)
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dirs[2], "nm.service", "[Unit]\nAlso=not-install.service\n[Install]\nWantedBy=multi-user.target\nAlso=nm-dispatcher.service\nAlso = nm-wait.service other.socket\n")
	write(dirs[2], "other.socket", "[Install]\nAlso=nm.service\n")
	write(dirs[2], "tpl@.service", "[Install]\nAlso=tpl-helper.service\n")
	// The copy in /etc wins over the vendor's.
	write(dirs[2], "over.service", "[Install]\nAlso=vendor.service\n")
	write(dirs[0], "over.service", "[Install]\n")
	// Modules deploy their own units to /usr/local.
	write(dirs[1], "local.service", "[Install]\nAlso=local.timer\n")

	got := WithAlso(dirs, []string{"nm.service", "tpl@a.service", "over.service", "nofile.timer", "local.service"})
	want := []string{"nm.service", "tpl@a.service", "over.service", "nofile.timer", "local.service", "nm-dispatcher.service", "nm-wait.service", "other.socket", "tpl-helper.service", "local.timer"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// A user's copy in ~/.local/share wins over the vendor's.
	userDirs := UnitDirs(root, filepath.Join(root, "home/.config"), filepath.Join(root, "home/.local/share"), true)
	write(userDirs[4], "u.service", "[Install]\nAlso=u-helper.service\n")
	write(userDirs[2], "u.service", "[Install]\n")
	if got := WithAlso(userDirs, []string{"u.service"}); !slices.Equal(got, []string{"u.service"}) {
		t.Errorf("user: got %v, want [u.service]", got)
	}
	for in, want := range map[string]string{"a@b.service": "a@.service", "a@.service": "a@.service", "a.service": "a.service"} {
		if got := Template(in); got != want {
			t.Errorf("Template(%q) = %q, want %q", in, got, want)
		}
	}
}
