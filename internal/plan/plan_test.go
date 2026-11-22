package plan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// fixture builds modules a (on), b (new) and c (on, being turned off) with
// a home layer where a and c are already deployed.
func fixture(t *testing.T) (Input, Half, string) {
	t.Helper()
	root := t.TempDir()
	modules := module.Set{}
	dirs := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		dir := filepath.Join(root, "modules", name)
		if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
			t.Fatal(err)
		}
		modules[name] = &module.Module{Name: name, Dir: dir}
		dirs[name] = dir
	}
	modules["a"].Packages.AUR = []string{"yay-pkg"}
	modules["a"].Units.User = []string{"mako.service"}
	modules["b"].Units.User = []string{"mako.service", "kanshi.service"}
	modules["b"].Units.Now = true
	modules["c"].Units.Now = true

	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	l := layer.NewHome(home, filepath.Join(root, "state"), dirs)
	st := &state.State{}
	for _, name := range []string{"c", "a"} {
		if err := l.Activate(context.Background(), name, st.Modules); err != nil {
			t.Fatal(err)
		}
		st.Modules = append(st.Modules, name)
	}
	st.Own(state.UserUnits, "c", []string{"swayidle.service"})
	st.Own(state.AUR, "c", []string{"old-aur"})

	in := Input{Desired: []string{"a", "b"}, Modules: modules}
	h := Half{
		State:     st,
		Layer:     l,
		Installed: set("old-aur"),
		Units:     set("swayidle.service"),
	}
	return in, h, dirs["a"]
}

func TestUserPlan(t *testing.T) {
	in, h, _ := fixture(t)
	steps, err := User(in, h)
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Stage: UserTeardown, Op: Disable, Module: "c", Names: []string{"swayidle.service"}, Now: true},
		{Stage: UserTeardown, Op: LayerOff, Module: "c"},
		{Stage: AURRemove, Op: Remove, Names: []string{"old-aur"}},
		{Stage: AURInstall, Op: Install, Module: "a", Names: []string{"yay-pkg"}},
		{Stage: UserSetup, Op: LayerOn, Module: "b"},
		{Stage: UserSetup, Op: Enable, Module: "a", Names: []string{"mako.service"}},
		{Stage: UserSetup, Op: Enable, Module: "b", Names: []string{"kanshi.service"}, Now: true},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("got  %+v\nwant %+v", steps, want)
	}
	if got := h.State.Owned[state.AUR]["c"]; len(got) != 1 {
		t.Error("planning must not change the state")
	}
}

// Files added to a module that is already on are picked up by a restack.
func TestRestackWhenAModuleChanged(t *testing.T) {
	in, h, dirA := fixture(t)
	if err := os.WriteFile(filepath.Join(dirA, "home", ".newrc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	steps, err := User(in, h)
	if err != nil {
		t.Fatal(err)
	}
	var restack []Step
	for _, s := range steps {
		if s.Op == Restack {
			restack = append(restack, s)
		}
	}
	if len(restack) != 1 || restack[0].Module != "a" || len(restack[0].Changes) != 1 || restack[0].Changes[0][0] != '+' {
		t.Fatalf("restack steps = %+v", restack)
	}
}

// Packages taken over, pacman and AUR, become explicitly installed in one
// root step, unless they are already.
func TestMarkAdopted(t *testing.T) {
	modules := module.Set{"a": {Name: "a"}}
	modules["a"].Packages.Pacman = []string{"dep", "explicit", "missing"}
	modules["a"].Packages.AUR = []string{"aur-dep", "aur-explicit", "aur-owned"}
	user := &state.State{}
	user.Own(state.AUR, "a", []string{"aur-owned"})
	in := Input{Desired: []string{"a"}, Modules: modules}
	h := Half{
		State:     &state.State{},
		Layer:     layer.NewSystem(t.TempDir(), t.TempDir(), nil),
		Installed: set("dep", "explicit", "aur-dep", "aur-explicit", "aur-owned"),
		User:      user,
		Explicit:  set("explicit", "aur-explicit"),
	}
	marks := func(h Half) []string {
		steps, err := System(in, h)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.Op == MarkAdopted {
				if s.Stage != Packages {
					t.Errorf("stage = %v", s.Stage)
				}
				return s.Names
			}
		}
		return nil
	}

	if got := Adopted(in, h); !slices.Equal(got, []string{"aur-dep", "aur-explicit", "dep", "explicit"}) {
		t.Errorf("adopted = %v", got)
	}
	if got := marks(h); !slices.Equal(got, []string{"aur-dep", "dep"}) {
		t.Errorf("marked = %v", got)
	}
	h.User = nil // no AUR package is installed: the user half isn't read
	if got := marks(h); !slices.Equal(got, []string{"dep"}) {
		t.Errorf("marked without the user half = %v", got)
	}
	h.Explicit = set("dep", "explicit")
	if got := marks(h); got != nil {
		t.Errorf("marked though all are explicit: %v", got)
	}
}

func TestSystemPackagesAndGroups(t *testing.T) {
	modules := module.Set{
		"a": {Name: "a"},
		"b": {Name: "b"},
	}
	modules["a"].Packages.Pacman = []string{"foot", "wmenu", "go"}
	modules["b"].Packages.Pacman = []string{"foot", "river"}
	modules["b"].Groups = []string{"video"}
	st := &state.State{}
	st.Own(state.Pacman, "gone", []string{"oldpkg", "go"})
	st.Own(state.Groups, "gone", []string{"input"})

	in := Input{Desired: []string{"a", "b"}, Modules: modules, Protected: []string{"base"}}
	h := Half{
		State:     st,
		Layer:     layer.NewSystem(t.TempDir(), t.TempDir(), nil),
		Installed: set("wmenu", "go", "oldpkg"),
		Groups:    set("wheel", "input"),
	}
	steps, err := System(in, h)
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Stage: SystemTeardown, Op: Leave, Names: []string{"input"}},
		{Stage: Packages, Op: Remove, Names: []string{"oldpkg"}}, // go moves to a, which declares it
		{Stage: Packages, Op: Adopt, Of: state.Pacman, Module: "a", Names: []string{"wmenu"}},
		{Stage: Packages, Op: Install, Module: "a", Names: []string{"foot"}},
		{Stage: Packages, Op: Install, Module: "b", Names: []string{"river"}},
		{Stage: Packages, Op: MarkAdopted, Names: []string{"wmenu"}},
		// No layer steps: neither module has a system/ tree.
		{Stage: SystemSetup, Op: Join, Module: "b", Names: []string{"video"}},
	}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("got  %+v\nwant %+v", steps, want)
	}
}

