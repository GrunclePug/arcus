package plan

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
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
	install := Step{Stage: Packages, Op: InstallLocal, Module: "wl-base", Names: []string{"waylock-arcus"}, Recipe: hash}
	if !reflect.DeepEqual(steps, []Step{install}) {
		t.Fatalf("missing build: %+v", steps)
	}
	merged := Merge(steps, nil)
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
