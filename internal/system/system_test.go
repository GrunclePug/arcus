package system

import (
	"errors"
	"io"
	iofs "io/fs"
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
	sysDirs := UnitDirs(sys.Root, sys.ConfigHome, filepath.Join(filepath.Dir(sys.ConfigHome), ".local/share"), false)
	userDirs := UnitDirs(sys.Root, sys.ConfigHome, filepath.Join(filepath.Dir(sys.ConfigHome), ".local/share"), true)
	if units, err := EnabledUnits(sys.Root, userDirs); err != nil || len(units) != 0 {
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
	units, err := EnabledUnits(sys.Root, sysDirs)
	must(err)
	if got := keys(units); !slices.Equal(got, []string{"bluetooth.service"}) {
		t.Errorf("system units = %v", got)
	}
	units, err = EnabledUnits(sys.Root, userDirs)
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

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func makeTestLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledUnits(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "home/u/.config")
	data := filepath.Join(root, "home/u/.local/share")
	sysDirs := UnitDirs(root, cfg, data, false)
	userDirs := UnitDirs(root, cfg, data, true)
	etcSys := sysDirs[0]
	libSys := sysDirs[len(sysDirs)-1]

	// Missing dirs[0] is an empty set, not an error.
	if got, err := EnabledUnits(root, sysDirs); err != nil || len(got) != 0 {
		t.Fatalf("missing system dir: %v, %v", got, err)
	}
	if got, err := EnabledUnits(root, nil); err != nil || len(got) != 0 {
		t.Fatalf("empty dirs: %v, %v", got, err)
	}

	// Vendor system units.
	for name, body := range map[string]string{
		"sshd.service":                      "[Install]\nWantedBy=multi-user.target\n",
		"req.service":                       "[Install]\nRequiredBy=multi-user.target\n",
		"uph.service":                       "[Install]\nUpheldBy=multi-user.target\n",
		"getty@.service":                    "[Install]\nAlias=autovt@.service\nWantedBy=getty.target\nDefaultInstance=tty1\n",
		"wg-quick@.service":                 "[Install]\nWantedBy=multi-user.target\n",
		"NetworkManager-dispatcher.service": "[Install]\nAlias=dbus-org.freedesktop.nm-dispatcher.service\n",
		"multi-user.target":                 "[Unit]\nDescription=Multi-User\n",
		"masked.service":                    "[Install]\nWantedBy=multi-user.target\n",
		"empty-masked.service":              "[Install]\nWantedBy=multi-user.target\n",
		"link-empty.service":                "[Install]\nWantedBy=multi-user.target\n",
		"chain-mask.service":                "[Install]\nWantedBy=multi-user.target\n",
		"brok-shadow.service":               "[Install]\nWantedBy=multi-user.target\n",
		"dep-mask.service":                  "[Install]\nWantedBy=multi-user.target\n",
		"dep-relmask.service":               "[Install]\nWantedBy=multi-user.target\n",
		"alias-masked.service":              "[Install]\nAlias=am.service\n",
		"real.service":                      "[Install]\nWantedBy=multi-user.target\n",
		"other.service":                     "[Install]\nWantedBy=multi-user.target\n",
		"libu.service":                      "[Install]\nAlias=libalias.service\n",
	} {
		writeTestFile(t, filepath.Join(libSys, name), body, 0o644)
	}
	// Merged-/usr /lib -> usr/lib symlink inside root.
	makeTestLink(t, "usr/lib", filepath.Join(root, "lib"))

	// Active enablement links: .wants, .requires, .upholds, template instances,
	// stock Arch getty@.service layout (autovt@.service + getty@tty1.service),
	// stale autovt@tty2.service, alias-only unit, merged-/usr alias,
	// mismatched .wants target (systemd loads by entry name), and linked external
	// alias-only unit.
	makeTestLink(t, "/usr/lib/systemd/system/sshd.service", filepath.Join(etcSys, "multi-user.target.wants/sshd.service"))
	makeTestLink(t, "/usr/lib/systemd/system/req.service", filepath.Join(etcSys, "multi-user.target.requires/req.service"))
	makeTestLink(t, "/usr/lib/systemd/system/uph.service", filepath.Join(etcSys, "multi-user.target.upholds/uph.service"))
	makeTestLink(t, "/usr/lib/systemd/system/getty@.service", filepath.Join(etcSys, "getty.target.wants/getty@tty1.service"))
	makeTestLink(t, "/usr/lib/systemd/system/getty@.service", filepath.Join(etcSys, "autovt@.service"))
	makeTestLink(t, "/usr/lib/systemd/system/getty@.service", filepath.Join(etcSys, "autovt@tty2.service"))
	makeTestLink(t, "/usr/lib/systemd/system/wg-quick@.service", filepath.Join(etcSys, "multi-user.target.wants/wg-quick@wg0.service"))
	makeTestLink(t, "/usr/lib/systemd/system/wg-quick@.service", filepath.Join(etcSys, "multi-user.target.wants/wg-quick@.service")) // bare template in wants: ignored
	makeTestLink(t, "/usr/lib/systemd/system/NetworkManager-dispatcher.service", filepath.Join(etcSys, "dbus-org.freedesktop.nm-dispatcher.service"))
	makeTestLink(t, "/lib/systemd/system/libu.service", filepath.Join(etcSys, "libalias.service"))
	makeTestLink(t, "/usr/lib/systemd/system/other.service", filepath.Join(etcSys, "multi-user.target.wants/real.service"))

	// Alias-only linked external unit: ext.service -> /opt/ext/ext.service,
	// alias extalias.service -> /etc/systemd/system/ext.service.
	writeTestFile(t, filepath.Join(root, "opt/ext/ext.service"), "[Install]\nAlias=extalias.service\n", 0o644)
	makeTestLink(t, "/opt/ext/ext.service", filepath.Join(etcSys, "ext.service"))
	makeTestLink(t, "/etc/systemd/system/ext.service", filepath.Join(etcSys, "extalias.service"))

	// External linked fragment with a different name that happens to match a
	// vendor unit basename must NOT be misclassified as an alias of that vendor unit.
	writeTestFile(t, filepath.Join(root, "opt/ext/alias-masked.service"), "[Install]\nAlias=not-an-alias.service\n", 0o644)
	makeTestLink(t, "/opt/ext/alias-masked.service", filepath.Join(etcSys, "not-an-alias.service"))

	// Non-Alias= top-level link (systemctl set-default) must be ignored.
	makeTestLink(t, "/usr/lib/systemd/system/multi-user.target", filepath.Join(etcSys, "default.target"))

	// Dangling .wants link (uninstalled package) must be ignored.
	makeTestLink(t, "/usr/lib/systemd/system/gone.service", filepath.Join(etcSys, "multi-user.target.wants/gone.service"))

	// Symlinked .wants directory (not IsDir()) is safely ignored.
	writeTestFile(t, filepath.Join(root, "opt/fakewants/target.service"), "[Unit]\n", 0o644)
	makeTestLink(t, "/opt/fakewants", filepath.Join(etcSys, "linked.target.wants"))

	// Masks: /dev/null, 0-byte file, symlink to 0-byte file, chained symlink to /dev/null,
	// broken symlink in /etc shadowing /usr/lib, masked exact instance shadowing template,
	// dep link to /dev/null and relative ../../../dev/null, and masked alias target.
	makeTestLink(t, "/dev/null", filepath.Join(etcSys, "masked.service"))
	makeTestLink(t, "/usr/lib/systemd/system/masked.service", filepath.Join(etcSys, "multi-user.target.wants/masked.service"))
	writeTestFile(t, filepath.Join(etcSys, "empty-masked.service"), "", 0o644)
	makeTestLink(t, "/usr/lib/systemd/system/empty-masked.service", filepath.Join(etcSys, "multi-user.target.wants/empty-masked.service"))
	writeTestFile(t, filepath.Join(root, "opt/zero.service"), "", 0o644)
	makeTestLink(t, "/opt/zero.service", filepath.Join(etcSys, "link-empty.service"))
	makeTestLink(t, "/usr/lib/systemd/system/link-empty.service", filepath.Join(etcSys, "multi-user.target.wants/link-empty.service"))
	makeTestLink(t, "/dev/null", filepath.Join(root, "opt/to-null"))
	makeTestLink(t, "/opt/to-null", filepath.Join(etcSys, "chain-mask.service"))
	makeTestLink(t, "/usr/lib/systemd/system/chain-mask.service", filepath.Join(etcSys, "multi-user.target.wants/chain-mask.service"))
	makeTestLink(t, "/nonexistent/brok-shadow.service", filepath.Join(etcSys, "brok-shadow.service"))
	makeTestLink(t, "/usr/lib/systemd/system/brok-shadow.service", filepath.Join(etcSys, "multi-user.target.wants/brok-shadow.service"))
	makeTestLink(t, "/dev/null", filepath.Join(etcSys, "wg-quick@wg1.service"))
	makeTestLink(t, "/usr/lib/systemd/system/wg-quick@.service", filepath.Join(etcSys, "multi-user.target.wants/wg-quick@wg1.service"))
	makeTestLink(t, "/dev/null", filepath.Join(etcSys, "multi-user.target.wants/dep-mask.service"))
	makeTestLink(t, "../../../../../../dev/null", filepath.Join(etcSys, "multi-user.target.wants/dep-relmask.service"))
	makeTestLink(t, "/dev/null", filepath.Join(etcSys, "alias-masked.service"))
	makeTestLink(t, "/usr/lib/systemd/system/alias-masked.service", filepath.Join(etcSys, "am.service"))

	got, err := EnabledUnits(root, sysDirs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"NetworkManager-dispatcher.service",
		"ext.service",
		"getty@tty1.service",
		"libu.service",
		"real.service",
		"req.service",
		"sshd.service",
		"uph.service",
		"wg-quick@wg0.service",
	}
	if !slices.Equal(keys(got), want) {
		t.Errorf("system EnabledUnits = %v\nwant %v", keys(got), want)
	}

	// User scope: ignores /etc/systemd/user/*.wants, sees ~/.config/systemd/user/*.wants,
	// respects /etc/systemd/user masks, sees /usr/share/systemd/user units, and does
	// not treat same-name home/-deployed unit links as enabled without a .wants link.
	writeTestFile(t, filepath.Join(root, "usr/lib/systemd/user/pipewire.socket"), "[Install]\nWantedBy=sockets.target\n", 0o644)
	writeTestFile(t, filepath.Join(root, "usr/lib/systemd/user/syncthing.service"), "[Install]\nWantedBy=default.target\n", 0o644)
	writeTestFile(t, filepath.Join(root, "usr/lib/systemd/user/globally-masked.service"), "[Install]\nWantedBy=default.target\n", 0o644)
	writeTestFile(t, filepath.Join(root, "usr/share/systemd/user/shared-app.service"), "[Install]\nWantedBy=default.target\n", 0o644)
	writeTestFile(t, filepath.Join(root, "home/u/.config/arcus/modules/m/home/.config/systemd/user/home-unit.service"), "[Install]\nWantedBy=default.target\n", 0o644)

	makeTestLink(t, "/usr/lib/systemd/user/pipewire.socket", filepath.Join(root, "etc/systemd/user/sockets.target.wants/pipewire.socket"))
	makeTestLink(t, "/dev/null", filepath.Join(root, "etc/systemd/user/globally-masked.service"))
	makeTestLink(t, "/usr/lib/systemd/user/syncthing.service", filepath.Join(cfg, "systemd/user/default.target.wants/syncthing.service"))
	makeTestLink(t, "/usr/lib/systemd/user/globally-masked.service", filepath.Join(cfg, "systemd/user/default.target.wants/globally-masked.service"))
	makeTestLink(t, "/usr/share/systemd/user/shared-app.service", filepath.Join(cfg, "systemd/user/default.target.wants/shared-app.service"))
	makeTestLink(t, filepath.Join(root, "home/u/.config/arcus/modules/m/home/.config/systemd/user/home-unit.service"), filepath.Join(cfg, "systemd/user/home-unit.service"))

	gotUser, err := EnabledUnits(root, userDirs)
	if err != nil {
		t.Fatal(err)
	}
	if wantUser := []string{"shared-app.service", "syncthing.service"}; !slices.Equal(keys(gotUser), wantUser) {
		t.Errorf("user EnabledUnits = %v, want %v", keys(gotUser), wantUser)
	}

	// Enabling the home/-deployed unit via .wants makes it visible.
	makeTestLink(t, filepath.Join(cfg, "systemd/user/home-unit.service"), filepath.Join(cfg, "systemd/user/default.target.wants/home-unit.service"))
	gotUser, err = EnabledUnits(root, userDirs)
	if err != nil {
		t.Fatal(err)
	}
	if wantUser := []string{"home-unit.service", "shared-app.service", "syncthing.service"}; !slices.Equal(keys(gotUser), wantUser) {
		t.Errorf("user EnabledUnits with home unit = %v, want %v", keys(gotUser), wantUser)
	}
}

func TestEnabledUnitsPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks don't restrict root")
	}
	root := t.TempDir()
	dirs := UnitDirs(root, filepath.Join(root, "home/u/.config"), filepath.Join(root, "home/u/.local/share"), false)
	etcSys := dirs[0]

	// A 0600 unit file with a .wants link is still seen (unitFile is stat-only),
	// and a 0600 alias-only unit is skipped gracefully without aborting inventory.
	writeTestFile(t, filepath.Join(etcSys, "secret.service"), "[Install]\nWantedBy=multi-user.target\nAlias=secret-alias.service\n", 0o600)
	if err := os.Chmod(filepath.Join(etcSys, "secret.service"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(etcSys, "secret.service"), 0o644) })
	makeTestLink(t, "/etc/systemd/system/secret.service", filepath.Join(etcSys, "multi-user.target.wants/secret.service"))
	makeTestLink(t, "/etc/systemd/system/secret.service", filepath.Join(etcSys, "secret-alias.service"))

	writeTestFile(t, filepath.Join(etcSys, "alias-only-secret.service"), "[Install]\nAlias=aos.service\n", 0o600)
	if err := os.Chmod(filepath.Join(etcSys, "alias-only-secret.service"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(etcSys, "alias-only-secret.service"), 0o644) })
	makeTestLink(t, "/etc/systemd/system/alias-only-secret.service", filepath.Join(etcSys, "aos.service"))

	// An unrelated top-level link whose parent traverses an unreadable external
	// directory must not abort inventory with EACCES.
	privDir := filepath.Join(root, "root/private")
	writeTestFile(t, filepath.Join(privDir, "nested/ext.service"), "[Unit]\n", 0o644)
	if err := os.Chmod(privDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(privDir, 0o755) })
	makeTestLink(t, "/root/private/nested/ext.service", filepath.Join(etcSys, "priv-ext.service"))

	got, err := EnabledUnits(root, dirs)
	if err != nil {
		t.Fatalf("0600 units must not fail inventory: %v", err)
	}
	if want := []string{"secret.service"}; !slices.Equal(keys(got), want) {
		t.Errorf("got %v, want %v", keys(got), want)
	}

	// An unreadable enablement directory itself IS an error.
	badRoot := t.TempDir()
	badDirs := UnitDirs(badRoot, filepath.Join(badRoot, "home/u/.config"), filepath.Join(badRoot, "home/u/.local/share"), false)
	if err := os.MkdirAll(badDirs[0], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(badDirs[0], 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(badDirs[0], 0o755) })
	if _, err := EnabledUnits(badRoot, badDirs); err == nil {
		t.Error("unreadable dirs[0] must return an error")
	}
}