func TestMergeByStageAndHash(t *testing.T) {
	system := []Step{{Stage: SystemTeardown, Op: LayerOff, Module: "x"}, {Stage: SystemSetup, Op: LayerOn, Module: "y"}}
	user := []Step{{Stage: UserTeardown, Op: LayerOff, Module: "x"}, {Stage: UserSetup, Op: LayerOn, Module: "y"}}
	steps := Merge(system, user)

	var stages []Stage
	for _, g := range ByStage(steps) {
		stages = append(stages, g[0].Stage)
	}
	if want := []Stage{UserTeardown, SystemTeardown, SystemSetup, UserSetup}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for stage, root := range map[Stage]bool{UserTeardown: false, SystemTeardown: true, AURRemove: false, Packages: true, AURInstall: false, SystemSetup: true, UserSetup: false} {
		if stage.Root() != root {
			t.Errorf("stage %d: Root() = %v", stage, !root)
		}
	}

	a, b := Filter(steps, SystemSetup), Filter(steps, SystemSetup)
	if Hash(a) != Hash(b) {
		t.Error("hash must be deterministic")
	}
	b[0].Changes = append(b[0].Changes, "! /etc/x.conf (kept as x.conf.orig)")
	if Hash(a) != Hash(b) {
		t.Error("a file that an earlier stage created must not change the hash")
	}
	b[0].Changes = append(b[0].Changes, "~ /etc/x.conf")
	if Hash(a) == Hash(b) {
		t.Error("hash must change with what is deployed")
	}
	b[0].Module = "z"
	if Hash(a) == Hash(b) {
		t.Error("hash must change with the steps")
	}

	x := []Step{{Stage: LocalPackages, Op: RemoveConflicting, Names: []string{"old"}, Changes: []string{"x old: conflicts with new"}}}
	y := slices.Clone(x)
	y[0].Changes = []string{"x old: conflicts with new; needed by a"}
	if Hash(x) != Hash(y) {
		t.Error("what needs a conflicting package must not change the hash")
	}
	y[0].Changes = []string{"x old: conflicts with newer"}
	if Hash(x) == Hash(y) {
		t.Error("hash must change with what a conflicting package conflicts with")
	}
	x[0].Stage, y[0].Stage = Packages, Packages
	y[0].Changes = []string{"x old: conflicts with new; needed by a"}
	if Hash(x) == Hash(y) {
		t.Error("what needs a repository package being removed must change the hash")
	}
}

func localSteps(names ...string) []Step {
	var steps []Step
	for _, n := range names {
		steps = append(steps, Step{Stage: LocalPackages, Op: InstallLocal, Module: "m", Names: []string{n}})
	}
	return steps
}

// localBatches plans every batch with nextBatch, installing each one.
func localBatches(pending []Step, incoming, installed map[string]system.Package) ([][]string, error) {
	installed = maps.Clone(installed)
	var out [][]string
	for len(pending) > 0 {
		batch, err := nextBatch(pending, incoming, installed)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, s := range batch {
			names = append(names, s.Names[0])
			installed[s.Names[0]] = incoming[s.Names[0]]
		}
		pending = slices.DeleteFunc(pending, func(s Step) bool { return slices.Contains(names, s.Names[0]) })
		out = append(out, names)
	}
	return out, nil
}

