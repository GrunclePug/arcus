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

// A split package's own section replaces what pkgbase declares, and
// depends has its variant for the build architecture.
func TestParseSrcInfo(t *testing.T) {
	info := `pkgbase = waylock-arcus
	pkgver = 1.6.0
	makedepends = zig>=0.13
	provides = waylock
	conflicts = waylock
	depends = pam
	depends_x86_64 = x86-only
	depends_aarch64 = arm-only

pkgname = waylock-arcus

pkgname = waylock-arcus-debug
	conflicts = waylock-debug
	depends = waylock-arcus
`
	if got, want := parseSrcInfo(info, "waylock-arcus", "x86_64"), (Package{Provides: []string{"waylock"}, Conflicts: []string{"waylock"}, Depends: []string{"pam", "x86-only"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("waylock-arcus = %v, want %v", got, want)
	}
	if got, want := parseSrcInfo(info, "waylock-arcus-debug", "aarch64"), (Package{Provides: []string{"waylock"}, Conflicts: []string{"waylock-debug"}, Depends: []string{"waylock-arcus", "arm-only"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("waylock-arcus-debug = %v, want %v", got, want)
	}
}

// SrcInfo reads what building needs as makepkg does, with the real
// libmakepkg when it is installed: global depends and makedepends, and
// checkdepends while a check function runs, each with its CARCH variant,
// also from files the PKGBUILD sources from its startdir.
func TestSrcInfoBuildDepends(t *testing.T) {
	if _, err := os.Stat("/usr/share/makepkg/util/config.sh"); err != nil {
		t.Skip("libmakepkg is not installed")
	}
	if _, err := exec.LookPath("makepkg"); err != nil {
		t.Skip("makepkg is not installed")
	}
	t.Setenv("MAKEPKG_LIBRARY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	pkgbuild := `pkgname=(demo demo-extra)
pkgbase=demo
pkgver=1
pkgrel=1
arch=(x86_64 aarch64)
depends=(glibc 'zlib>=1')
depends_x86_64=(x86-lib)
depends_aarch64=(arm-lib)
makedepends=(cmake)
makedepends_aarch64=(arm-tool)
checkdepends=(bats)
source "$startdir/more.sh"
function check { :; }
package_demo() { depends=(only-demo); }
package_demo-extra() { :; }
`
	if err := os.WriteFile(filepath.Join(dir, "PKGBUILD"), []byte(pkgbuild), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "more.sh"), []byte("makedepends+=(own-tool)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		carch, conf string
		want        []string
	}{
		{"x86_64", "", []string{"glibc", "zlib>=1", "x86-lib", "cmake", "own-tool", "bats"}},
		{"aarch64", "", []string{"glibc", "zlib>=1", "arm-lib", "cmake", "own-tool", "arm-tool", "bats"}},
		{"x86_64", "BUILDENV=(!check)\n", []string{"glibc", "zlib>=1", "x86-lib", "cmake", "own-tool"}},
	} {
		t.Setenv("CARCH", c.carch)
		conf := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "pacman", "makepkg.conf")
		if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(conf, []byte(c.conf), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := SrcInfo(dir, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.BuildDepends, c.want) {
			t.Errorf("%s %q: BuildDepends = %v, want %v", c.carch, c.conf, p.BuildDepends, c.want)
		}
		arch := "x86-lib"
		if c.carch == "aarch64" {
			arch = "arm-lib"
		}
		if want := []string{"only-demo", arch}; !reflect.DeepEqual(p.Depends, want) {
			t.Errorf("%s: Depends = %v, want %v", c.carch, p.Depends, want)
		}
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
	dirs := UnitDirs(root, filepath.Join(root, "home/.config"), false)
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(dirs[1], "nm.service", "[Unit]\nAlso=not-install.service\n[Install]\nWantedBy=multi-user.target\nAlso=nm-dispatcher.service\nAlso = nm-wait.service other.socket\n")
	write(dirs[1], "other.socket", "[Install]\nAlso=nm.service\n")
	write(dirs[1], "tpl@.service", "[Install]\nAlso=tpl-helper.service\n")
	// The copy in /etc wins over the vendor's.
	write(dirs[1], "over.service", "[Install]\nAlso=vendor.service\n")
	write(dirs[0], "over.service", "[Install]\n")

	got := WithAlso(dirs, []string{"nm.service", "tpl@a.service", "over.service", "nofile.timer"})
	want := []string{"nm.service", "tpl@a.service", "over.service", "nofile.timer", "nm-dispatcher.service", "nm-wait.service", "other.socket", "tpl-helper.service"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	for in, want := range map[string]string{"a@b.service": "a@.service", "a@.service": "a@.service", "a.service": "a.service"} {
		if got := Template(in); got != want {
			t.Errorf("Template(%q) = %q, want %q", in, got, want)
		}
	}
}