func TestResolveInRoot(t *testing.T) {
	// Root "/" handles real absolute paths and /dev/null without double slashes.
	tmp := t.TempDir()
	realFile := filepath.Join(tmp, "a.service")
	writeTestFile(t, realFile, "[Unit]\n", 0o644)
	linkFile := filepath.Join(tmp, "link.service")
	makeTestLink(t, realFile, linkFile)
	resolved, fi, devNull, err := resolveInRoot("/", linkFile)
	if err != nil || devNull || fi == nil || !fi.Mode().IsRegular() || filepath.Base(resolved) != "a.service" {
		t.Fatalf("root=/ link: resolved=%q fi=%v devNull=%v err=%v", resolved, fi, devNull, err)
	}
	nullLink := filepath.Join(tmp, "mask.service")
	makeTestLink(t, "/dev/null", nullLink)
	if _, _, devNull, err := resolveInRoot("/", nullLink); err != nil || !devNull {
		t.Fatalf("root=/ mask: devNull=%v err=%v", devNull, err)
	}

	// Sandboxed root clamps relative .. escapes and stays inside root.
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "etc/target"), "inside", 0o644)
	esc := filepath.Join(root, "a/b/esc")
	makeTestLink(t, "../../../../../../../../etc/target", esc)
	resolved, fi, devNull, err = resolveInRoot(root, esc)
	if err != nil || devNull || resolved != filepath.Join(root, "etc/target") || fi.Size() != 6 {
		t.Fatalf("clamped escape: resolved=%q devNull=%v err=%v", resolved, devNull, err)
	}
	// Symlinked parent directory inside root resolves inside root.
	makeTestLink(t, "/etc", filepath.Join(root, "dirlink"))
	resolved, _, devNull, err = resolveInRoot(root, filepath.Join(root, "dirlink/target"))
	if err != nil || devNull || resolved != filepath.Join(root, "etc/target") {
		t.Fatalf("parent symlink: resolved=%q devNull=%v err=%v", resolved, devNull, err)
	}
}