// Own builds wait for the pending own builds they need to build, by name
// or through what those provide, in as few batches as that allows.
func TestLocalBatches(t *testing.T) {
	for _, c := range []struct {
		name     string
		order    []string
		incoming map[string]system.Package
		want     [][]string
		err      string
	}{
		{"unrelated", []string{"a", "b"}, map[string]system.Package{"a": {BuildDepends: []string{"glibc"}}}, [][]string{{"a", "b"}}, ""},
		{"chain", []string{"c", "b", "a"}, map[string]system.Package{
			"c": {BuildDepends: []string{"b>=1"}}, "b": {BuildDepends: []string{"a"}},
		}, [][]string{{"a"}, {"b"}, {"c"}}, ""},
		{"diamond", []string{"top", "left", "right", "root", "other"}, map[string]system.Package{
			"top": {BuildDepends: []string{"left", "right"}}, "left": {BuildDepends: []string{"root"}}, "right": {BuildDepends: []string{"root"}},
		}, [][]string{{"root", "other"}, {"left", "right"}, {"top"}}, ""},
		{"provides", []string{"consumer", "libfoo-git"}, map[string]system.Package{
			"consumer": {BuildDepends: []string{"libfoo>=1"}}, "libfoo-git": {Provides: []string{"libfoo=1.2"}},
		}, [][]string{{"libfoo-git"}, {"consumer"}}, ""},
		{"own provide", []string{"a"}, map[string]system.Package{"a": {Provides: []string{"a-virtual"}, BuildDepends: []string{"a-virtual"}}}, [][]string{{"a"}}, ""},
		{"already built", []string{"a", "b"}, map[string]system.Package{"a": {}, "b": {BuildDepends: nil}}, [][]string{{"a", "b"}}, ""},
		{"provided already", []string{"consumer", "libfoo-git"}, map[string]system.Package{
			"consumer": {BuildDepends: []string{"libbar"}}, "libfoo-git": {Provides: []string{"libbar"}},
		}, [][]string{{"consumer", "libfoo-git"}}, ""},
		{"installed older", []string{"consumer", "base"}, map[string]system.Package{"consumer": {BuildDepends: []string{"base"}}}, [][]string{{"base"}, {"consumer"}}, ""},
		{"cycle", []string{"x", "a", "b", "c"}, map[string]system.Package{
			"x": {BuildDepends: []string{"a"}}, "a": {BuildDepends: []string{"b"}, Provides: []string{"virt"}},
			"b": {BuildDepends: []string{"c"}}, "c": {BuildDepends: []string{"virt"}},
		}, nil, "own builds need each other in a cycle: a → b → c → a"},
		{"any provider", []string{"consumer", "provider1", "provider2"}, map[string]system.Package{
			"consumer":  {BuildDepends: []string{"virt"}},
			"provider1": {Provides: []string{"virt"}, BuildDepends: []string{"consumer"}},
			"provider2": {Provides: []string{"virt"}},
		}, [][]string{{"provider2"}, {"consumer"}, {"provider1"}}, ""},
		{"installed too old", []string{"consumer", "upgrade"}, map[string]system.Package{
			"consumer": {BuildDepends: []string{"libbar>=3"}}, "upgrade": {Provides: []string{"libbar=3"}},
		}, [][]string{{"upgrade"}, {"consumer"}}, ""},
		{"install with what it needs", []string{"app", "lib", "tool"}, map[string]system.Package{
			"app": {Depends: []string{"lib"}}, "lib": {BuildDepends: []string{"tool"}},
		}, [][]string{{"tool"}, {"app", "lib"}}, ""},
		{"install needs each other", []string{"a", "b"}, map[string]system.Package{
			"a": {Depends: []string{"b"}}, "b": {Depends: []string{"a"}},
		}, [][]string{{"a", "b"}}, ""},
		{"install waits for a build", []string{"app", "lib", "tool"}, map[string]system.Package{
			"app": {Depends: []string{"lib"}}, "lib": {BuildDepends: []string{"tool"}}, "tool": {BuildDepends: []string{"app"}},
		}, nil, "own builds need each other in a cycle: app → lib → tool → app"},
	} {
		t.Run(c.name, func(t *testing.T) {
			installed := map[string]system.Package{"base": {}, "libbar-bin": {Provides: []string{"libbar=2"}}}
			got, err := localBatches(localSteps(c.order...), c.incoming, installed)
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("err = %v, want %s", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("batches = %v, want %v", got, c.want)
			}
		})
	}
}

