package system

import (
	"io"
	"maps"
	"os"
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

// A split package's own section replaces what pkgbase declares.
func TestParseSrcInfo(t *testing.T) {
	info := `pkgbase = waylock-arcus
	pkgver = 1.6.0
	provides = waylock
	conflicts = waylock
	depends = pam

pkgname = waylock-arcus

pkgname = waylock-arcus-debug
	conflicts = waylock-debug
`
	if got, want := parseSrcInfo(info, "waylock-arcus"), (Package{Provides: []string{"waylock"}, Conflicts: []string{"waylock"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("waylock-arcus = %v, want %v", got, want)
	}
	if got, want := parseSrcInfo(info, "waylock-arcus-debug"), (Package{Provides: []string{"waylock"}, Conflicts: []string{"waylock-debug"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("waylock-arcus-debug = %v, want %v", got, want)
	}
}