func TestEnabledUnitsWithSystemctl(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl is not installed")
	}
	root := t.TempDir()
	libSys := filepath.Join(root, "usr/lib/systemd/system")
	etcSys := filepath.Join(root, "etc/systemd/system")
	if err := os.MkdirAll(etcSys, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"getty@.service":  "[Install]\nAlias=autovt@.service\nWantedBy=getty.target\nDefaultInstance=tty1\n",
		"wg@.service":     "[Install]\nWantedBy=multi-user.target\n",
		"nm.service":      "[Install]\nWantedBy=multi-user.target\nAlso=nm-disp.service\n",
		"nm-disp.service": "[Install]\nAlias=dbus-nm-disp.service\n",
		"qalias.service":  "[Install]\nAlias=\"quoted-alias.service\" 'single-alias.service'\n",
		"escaped.service": "[Install]\nAlias=\"quoted\\x2dalias.service\"\n",
	} {
		writeTestFile(t, filepath.Join(libSys, name), body, 0o644)
	}
	// Alias-only linked external unit file.
	writeTestFile(t, filepath.Join(root, "opt/ext/ext.service"), "[Install]\nAlias=ext-alias.service\n", 0o644)
	makeTestLink(t, "/opt/ext/ext.service", filepath.Join(etcSys, "ext.service"))

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("systemctl", append([]string{"--root=" + root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("systemctl %v: %v\n%s", args, err, out)
		}
	}
	run("enable", "getty@.service")
	run("enable", "getty@tty2.service")
	run("enable", "wg@0.service")
	run("enable", "wg@1.service")
	run("enable", "nm.service")
	run("enable", "ext.service")
	run("enable", "qalias.service")
	run("enable", "escaped.service")

	dirs := UnitDirs(root, filepath.Join(root, "home/u/.config"), filepath.Join(root, "home/u/.local/share"), false)
	got, err := EnabledUnits(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"escaped.service", "ext.service", "getty@tty1.service", "getty@tty2.service", "nm-disp.service", "nm.service", "qalias.service", "wg@0.service", "wg@1.service"}
	if !slices.Equal(keys(got), want) {
		t.Fatalf("after enable: got %v, want %v", keys(got), want)
	}

	// Disabling one instance leaves sibling instances intact, and any stale
	// instance alias (autovt@tty2.service) is ignored.
	run("disable", "getty@tty2.service")
	run("disable", "wg@0.service")
	got, err = EnabledUnits(root, dirs)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"escaped.service", "ext.service", "getty@tty1.service", "nm-disp.service", "nm.service", "qalias.service", "wg@1.service"}
	if !slices.Equal(keys(got), want) {
		t.Fatalf("after disable: got %v, want %v", keys(got), want)
	}
}