// Each batch is built, then installed after the removal of what it
// conflicts with, as the earlier stages and batches leave the system: a
// package that the AUR removal or package stage removes or installs, or
// that an earlier batch installs, counts as such.
func TestLocal(t *testing.T) {
	m := &module.Module{Name: "m"}
	m.Packages.Local = []string{"a", "replacement"}
	in := Input{Desired: []string{"m"}, Modules: module.Set{"m": m}}
	h := Half{
		Packages: map[string]system.Package{"old": {}, "gone": {Conflicts: []string{"a"}}, "old-aur": {Provides: []string{"virt"}}},
		Incoming: map[string]system.Package{
			"term":        {Conflicts: []string{"a"}},
			"a":           {BuildDepends: []string{"old", "virt"}},
			"virt-git":    {Provides: []string{"virt"}},
			"replacement": {BuildDepends: []string{"a"}, Provides: []string{"old"}, Conflicts: []string{"old", "term"}},
		},
	}
	user := Step{Stage: AURInstall, Op: Install, Module: "m", Names: []string{"aur"}}
	aur := Step{Stage: AURRemove, Op: Remove, Names: []string{"old-aur"}}
	remove := Step{Stage: Packages, Op: Remove, Names: []string{"gone"}}
	install := Step{Stage: Packages, Op: Install, Module: "m", Names: []string{"term"}}
	stale := Step{Stage: LocalPackages, Op: RemoveConflicting, Names: []string{"stale"}}
	m.Packages.Local = append(m.Packages.Local, "virt-git")
	local := localSteps("replacement", "a", "virt-git")
	steps := slices.Concat([]Step{aur, remove, install, stale}, local, []Step{user})
	got, err := Local(in, h, steps)
	if err != nil {
		t.Fatal(err)
	}
	build := func(s Step) Step { s.Stage, s.Op = Build, BuildLocal; return s }
	want := []Step{
		aur, remove, install,
		build(local[2]),
		local[2],
		build(local[1]),
		{Stage: LocalPackages, Op: RemoveConflicting, Names: []string{"term"}, Changes: []string{"x term: conflicts with a"}},
		local[1],
		build(local[0]),
		{Stage: LocalPackages, Op: RemoveConflicting, Names: []string{"old"}, Changes: []string{"x old: conflicts with replacement"}},
		local[0],
		user,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if _, ok := h.Packages["a"]; ok {
		t.Error("Local changed the caller's packages")
	}

	if got, _ := Local(in, h, []Step{user}); !reflect.DeepEqual(got, []Step{user}) {
		t.Fatalf("no own builds: %+v", got)
	}
}

func TestOwnBuilds(t *testing.T) {
	dir := t.TempDir()
	recipe := filepath.Join(dir, "pkg", "waylock-arcus")
	if err := os.MkdirAll(recipe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recipe, "PKGBUILD"), []byte("pkgname=waylock-arcus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &module.Module{Name: "wl-base", Dir: dir}
	m.Packages.Local = []string{"waylock-arcus"}
	in := Input{Desired: []string{"wl-base"}, Modules: module.Set{"wl-base": m}}
	h := Half{State: &state.State{}, Layer: layer.NewSystem(t.TempDir(), t.TempDir(), nil)}
	hash, err := module.RecipeHash(recipe)
	if err != nil {
		t.Fatal(err)
	}

	steps, err := System(in, h)
	if err != nil {
		t.Fatal(err)
	}
	install := Step{Stage: LocalPackages, Op: InstallLocal, Module: "wl-base", Names: []string{"waylock-arcus"}, Recipe: hash}
	if !reflect.DeepEqual(steps, []Step{install}) {
		t.Fatalf("missing build: %+v", steps)
	}
	merged, err := Local(in, Half{Incoming: map[string]system.Package{"waylock-arcus": {}}}, Merge(steps, nil))
	if err != nil {
		t.Fatal(err)
	}
	build := Step{Stage: Build, Op: BuildLocal, Module: "wl-base", Names: []string{"waylock-arcus"}, Recipe: hash}
	if !reflect.DeepEqual(merged, []Step{build, install}) {
		t.Fatalf("merged: %+v", merged)
	}

	// Installed from this recipe: nothing to do, unless a rebuild is asked for.
	h.Installed = set("waylock-arcus")
	h.State.Built = map[string]string{"waylock-arcus": hash}
	if steps, _ := System(in, h); len(steps) != 0 {
		t.Fatalf("current build: %+v", steps)
	}
	in.Rebuild = []string{"waylock-arcus"}
	if steps, _ := System(in, h); !reflect.DeepEqual(steps, []Step{install}) {
		t.Fatalf("rebuild: %+v", steps)
	}
	in.Rebuild = nil

	// A changed recipe is rebuilt.
	if err := os.WriteFile(filepath.Join(recipe, "bordercolors.patch"), []byte("+color"), 0o644); err != nil {
		t.Fatal(err)
	}
	if steps, _ := System(in, h); len(steps) != 1 || steps[0].Recipe == hash {
		t.Fatalf("changed recipe: %+v", steps)
	}

	// Turned off: an owned build is removed.
	h.State.Own(state.Local, "wl-base", []string{"waylock-arcus"})
	in.Desired = nil
	if steps, _ := System(in, h); !reflect.DeepEqual(steps, []Step{{Stage: Packages, Op: RemoveLocal, Names: []string{"waylock-arcus"}}}) {
		t.Fatalf("off: %+v", steps)
	}
}

func TestNeeded(t *testing.T) {
	tests := []struct {
		remove     []string
		requiredBy map[string][]string
		want       []string
	}{
		{[]string{"a", "b"}, nil, []string{}},
		{[]string{"a", "b"}, map[string][]string{"a": {"steam"}}, []string{"a"}},
		// b needs a, and both go: nothing outside needs either.
		{[]string{"a", "b"}, map[string][]string{"a": {"b"}}, []string{}},
		// steam needs b, which needs a: both stay.
		{[]string{"a", "b"}, map[string][]string{"a": {"b"}, "b": {"steam"}}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		if got := Needed(tt.remove, tt.requiredBy); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Needed(%v, %v) = %v, want %v", tt.remove, tt.requiredBy, got, tt.want)
		}
	}
}

// An installed package that a package to install conflicts with, either
// way round and through what it provides, is removed right before that
// install; a versioned conflict is left to pacman.
func TestConflicts(t *testing.T) {
	m := &module.Module{Name: "stream"}
	m.Packages.Pacman = []string{"river", "foot"}
	m.Packages.Local = []string{"waylock-arcus"}
	in := Input{Desired: []string{"stream"}, Modules: module.Set{"stream": m}}
	h := Half{
		Packages: map[string]system.Package{
			"river-classic": {Conflicts: []string{"river"}},
			"waylock":       {RequiredBy: []string{"pg-lock"}},
			"alacritty":     {},
			"old-term":      {Provides: []string{"terminal=1"}},
			"pg-lock":       {},
		},
		Incoming: map[string]system.Package{
			"river":         {},
			"foot":          {Conflicts: []string{"terminal<2"}},
			"waylock-arcus": {Provides: []string{"waylock=1.6.0"}, Conflicts: []string{"waylock"}},
		},
	}
	install := Step{Stage: Packages, Op: Install, Module: "stream", Names: []string{"river", "foot"}}
	local := Step{Stage: LocalPackages, Op: InstallLocal, Module: "stream", Names: []string{"waylock-arcus"}}
	got, err := Conflicts(in, h, []Step{install, local})
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Stage: Packages, Op: RemoveConflicting, Names: []string{"river-classic"}, Changes: []string{"x river-classic: conflicts with river"}},
		install,
		{Stage: LocalPackages, Op: RemoveConflicting, Names: []string{"waylock"}, Changes: []string{"x waylock: conflicts with waylock-arcus; needed by pg-lock"}},
		local,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	// A package that a desired module lists is never removed for another.
	h.Packages["alacritty"] = system.Package{Conflicts: []string{"foot"}}
	m.Packages.Pacman = append(m.Packages.Pacman, "alacritty")
	if _, err := Conflicts(in, h, []Step{install}); err == nil || err.Error() != "foot conflicts with alacritty, which module stream lists" {
		t.Fatalf("got %v", err)
	}
}

// A provider that an earlier batch removes for a conflict no longer
// satisfies what a later build needs.
func TestLocalRemovedProvider(t *testing.T) {
	m := &module.Module{Name: "m"}
	m.Packages.Local = []string{"consumer", "provider", "remover"}
	in := Input{Desired: []string{"m"}, Modules: module.Set{"m": m}}
	h := Half{
		Packages: map[string]system.Package{"old": {Provides: []string{"virt"}}},
		Incoming: map[string]system.Package{
			"consumer": {BuildDepends: []string{"remover", "virt"}},
			"provider": {BuildDepends: []string{"remover"}, Provides: []string{"virt"}},
			"remover":  {Conflicts: []string{"old"}},
		},
	}
	got, err := Local(in, h, localSteps("consumer", "provider", "remover"))
	if err != nil {
		t.Fatal(err)
	}
	var installs []string
	for _, s := range got {
		if s.Op == InstallLocal {
			installs = append(installs, s.Names[0])
		}
	}
	if want := []string{"remover", "provider", "consumer"}; !reflect.DeepEqual(installs, want) {
		t.Fatalf("installs = %v, want %v", installs, want)
	}
}

// pkg is a test package: "p:virt" provides virt, "c:x" conflicts with x,
// "d:virt" depends on it, "b:base" needs it to build.
func pkg(fields ...string) system.Package {
	var p system.Package
	for _, f := range fields {
		k, v, _ := strings.Cut(f, ":")
		switch k {
		case "p":
			p.Provides = append(p.Provides, v)
		case "c":
			p.Conflicts = append(p.Conflicts, v)
		case "d":
			p.Depends = append(p.Depends, v)
		case "b":
			p.BuildDepends = append(p.BuildDepends, v)
		}
	}
	return p
}

// batchesOf plans own builds with Local, conflict removals included, and
// returns the installs of each batch, sorted within a batch.
func batchesOf(installed, incoming map[string]system.Package, order []string) ([][]string, error) {
	m := &module.Module{Name: "m"}
	m.Packages.Local = order
	in := Input{Desired: []string{"m"}, Modules: module.Set{"m": m}}
	steps, err := Local(in, Half{Packages: installed, Incoming: incoming}, localSteps(order...))
	if err != nil {
		return nil, err
	}
	var out [][]string
	building := true
	for _, s := range steps {
		switch s.Op {
		case BuildLocal:
			if !building {
				building = true
			}
		case InstallLocal:
			if building {
				out = append(out, nil)
				building = false
			}
			out[len(out)-1] = append(out[len(out)-1], s.Names[0])
		}
	}
	for _, b := range out {
		slices.Sort(b)
	}
	return out, nil
}

// permutations returns every order of names.
func permutations(names []string) [][]string {
	if len(names) <= 1 {
		return [][]string{slices.Clone(names)}
	}
	var out [][]string
	for i := range names {
		rest := slices.Concat(names[:i], names[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]string{names[i]}, p...))
		}
	}
	return out
}

