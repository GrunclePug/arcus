package cli

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
)

// unitFile writes a system unit file under the test root's /usr/lib.
func (e *env) unitFile(name, content string) {
	e.t.Helper()
	dir := filepath.Join(e.root, "usr/lib/systemd/system")
	must(e.t, os.MkdirAll(dir, 0o755))
	must(e.t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

// mutations returns the calls in log that change the system: package
// transactions, builds, enabling or disabling units, group changes.
func mutations(log []string) []string {
	var out []string
	for _, l := range log {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "pacman", "yay", "paru", "aurlock":
			if !strings.HasPrefix(f[1], "-Q") && f[1] != "-Si" {
				out = append(out, l)
			}
		case "systemctl":
			if !slices.Contains(f, "list-unit-files") {
				out = append(out, l)
			}
		case "makepkg", "gpasswd":
			out = append(out, l)
		}
	}
	return out
}

func TestNewUnlisted(t *testing.T) {
	e := newEnv(t)
	me, err := user.Current()
	must(t, err)
	e.wlBase()
	e.module("net", "[units]\nsystem = [\"sshd.service\"]\n", nil) // off: doesn't count

	e.sys.SetPackages("base", "linux", "yay", "base-tools", "foot", "wmenu", "wlr-randr-git",
		"htop", "lib-needed", "orphan-dep", "my-aur", "hand-built", "aur-dep")
	e.sys.SetDeps("orphan-dep", "aur-dep")
	e.sys.SetRequires("htop lib-needed", "my-aur aur-dep")
	e.sys.SetForeign("wlr-randr-git", "my-aur", "hand-built", "aur-dep")

	e.sys.SetUnits(false, "seatd.service", "sshd.service", "getty@.service",
		"NetworkManager.service", "NetworkManager-dispatcher.service", "NetworkManager-wait-online.service",
		"a.service", "b.service", "foo.slice", "helper.service", `mnt-my\x2dshare.mount`)
	e.sys.SetPresets(false, "getty@.service")
	e.unitFile("NetworkManager.service", "[Install]\nWantedBy=multi-user.target\nAlso=NetworkManager-dispatcher.service\nAlso=NetworkManager-wait-online.service\n")
	e.unitFile("a.service", "[Install]\nAlso=b.service\n")
	e.unitFile("b.service", "[Install]\nAlso=a.service\n")
	// A unit arcus can't list still doesn't hide what it brings along.
	e.unitFile("foo.slice", "[Install]\nAlso=helper.service\n")
	e.sys.SetUnits(true, "mako.service", "syncthing.service")
	e.sys.SetGroups(me.Username, "wheel", "users", "video", "audio", "staff", "DomainUsers")
	e.sys.SetPrimaryGroup("staff")

	must(t, e.run("on", "-y", "wl-base"))
	must(t, e.run("new", "-unlisted", "mine"))
	out := e.out.String()
	for _, want := range []string{
		"Left out (arcus can't manage these names): foo.slice DomainUsers\n",
		": 2 pacman, 2 aur, 6 units, 1 groups.\n",
		"Review it, then 'arcus on mine'.\n",
		"The machine module isn't on",
		"'arcus rm mine'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	dir := filepath.Join(e.home, ".config/arcus/modules/mine")
	m, err := module.Load(dir)
	must(t, err)
	for _, c := range []struct {
		field     string
		got, want []string
	}{
		{"pacman", m.Packages.Pacman, []string{"htop", "lib-needed"}},
		{"aur", m.Packages.AUR, []string{"hand-built", "my-aur"}},
		{"units.system", m.Units.System, []string{"NetworkManager.service", "a.service", "helper.service", `mnt-my\x2dshare.mount`, "sshd.service"}},
		{"units.user", m.Units.User, []string{"syncthing.service"}},
		{"groups", m.Groups, []string{"audio"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if !strings.HasPrefix(e.read("home/u/.config/arcus/modules/mine/module.toml"), "# Written by 'arcus new -unlisted'") {
		t.Error("the module lacks its header")
	}

	// Turning it on only takes over what is there.
	before := len(e.sys.Log())
	must(t, e.run("on", "-y", "mine"))
	if m := mutations(e.sys.Log()[before:]); len(m) > 0 {
		t.Errorf("on mine changed the system: %v", m)
	}
	contains(t, e.state(true).Owned[state.Pacman]["mine"], "htop", "lib-needed")
	contains(t, e.state(false).Owned[state.AUR]["mine"], "hand-built", "my-aur") // the user half's
	contains(t, e.state(true).Owned[state.SystemUnits]["mine"], "NetworkManager.service", "a.service", "helper.service", `mnt-my\x2dshare.mount`, "sshd.service")
	lacks(t, e.state(true).Owned[state.SystemUnits]["mine"], "NetworkManager-dispatcher.service", "b.service")
	contains(t, e.state(false).Owned[state.UserUnits]["mine"], "syncthing.service")
	contains(t, e.state(true).Owned[state.Groups]["mine"], "audio")

	// Doctor reports only the orphan and the names arcus can't list.
	err = e.run("doctor")
	out = e.out.String()
	for _, want := range []string{
		"no module lists these and nothing needs them: orphan-dep\n",
		"system units enabled that no module lists: foo.slice\n",
		"groups no module lists: DomainUsers\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q (%v):\n%s", want, err, out)
		}
	}
	if n := strings.Count(out, "✗"); n != 2 {
		t.Errorf("doctor found %d problems, want 2 (foo.slice, DomainUsers):\n%s", n, out)
	}
	for _, unwanted := range []string{"user units enabled", "NetworkManager-", "staff", "audio", "htop", "other packages need them", "doesn't match"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("doctor mentions %q:\n%s", unwanted, out)
		}
	}
}

func TestNewUnlistedRefusals(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t)
		me, err := user.Current()
		must(t, err)
		e.sys.SetPackages("base", "htop")
		e.sys.SetGroups(me.Username)
		return e
	}
	dir := func(e *env) string { return filepath.Join(e.home, ".config/arcus/modules/mine") }

	t.Run("nothing unlisted", func(t *testing.T) {
		e := setup(t)
		e.sys.SetPackages("base")
		must(t, e.run("new", "-unlisted", "mine"))
		if !strings.Contains(e.out.String(), "Nothing arcus can manage is unlisted; no module written.\n") {
			t.Errorf("output:\n%s", e.out.String())
		}
		if _, err := os.Stat(dir(e)); !os.IsNotExist(err) {
			t.Errorf("a directory was created: %v", err)
		}
	})
	t.Run("only names arcus can't list", func(t *testing.T) {
		e := setup(t)
		e.sys.SetPackages("base")
		e.sys.SetUnits(false, "foo.slice")
		must(t, e.run("new", "-unlisted", "mine"))
		if out := e.out.String(); !strings.Contains(out, "Left out (arcus can't manage these names): foo.slice\n") || !strings.Contains(out, "no module written") {
			t.Errorf("output:\n%s", out)
		}
		if _, err := os.Stat(dir(e)); !os.IsNotExist(err) {
			t.Errorf("a directory was created: %v", err)
		}
	})
	t.Run("no host file", func(t *testing.T) {
		e := setup(t)
		e.sys.SetPackages("base", "linux", "yay", "htop", "my-aur")
		e.sys.SetForeign("yay", "my-aur")
		must(t, e.run("new", "-unlisted", "mine"))
		if !strings.Contains(e.out.String(), "The machine module isn't on") {
			t.Errorf("no machine hint:\n%s", e.out.String())
		}
		m, err := module.Load(dir(e))
		must(t, err)
		if !slices.Equal(m.Packages.Pacman, []string{"htop"}) || !slices.Equal(m.Packages.AUR, []string{"my-aur"}) {
			t.Errorf("pacman = %v, aur = %v; want [htop], [my-aur]", m.Packages.Pacman, m.Packages.AUR)
		}
	})
	t.Run("group names with spaces", func(t *testing.T) {
		e := setup(t)
		me, err := user.Current()
		must(t, err)
		e.sys.SetGroups(me.Username, "domain users", "audio")
		must(t, e.run("new", "-unlisted", "mine"))
		if !strings.Contains(e.out.String(), "Left out (arcus can't manage these names): domain users\n") {
			t.Errorf("output:\n%s", e.out.String())
		}
		m, err := module.Load(dir(e))
		must(t, err)
		if !slices.Equal(m.Groups, []string{"audio"}) {
			t.Errorf("groups = %v, want [audio]", m.Groups)
		}
	})
	t.Run("machine module on", func(t *testing.T) {
		e := setup(t)
		mdir := filepath.Join(e.root, "etc/arcus/modules/machine")
		must(t, os.MkdirAll(mdir, 0o755))
		must(t, os.WriteFile(filepath.Join(mdir, "module.toml"), []byte("[packages]\npacman = [\"grub\"]\n"), 0o644))
		must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"machine\"]\n"), 0o644))
		e.sys.SetPackages("base", "grub", "htop")
		must(t, e.run("new", "-unlisted", "mine"))
		if strings.Contains(e.out.String(), "machine module isn't on") {
			t.Errorf("hint with the machine module on:\n%s", e.out.String())
		}
		m, err := module.Load(dir(e))
		must(t, err)
		if !slices.Equal(m.Packages.Pacman, []string{"htop"}) {
			t.Errorf("pacman = %v, want [htop]", m.Packages.Pacman)
		}
	})
	for _, c := range []struct {
		name    string
		prepare func(e *env)
		args    []string
		want    string
	}{
		{"name of the machine module", nil, []string{"new", "-unlisted", "machine"}, "machine module's name"},
		{"plain new as machine", nil, []string{"new", "machine"}, "machine module's name"},
		{"existing module", func(e *env) { e.module("mine", "", nil) }, nil, "already exists"},
		{"existing profile", func(e *env) { e.profileFile("mine", "modules = []\n") }, nil, "already a profile"},
		{"draft directory", func(e *env) {
			must(t, os.MkdirAll(filepath.Join(dir(e), "home"), 0o755))
			must(t, os.WriteFile(filepath.Join(dir(e), "home/.bashrc"), []byte("x"), 0o644))
		}, nil, "already exists"},
		{"old host file", func(e *env) {
			old := filepath.Join(e.home, ".config/arcus/hosts/testhost.toml")
			must(t, os.MkdirAll(filepath.Dir(old), 0o755))
			must(t, os.WriteFile(old, nil, 0o644))
		}, nil, "host file moved"},
		{"no sync databases", func(e *env) { e.sys.NoSync() }, nil, "database file"},
		{"mv to machine", func(e *env) { e.module("mine", "", nil) }, []string{"mv", "mine", "machine"}, "machine module's name"},
		{"broken host file", func(e *env) { must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"nope\"]\n"), 0o644)) }, nil, "nope"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			if c.prepare != nil {
				c.prepare(e)
			}
			args := c.args
			if args == nil {
				args = []string{"new", "-unlisted", "mine"}
			}
			err := e.run(args...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v: got %v, want an error with %q", args, err, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir(e), "module.toml")); c.name != "existing module" && c.name != "mv to machine" && !os.IsNotExist(err) {
				t.Errorf("a module was written: %v", err)
			}
		})
	}
}