func TestWithAlso(t *testing.T) {
	root := t.TempDir()
	dirs := UnitDirs(root, filepath.Join(root, "home/.config"), filepath.Join(root, "home/.local/share"), false)
	write := func(dir, name, content string) {
		t.Helper()
		writeTestFile(t, filepath.Join(dir, name), content, 0o644)
	}
	write(dirs[2], "nm.service", "[Unit]\nAlso=not-install.service\n[Install]\n; semicolon comment\n# hash comment\nWantedBy=multi-user.target\nAlso=nm-dispatcher.service\nAlso = nm-wait.service other.socket\nAlso=\n")
	write(dirs[2], "other.socket", "[Install]\nAlso=nm.service\n")
	write(dirs[2], "tpl@.service", "[Install]\nAlso=tpl-helper.service tpl-sub@%i.service skip@%p.service lit%%@%i.service\n")
	// The copy in /etc wins over the vendor's.
	write(dirs[2], "over.service", "[Install]\nAlso=vendor.service\n")
	write(dirs[0], "over.service", "[Install]\n")
	// Masked unit adds nothing.
	write(dirs[2], "masked.service", "[Install]\nAlso=should-not-follow.service\n")
	makeTestLink(t, "/dev/null", filepath.Join(dirs[0], "masked.service"))
	// Modules deploy their own units to /usr/local.
	write(dirs[1], "local.service", "[Install]\nAlso=local.timer\n")
	// Order-sensitive Also= around DefaultInstance= in a bare template reached via Also=,
	// reading the template directly (ignoring an exact instance override).
	write(dirs[2], "parent.service", "[Install]\nAlso=order@.service\n")
	write(dirs[2], "order@.service", "[Install]\nAlso=before@%i.service\nDefaultInstance=one\nAlso=after@%i.service\n")
	write(dirs[0], "order@one.service", "[Install]\nAlso=override-only.service\n")
	// DefaultInstance reset and DefaultInstance=- (like xfs_healer@.service).
	write(dirs[2], "dash-parent.service", "[Install]\nAlso=dash@.service\n")
	write(dirs[2], "dash@.service", "[Install]\nDefaultInstance=tmp\nDefaultInstance=\nDefaultInstance=-\n")
	// Bare template without DefaultInstance= still walks its plain Also= entries.
	write(dirs[2], "nodef-parent.service", "[Install]\nAlso=nodef@.service\n")
	write(dirs[2], "nodef@.service", "[Install]\nAlso=nodef-helper.service nodef-sub@%i.service\n")

	got, err := WithAlso(root, dirs, []string{"nm.service", "tpl@a.service", "over.service", "masked.service", "nofile.timer", "local.service"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nm.service", "tpl@a.service", "over.service", "masked.service", "nofile.timer", "local.service", "nm-dispatcher.service", "nm-wait.service", "other.socket", "tpl-helper.service", "tpl-sub@a.service", "lit%@a.service", "local.timer"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}

	gotOrder, err := WithAlso(root, dirs, []string{"parent.service"})
	if err != nil {
		t.Fatal(err)
	}
	if wantOrder := []string{"parent.service", "order@one.service", "after@one.service"}; !slices.Equal(gotOrder, wantOrder) {
		t.Errorf("order-sensitive template: got %v, want %v", gotOrder, wantOrder)
	}
	// When order@one.service is also declared directly, its exact override is walked too.
	gotBoth, err := WithAlso(root, dirs, []string{"parent.service", "order@one.service"})
	if err != nil {
		t.Fatal(err)
	}
	if wantBoth := []string{"parent.service", "order@one.service", "override-only.service", "after@one.service"}; !slices.Equal(gotBoth, wantBoth) {
		t.Errorf("parent + direct instance: got %v, want %v", gotBoth, wantBoth)
	}

	gotDash, err := WithAlso(root, dirs, []string{"dash-parent.service"})
	if err != nil {
		t.Fatal(err)
	}
	if wantDash := []string{"dash-parent.service", "dash@-.service"}; !slices.Equal(gotDash, wantDash) {
		t.Errorf("dash default instance: got %v, want %v", gotDash, wantDash)
	}

	gotNoDef, err := WithAlso(root, dirs, []string{"nodef-parent.service"})
	if err != nil {
		t.Fatal(err)
	}
	if wantNoDef := []string{"nodef-parent.service", "nodef-helper.service"}; !slices.Equal(gotNoDef, wantNoDef) {
		t.Errorf("no-default template Also=: got %v, want %v", gotNoDef, wantNoDef)
	}

	// A user's copy in ~/.local/share wins over the vendor's.
	userDirs := UnitDirs(root, filepath.Join(root, "home/.config"), filepath.Join(root, "home/.local/share"), true)
	write(userDirs[len(userDirs)-1], "u.service", "[Install]\nAlso=u-helper.service\n")
	write(userDirs[2], "u.service", "[Install]\n")
	if got, err := WithAlso(root, userDirs, []string{"u.service"}); err != nil || !slices.Equal(got, []string{"u.service"}) {
		t.Errorf("user: got %v (%v), want [u.service]", got, err)
	}

	if os.Geteuid() != 0 {
		secretPath := filepath.Join(dirs[0], "secret-also.service")
		writeTestFile(t, secretPath, "[Install]\nAlso=hidden.service\n", 0o600)
		if err := os.Chmod(secretPath, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(secretPath, 0o644) })
		if got, err := WithAlso(root, dirs, []string{"secret-also.service"}); err != nil || !slices.Equal(got, []string{"secret-also.service"}) {
			t.Errorf("0600 unit in WithAlso: got %v (%v)", got, err)
		}
	}

	for in, want := range map[string]string{"a@b.service": "a@.service", "a@.service": "a@.service", "a.service": "a.service"} {
		if got := Template(in); got != want {
			t.Errorf("Template(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]bool{
		"a@.service":     true,
		"a.b@.service":   true,
		"a@b.service":    false,
		"a@.bar.service": false,
		"a.service":      false,
		"foo.bar@":       false,
		"@.service":      false,
		"foo@.":          false,
	} {
		if got := IsTemplate(in); got != want {
			t.Errorf("IsTemplate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDisableClosureAndInstallable(t *testing.T) {
	root := t.TempDir()
	dirs := UnitDirs(root, filepath.Join(root, "home/.config"), filepath.Join(root, "home/.local/share"), false)
	etcDir, libDir := dirs[0], dirs[2]
	write := func(dir, name, content string) {
		t.Helper()
		writeTestFile(t, filepath.Join(dir, name), content, 0o644)
	}

	// Chain through unenabled Also=-only intermediate unit and mutual cycle.
	write(libDir, "top.service", "[Install]\nWantedBy=multi-user.target\nAlso=mid.service\n")
	write(libDir, "mid.service", "[Install]\nAlso=leaf.service\n")
	write(libDir, "leaf.service", "[Install]\nWantedBy=multi-user.target\n")
	write(libDir, "cyc-a.service", "[Install]\nWantedBy=multi-user.target\nAlso=cyc-b.service\n")
	write(libDir, "cyc-b.service", "[Install]\nWantedBy=multi-user.target\nAlso=cyc-a.service\n")

	// Bare template with DefaultInstance=, %i sub-unit, and exact override on another instance.
	write(libDir, "worker@.service", "[Install]\nWantedBy=multi-user.target\nDefaultInstance=one\nAlso=sub@%i.service\n")
	write(libDir, "sub@.service", "[Install]\nWantedBy=multi-user.target\n")
	write(etcDir, "worker@two.service", "[Install]\nAlso=override-leaf.service\n")
	write(libDir, "caller-tpl.service", "[Install]\nWantedBy=multi-user.target\nAlso=worker@.service\n")

	// Masked bare template (skipped) vs missing bare template (still catches exact instances).
	write(libDir, "masked-tpl@.service", "[Install]\nWantedBy=multi-user.target\n")
	makeTestLink(t, "/dev/null", filepath.Join(etcDir, "masked-tpl@.service"))
	write(etcDir, "masked-tpl@exact.service", "[Install]\nWantedBy=multi-user.target\n")
	write(etcDir, "missing-tpl@exact.service", "[Install]\nWantedBy=multi-user.target\n")
	write(libDir, "caller-mask-miss.service", "[Install]\nWantedBy=multi-user.target\nAlso=masked-tpl@.service missing-tpl@.service\n")

	// Static bare template does not enter ByEnable.
	write(libDir, "static@.service", "[Install]\nDefaultInstance=def\n")
	write(libDir, "caller-static-tpl.service", "[Install]\nWantedBy=multi-user.target\nAlso=static@.service\n")

	// Installable fixtures.
	write(libDir, "alias-only.service", "[Install]\nAlias=ao.service\n")
	write(libDir, "cleared.service", "[Install]\nWantedBy=multi-user.target\nWantedBy=\n")
	write(libDir, "also-only.service", "[Install]\nAlso=leaf.service\n")
	write(libDir, "tpl-alias-only@.service", "[Install]\nAlias=tao@%i.service\n")
	write(libDir, "getty@.service", "[Install]\nAlias=autovt@.service\nWantedBy=getty.target\nDefaultInstance=tty1\n")

	// In-tree alias vs external linked unit and linked bare template in dirs[0].
	makeTestLink(t, "/usr/lib/systemd/system/leaf.service", filepath.Join(etcDir, "alias-leaf.service"))
	writeTestFile(t, filepath.Join(root, "opt/ext/linked.service"), "[Install]\nWantedBy=multi-user.target\n", 0o644)
	makeTestLink(t, "/opt/ext/linked.service", filepath.Join(etcDir, "linked.service"))
	writeTestFile(t, filepath.Join(root, "opt/ext/ltpl@.service"), "[Install]\nWantedBy=multi-user.target\n", 0o644)
	makeTestLink(t, "/opt/ext/ltpl@.service", filepath.Join(etcDir, "ltpl@.service"))
	write(etcDir, "ltpl@exact.service", "[Install]\nWantedBy=multi-user.target\n")

	// Chain through unenabled Also=-only intermediate.
	imp, err := DisableClosure(root, dirs, map[string]bool{"leaf.service": true}, "top.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"leaf.service", "mid.service"}; !slices.Equal(imp.Units, want) {
		t.Errorf("top Units = %v, want %v", imp.Units, want)
	}
	if want := []string{"leaf.service"}; !slices.Equal(imp.ByEnable, want) {
		t.Errorf("top ByEnable = %v, want %v (mid is Also=-only)", imp.ByEnable, want)
	}

	// Mutual cycle excludes starting unit and terminates.
	impCyc, err := DisableClosure(root, dirs, map[string]bool{"cyc-a.service": true, "cyc-b.service": true}, "cyc-a.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cyc-b.service"}; !slices.Equal(impCyc.Units, want) {
		t.Errorf("cyc-a Units = %v, want %v", impCyc.Units, want)
	}

	// Bare template matches enabled instances without recursing into override.
	impTpl, err := DisableClosure(root, dirs, map[string]bool{
		"worker@one.service": true, "worker@two.service": true,
		"sub@one.service": true, "sub@two.service": true, "override-leaf.service": true,
	}, "caller-tpl.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sub@one.service", "worker@one.service", "worker@two.service"}; !slices.Equal(impTpl.Units, want) {
		t.Errorf("caller-tpl Units = %v, want %v", impTpl.Units, want)
	}
	if want := []string{"sub@one.service", "worker@one.service"}; !slices.Equal(impTpl.ByEnable, want) {
		t.Errorf("caller-tpl ByEnable = %v, want %v", impTpl.ByEnable, want)
	}
	if want := []string{"worker@one.service", "worker@two.service"}; !slices.Equal(impTpl.Templates["worker@.service"], want) {
		t.Errorf("caller-tpl Templates = %v, want %v", impTpl.Templates, want)
	}

	// Masked bare template is skipped; missing bare template still catches exact instances.
	impMaskMiss, err := DisableClosure(root, dirs, map[string]bool{
		"masked-tpl@exact.service": true, "missing-tpl@exact.service": true,
	}, "caller-mask-miss.service")
	if err != nil {
		t.Fatal(err)
	}
	if _, hasMasked := impMaskMiss.Templates["masked-tpl@.service"]; hasMasked {
		t.Errorf("masked bare template should be skipped: %v", impMaskMiss.Templates)
	}
	if want := []string{"missing-tpl@exact.service"}; !slices.Equal(impMaskMiss.Templates["missing-tpl@.service"], want) {
		t.Errorf("missing bare template Templates = %v, want %v", impMaskMiss.Templates, want)
	}

	// Static bare template does not exempt its DefaultInstance= via ByEnable.
	impStatic, err := DisableClosure(root, dirs, map[string]bool{"static@def.service": true}, "caller-static-tpl.service")
	if err != nil {
		t.Fatal(err)
	}
	if len(impStatic.ByEnable) != 0 {
		t.Errorf("static template ByEnable = %v, want empty", impStatic.ByEnable)
	}

	// Installable checks.
	for unit, want := range map[string]bool{
		"leaf.service":             true,
		"alias-only.service":       true,
		"getty@tty1.service":       true,
		"cleared.service":          false,
		"also-only.service":        false,
		"tpl-alias-only@x.service": false,
		"missing.service":          false,
	} {
		got, err := Installable(root, dirs, unit)
		if err != nil {
			t.Fatalf("Installable(%q): %v", unit, err)
		}
		if got != want {
			t.Errorf("Installable(%q) = %v, want %v", unit, got, want)
		}
	}

	// IsLinkedUnit checks (concrete instances do not inherit linked bare template).
	for unit, want := range map[string]bool{
		"linked.service":     true,
		"ltpl@.service":      true,
		"ltpl@exact.service": false,
		"ltpl@inst.service":  false,
		"leaf.service":       false,
		"alias-leaf.service": false,
	} {
		got, err := IsLinkedUnit(root, dirs, unit)
		if err != nil {
			t.Fatalf("IsLinkedUnit(%q): %v", unit, err)
		}
		if got != want {
			t.Errorf("IsLinkedUnit(%q) = %v, want %v", unit, got, want)
		}
	}

	if os.Geteuid() != 0 {
		secretUnit := filepath.Join(etcDir, "secret-inst.service")
		writeTestFile(t, secretUnit, "[Install]\nWantedBy=multi-user.target\n", 0o600)
		if err := os.Chmod(secretUnit, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(secretUnit, 0o644) })
		if _, err := Installable(root, dirs, "secret-inst.service"); !errors.Is(err, iofs.ErrPermission) {
			t.Errorf("Installable(0000 unit) err = %v, want ErrPermission", err)
		}
	}
}

func TestDisableClosureWithSystemctl(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl is not installed")
	}
	root := t.TempDir()
	libSys := filepath.Join(root, "usr/lib/systemd/system")
	etcSys := filepath.Join(root, "etc/systemd/system")
	if err := os.MkdirAll(etcSys, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := UnitDirs(root, filepath.Join(root, "home/u/.config"), filepath.Join(root, "home/u/.local/share"), false)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("systemctl", append([]string{"--root=" + root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("systemctl %v: %v\n%s", args, err, out)
		}
	}
	writeLib := func(name, body string) {
		t.Helper()
		writeTestFile(t, filepath.Join(libSys, name), body, 0o644)
	}

	// 1. Chain through unenabled Also=-only intermediate.
	writeLib("top.service", "[Install]\nWantedBy=multi-user.target\nAlso=mid.service\n")
	writeLib("mid.service", "[Install]\nAlso=leaf.service\n")
	writeLib("leaf.service", "[Install]\nWantedBy=multi-user.target\n")

	// 2. Bare template with default, non-default sibling, %i sub, and override.
	writeLib("worker@.service", "[Install]\nWantedBy=multi-user.target\nDefaultInstance=one\nAlso=sub@%i.service\n")
	writeLib("sub@.service", "[Install]\nWantedBy=multi-user.target\n")
	writeLib("override-leaf.service", "[Install]\nWantedBy=multi-user.target\n")
	writeTestFile(t, filepath.Join(etcSys, "worker@two.service"), "[Install]\nWantedBy=multi-user.target\nAlso=override-leaf.service\n", 0o644)
	writeLib("caller-tpl.service", "[Install]\nWantedBy=multi-user.target\nAlso=worker@.service\n")

	// 3. Masked bare template leaves exact instance alone; missing bare template removes it.
	writeLib("mtpl@.service", "[Install]\nWantedBy=multi-user.target\n")
	makeTestLink(t, "/dev/null", filepath.Join(etcSys, "mtpl@.service"))
	writeTestFile(t, filepath.Join(etcSys, "mtpl@exact.service"), "[Install]\nWantedBy=multi-user.target\n", 0o644)
	writeTestFile(t, filepath.Join(etcSys, "misstpl@exact.service"), "[Install]\nWantedBy=multi-user.target\n", 0o644)
	writeLib("caller-mask.service", "[Install]\nWantedBy=multi-user.target\nAlso=mtpl@.service misstpl@.service\n")

	run("enable", "top.service", "caller-tpl.service", "worker@two.service", "sub@two.service", "mtpl@exact.service", "misstpl@exact.service", "caller-mask.service")

	for _, target := range []string{"top.service", "caller-tpl.service", "caller-mask.service"} {
		pre, err := EnabledUnits(root, dirs)
		if err != nil {
			t.Fatal(err)
		}
		imp, err := DisableClosure(root, dirs, pre, target)
		if err != nil {
			t.Fatalf("DisableClosure(%q): %v", target, err)
		}
		run("disable", target)
		post, err := EnabledUnits(root, dirs)
		if err != nil {
			t.Fatal(err)
		}
		var removed []string
		for u := range pre {
			if !post[u] && u != target {
				removed = append(removed, u)
			}
		}
		slices.Sort(removed)
		var predicted []string
		for _, u := range imp.Units {
			if pre[u] {
				predicted = append(predicted, u)
			}
		}
		if !slices.Equal(predicted, removed) {
			t.Errorf("disable %s: DisableClosure predicted %v, real systemctl removed %v", target, predicted, removed)
		}
	}

	// External linked unit: real systemctl disable deletes its top-level link and IsLinkedUnit detects it.
	writeTestFile(t, filepath.Join(root, "opt/mod/ext.service"), "[Install]\nWantedBy=multi-user.target\n", 0o644)
	makeTestLink(t, "/opt/mod/ext.service", filepath.Join(etcSys, "ext.service"))
	wants := filepath.Join(etcSys, "multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	makeTestLink(t, "/etc/systemd/system/ext.service", filepath.Join(wants, "ext.service"))
	writeLib("caller-ext.service", "[Install]\nWantedBy=multi-user.target\nAlso=ext.service\n")
	run("enable", "caller-ext.service")
	if linked, err := IsLinkedUnit(root, dirs, "ext.service"); err != nil || !linked {
		t.Fatalf("IsLinkedUnit(ext.service) = %v, %v, want true", linked, err)
	}
	run("disable", "caller-ext.service")
	if _, err := os.Lstat(filepath.Join(etcSys, "ext.service")); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("expected real systemctl disable to delete external link ext.service, lstat err = %v", err)
	}
}