// A batch never removes an installed package one of its installs needs
// unless something in the batch provides it again; the consumer waits
// instead. Each case gives the same batches in every pending order, unless
// it is a fixed-order case.
func TestLocalKeepsWhatABatchNeeds(t *testing.T) {
	type world map[string]system.Package
	for _, c := range []struct {
		name      string
		installed world
		incoming  world
		want      string // batches, "|"-separated; "cycle" for a refusal
		fixed     []string
	}{
		{"remover is not the provider",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt"), "r": pkg("c:x"), "p": pkg("p:virt", "b:b"), "b": {}},
			"b r|c p", nil},
		{"the installed package declares the conflict",
			world{"x": pkg("p:virt", "c:r")},
			world{"c": pkg("d:virt"), "r": {}, "p": pkg("p:virt", "b:b"), "b": {}},
			"b r|c p", nil},
		{"the replacement builds with what needs the replaced",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt"), "p": pkg("p:virt", "c:x", "b:c")},
			"c|p", nil},
		{"order case: the remover waits with its dependency",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt"), "r": pkg("c:x", "d:b"), "b": pkg("b:c"), "p": pkg("p:virt", "b:c")},
			"c|b p r", nil},
		{"order case: an upgrade that stops providing",
			world{"x": pkg("p:w")},
			world{"x": pkg("d:a"), "a": pkg("p:w", "d:v"), "b": pkg("p:v", "d:c", "b:c"), "c": pkg("d:w")},
			"c|a b x", nil},
		{"own provides is no need",
			world{},
			world{"e": pkg("p:virt", "d:virt"), "b": pkg("p:virt", "b:e")},
			"e|b", nil},
		{"own provides is still a build need",
			world{},
			world{"e": pkg("p:virt", "b:virt"), "p": pkg("p:virt")},
			"p|e", nil},
		{"a build has what its own install will remove",
			world{"x": pkg("p:virt")},
			world{"c": pkg("b:virt", "c:x"), "p": pkg("p:virt", "b:c")},
			"c|p", nil},
		{"versioned own provides waits",
			world{},
			world{"e": pkg("p:virt", "d:virt>=2"), "b": pkg("p:virt=2", "b:z"), "z": {}},
			"z|b e", nil},
		{"a dropped consumer comes back when its remover has to wait",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt"), "r": pkg("c:x", "d:c"), "p": pkg("p:virt", "b:b"), "b": {}},
			"b c|p r", nil},
		{"drop order",
			world{"x": pkg("p:virt")},
			world{"a": pkg("d:b", "d:virt"), "b": pkg("d:a"), "r": pkg("c:x", "d:p"), "p": pkg("p:virt", "b:b")},
			"a b|p r", nil},
		{"a pending build's name comes first",
			world{"p": {}},
			world{"c": pkg("d:p"), "p": pkg("b:b"), "b": {}},
			"b|c p", nil},
		{"the consumer removes its own satisfier",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt", "c:x"), "p": pkg("p:virt", "b:b"), "b": {}},
			"b|c p", nil},
		{"an upgrade that stops providing what it needs",
			world{"x": pkg("p:virt")},
			world{"x": pkg("d:virt"), "p": pkg("p:virt", "b:b"), "b": {}},
			"b|p x", nil},
		{"an upgrade that stops providing what another needs",
			world{"x": pkg("p:virt")},
			world{"x": {}, "c": pkg("d:virt"), "p": pkg("p:virt", "b:b"), "b": {}},
			"b x|c p", nil},
		// Open cases, pinned as they behave now (see localNeeds).
		{"open: a later removal breaks an earlier install",
			world{"x": pkg("p:virt")},
			world{"c": pkg("d:virt"), "r": pkg("c:x", "b:c")},
			"c|r", nil},
		{"open: a versioned build need waits for a pending provider",
			world{"x": pkg("p:virt=2")},
			world{"p": pkg("p:virt=1", "c:x"), "q": pkg("b:virt>=2")},
			"p|q", nil},
		{"open: two builds that need each other aren't re-added",
			world{"x": pkg("p:virt")},
			world{"a": pkg("d:b", "d:virt"), "b": pkg("d:a"), "r": pkg("c:x", "d:a"), "p": pkg("p:virt", "b:a")},
			"cycle", nil},
		{"open: of two blocking consumers the first waits",
			world{"x": pkg("p:v", "p:w"), "y": pkg("p:v", "c:b")},
			world{"x": pkg("d:v"), "b": pkg("d:v"), "c": pkg("p:v", "b:b"), "d": pkg("d:c", "b:b"), "a": pkg("d:b", "b:d")},
			"cycle", []string{"b", "x", "c", "d", "a"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			orders := [][]string{c.fixed}
			if c.fixed == nil {
				orders = permutations(slices.Sorted(maps.Keys(c.incoming)))
			}
			for _, order := range orders {
				got, err := batchesOf(c.installed, c.incoming, order)
				var s string
				if err != nil {
					if !strings.Contains(err.Error(), "cycle") {
						t.Fatalf("%v: %v", order, err)
					}
					s = "cycle"
				} else {
					var parts []string
					for _, b := range got {
						parts = append(parts, strings.Join(b, " "))
					}
					s = strings.Join(parts, "|")
				}
				if s != c.want {
					t.Fatalf("order %v: got %q, want %q", order, s, c.want)
				}
			}
		})
	}
}