func TestCollapseAlso(t *testing.T) {
	dirs := []string{t.TempDir()}
	for name, content := range map[string]string{
		"NetworkManager.service": "[Install]\nAlso=NetworkManager-dispatcher.service NetworkManager-wait-online.service\n",
		"a.service":              "[Install]\nAlso=b.service\n",
		"b.service":              "[Install]\nAlso=c.service\n",
		"c.service":              "[Install]\nAlso=a.service\n",
		"root.service":           "[Install]\nAlso=mid.service\n",
		"mid.service":            "[Install]\nAlso=leaf.service\n",
		"tpl@.service":           "[Install]\nAlso=tpl-helper.service\n",
		"x.service":              "[Install]\nAlso=y.service\n",
		"y.service":              "[Install]\nAlso=x.service\n",
		"z.service":              "[Install]\nAlso=y.service\n",
	} {
		must(t, os.WriteFile(filepath.Join(dirs[0], name), []byte(content), 0o644))
	}
	for _, c := range []struct {
		name     string
		in, want []string
	}{
		{"NetworkManager", []string{"NetworkManager-dispatcher.service", "NetworkManager-wait-online.service", "NetworkManager.service"}, []string{"NetworkManager.service"}},
		{"cycle keeps the first", []string{"c.service", "b.service", "a.service"}, []string{"a.service"}},
		{"chain keeps the root", []string{"leaf.service", "mid.service", "root.service"}, []string{"root.service"}},
		{"instance", []string{"tpl@a.service", "tpl-helper.service"}, []string{"tpl@a.service"}},
		{"cycle reached from outside", []string{"x.service", "y.service", "z.service"}, []string{"z.service"}},
		{"unrelated stay", []string{"sshd.service", "a.service"}, []string{"sshd.service", "a.service"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := collapseAlso(dirs, c.in)
			slices.Sort(got)
			want := slices.Sorted(slices.Values(c.want))
			if !slices.Equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
			reversed := slices.Clone(c.in)
			slices.Reverse(reversed)
			again := collapseAlso(dirs, reversed)
			slices.Sort(again)
			if !slices.Equal(again, want) {
				t.Errorf("reversed input: got %v, want %v", again, want)
			}
		})
	}
}