// The root stage refuses a batch whose built packages need what the batch
// removes, and one with an install it has no package for.
func TestCheckBatch(t *testing.T) {
	type world map[string]system.Package
	install := func(names ...string) []Step { return localSteps(names...) }
	removes := func(names ...string) Step { return Step{Stage: LocalPackages, Op: RemoveConflicting, Names: names} }
	for _, c := range []struct {
		name      string
		installed world
		incoming  world
		steps     []Step
		rebuild   []string
		want      string // part of the error, or "" for none
	}{
		{"no package for an install", world{}, world{"a": {}},
			install("a", "b"), nil, "own build b has no package"},
		{"a dependency .SRCINFO missed loses its provider",
			world{"x": pkg("p:virt")}, world{"c": pkg("d:virt"), "r": pkg("c:x")},
			append([]Step{removes("x")}, install("c", "r")...), nil, "c needs virt, which this batch would remove; this batch was not installed, run the command again"},
		{"under rebuild the recipe has to list it",
			world{"x": pkg("p:virt")}, world{"c": pkg("d:virt"), "r": pkg("c:x")},
			append([]Step{removes("x")}, install("c", "r")...), []string{"c"}, "add it to depends in the PKGBUILD"},
		{"a soname dependency counts by name",
			world{"lib": pkg("p:libfoo.so=1-64")}, world{"c": pkg("d:libfoo.so=1-64"), "r": pkg("c:lib")},
			append([]Step{removes("lib")}, install("c", "r")...), nil, "c needs libfoo.so=1-64, which this batch would remove and nothing in it provides; this batch was not installed: run the command again if an own build provides libfoo.so, otherwise install a provider of it first"},
		{"a same-name upgrade stops providing what another install needs",
			world{"x": pkg("p:virt"), "y": pkg("p:virt")}, world{"x": {}, "c": pkg("d:virt"), "r": pkg("c:y")},
			append([]Step{removes("y")}, install("x", "c", "r")...), nil, "c needs virt"},
		{"a same-name upgrade stops providing what it needs itself",
			world{"x": pkg("p:v"), "z": pkg("p:v")}, world{"x": pkg("d:v"), "b": pkg("c:z")},
			append([]Step{removes("z")}, install("b", "x")...), nil, "x needs v"},
		{"a provider in the batch", world{"x": pkg("p:virt")}, world{"c": pkg("d:virt"), "p": pkg("p:virt", "c:x")},
			append([]Step{removes("x")}, install("c", "p")...), nil, ""},
		{"the consumer removes its own only provider: pacman decides",
			world{"x": pkg("p:virt")}, world{"c": pkg("d:virt", "c:x")},
			append([]Step{removes("x")}, install("c")...), nil, ""},
		{"not met before the batch: pacman decides", world{}, world{"c": pkg("d:virt")},
			install("c"), nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckBatch(Half{Packages: c.installed, Incoming: c.incoming}, c.steps, c.rebuild)
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("CheckBatch = %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("CheckBatch = %v, want %q", err, c.want)
			}
		})
	}
}

// appImageFixture is a home with module desk declaring AppImages and
// module old, which is being turned off, owning some. record writes a file
// and records it as arcus installed it; place writes one arcus doesn't know.
type appImageFixture struct {
	t     *testing.T
	home  string
	in    Input
	st    *state.State
	notes []string
}

func newAppImageFixture(t *testing.T) *appImageFixture {
	root := t.TempDir()
	f := &appImageFixture{t: t, home: filepath.Join(root, "home"), st: &state.State{}}
	f.in = Input{Desired: []string{"desk"}, Modules: module.Set{
		"desk": {Name: "desk", Dir: filepath.Join(root, "desk")},
		"old":  {Name: "old", Dir: filepath.Join(root, "old")},
	}}
	f.in.Modules["desk"].AppImages = map[string]module.AppImage{}
	if err := os.MkdirAll(f.home, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func sum(content string) string {
	s := sha256.Sum256([]byte(content))
	return hex.EncodeToString(s[:])
}

func (f *appImageFixture) declare(p, content string) {
	f.in.Modules["desk"].AppImages[p] = module.AppImage{URL: "https://x.org/" + p, SHA256: sum(content), Path: p}
}

func (f *appImageFixture) place(p, content string) {
	f.t.Helper()
	path := filepath.Join(f.home, p)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *appImageFixture) record(owner, p, content string) {
	f.t.Helper()
	f.place(p, content)
	f.st.SetAppImage(owner, p, sum(content))
}

func (f *appImageFixture) user() ([]Step, error) {
	f.notes = nil
	h := Half{State: f.st, Layer: layer.NewHome(f.home, filepath.Join(f.home, ".state"), nil), Note: func(n string) { f.notes = append(f.notes, n) }}
	return User(f.in, h)
}

func (f *appImageFixture) plan() []Step {
	f.t.Helper()
	steps, err := f.user()
	if err != nil {
		f.t.Fatal(err)
	}
	return steps
}

func (f *appImageFixture) refused(want string) {
	f.t.Helper()
	if _, err := f.user(); err == nil || !strings.Contains(err.Error(), want) {
		f.t.Fatalf("err = %v, want %q", err, want)
	}
}

// Each case of the AppImage decision table becomes its step, and a changed
// file is left alone with a note rather than a step.
func TestAppImagePlan(t *testing.T) {
	f := newAppImageFixture(t)
	f.declare("missing", "v1")
	f.declare("pinned", "v1")
	f.place("pinned", "v1")
	f.declare("bumped", "v2")
	f.record("desk", "bumped", "v1")
	f.declare("fine", "v1")
	f.record("desk", "fine", "v1")
	f.declare("updated", "v2")
	f.record("desk", "updated", "v1")
	f.place("updated", "v3 from the app")

	f.record("old", "a-unchanged", "v1")
	f.record("old", "b-modified", "v1")
	f.place("b-modified", "edited")
	f.record("old", "c-vanished", "v1")
	if err := os.Remove(filepath.Join(f.home, "c-vanished")); err != nil {
		t.Fatal(err)
	}

	want := []Step{
		{Stage: UserTeardown, Op: RemoveAppImage, Names: []string{"a-unchanged"}, Kept: []string{"b-modified", "c-vanished"}, Changes: []string{
			"= ~/b-modified: changed since arcus installed it, so it stays", "= ~/c-vanished: already gone",
		}},
		{Stage: UserSetup, Op: Adopt, Of: state.HomeAppImages, Module: "desk", Names: []string{"pinned"}},
		{Stage: UserSetup, Op: InstallAppImage, Module: "desk", Names: []string{"bumped"}, Changes: []string{"from https://x.org/bumped"}},
		{Stage: UserSetup, Op: InstallAppImage, Module: "desk", Names: []string{"missing"}, Changes: []string{"from https://x.org/missing"}},
	}
	steps := f.plan()
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("got  %+v\nwant %+v", steps, want)
	}
	if !slices.Equal(f.notes, []string{"~/updated: changed since arcus installed it, so it stays; remove it to get the pinned version"}) {
		t.Errorf("notes = %q", f.notes)
	}
	if len(f.st.Owned[state.HomeAppImages]["old"]) != 3 {
		t.Error("planning must not change the state")
	}
}

// A changed AppImage whose declaration moves to another module is handed
// over with its record, not dropped and then refused.
func TestAppImageHandOverOfAChangedFile(t *testing.T) {
	f := newAppImageFixture(t)
	f.declare("app", "v2")
	f.record("old", "app", "v1")
	f.place("app", "v3 from the app")
	want := []Step{{Stage: UserTeardown, Op: HandOver, Of: state.HomeAppImages, Changes: []string{"app: old → desk"}}}
	if got := f.plan(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if len(f.notes) != 1 {
		t.Errorf("notes = %q", f.notes)
	}
}

// Removing a file below a symlinked directory would follow the link, into
// a dotfiles checkout say: it stays.
func TestAppImageBelowASymlinkedParentStays(t *testing.T) {
	f := newAppImageFixture(t)
	f.record("old", "linked/app", "v1")
	checkout := filepath.Join(filepath.Dir(f.home), "checkout")
	if err := os.Rename(filepath.Join(f.home, "linked"), checkout); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(checkout, filepath.Join(f.home, "linked")); err != nil {
		t.Fatal(err)
	}
	want := []Step{{Stage: UserTeardown, Op: RemoveAppImage, Kept: []string{"linked/app"}, Changes: []string{
		"= ~/linked/app: parent " + filepath.Join(f.home, "linked") + " is a symlink, so it stays",
	}}}
	if got := f.plan(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestAppImagePlanRefuses(t *testing.T) {
	t.Run("another file", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("app", "v1")
		f.place("app", "mine")
		f.refused("isn't the pinned AppImage: move it away")
	})
	t.Run("a directory", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("app", "v1")
		if err := os.MkdirAll(filepath.Join(f.home, "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.refused("isn't a regular file")
	})
	t.Run("a leftover download", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("app", "v1")
		f.place("app.arcus-tmp", "half")
		f.refused("left by an interrupted download")
	})
	t.Run("arcus's own data", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare(".state/state.json", "v1")
		f.refused("inside arcus's own data")
	})
	t.Run("arcus's own data through a link", func(t *testing.T) {
		f := newAppImageFixture(t)
		state := filepath.Join(f.home, ".state")
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		// $HOME reaches the state directory under another name too.
		if err := os.Symlink(state, filepath.Join(filepath.Dir(f.home), "elsewhere")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(filepath.Dir(f.home), "elsewhere"), filepath.Join(f.home, "s")); err != nil {
			t.Fatal(err)
		}
		f.declare("s/state.json", "v1")
		f.refused("inside arcus's own data")
	})
	t.Run("a symlinked parent", func(t *testing.T) {
		f := newAppImageFixture(t)
		checkout := filepath.Join(filepath.Dir(f.home), "checkout")
		if err := os.MkdirAll(checkout, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(checkout, filepath.Join(f.home, ".apps")); err != nil {
			t.Fatal(err)
		}
		f.declare(".apps/bin/app.AppImage", "v1")
		f.refused("parent " + filepath.Join(f.home, ".apps") + " is a symlink")
	})
	t.Run("two modules, one path", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("Applications/app", "v1")
		f.in.Desired = append(f.in.Desired, "old")
		f.in.Modules["old"].AppImages = map[string]module.AppImage{"x": {URL: "https://x.org/x", SHA256: sum("v1"), Path: "Applications/app/inner"}}
		f.refused("both put an AppImage at ~/Applications/app or inside it")
	})
	t.Run("a home link above it", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("Applications/app", "v1")
		f.in.Modules["desk"].Links = map[string]string{"Applications": "Sync/apps"}
		f.refused("module desk links ~/Applications")
	})
	t.Run("a home file of a module", func(t *testing.T) {
		f := newAppImageFixture(t)
		f.declare("Applications/app", "v1")
		home := filepath.Join(f.in.Modules["desk"].Dir, "home", "Applications", "app")
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		f.refused("module desk deploys a file at or above that path from its home/")
	})
}
