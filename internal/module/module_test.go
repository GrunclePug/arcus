package module

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeModule creates dir/name/module.toml with the given content.
func writeModule(t *testing.T, dir, name, manifest string) string {
	t.Helper()
	pdir := filepath.Join(dir, name)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return pdir
}

func TestLoadFullManifest(t *testing.T) {
	dir := writeModule(t, t.TempDir(), "wl-base", `
# comments are the reason for TOML
description = "Shared Wayland session pieces"
category  = "desktop"
requires  = ["base"]
conflicts = ["x11-base"]
groups    = ["video"]

[packages]
pacman = ["foot", "wmenu", "wl-clipboard"]
aur    = ["wlr-randr-git"]
local  = []

[units]
system = []
user   = ["mako.service"]
now    = true
`)
	p, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "wl-base" || p.Dir != dir {
		t.Errorf("name/dir = %q/%q", p.Name, p.Dir)
	}
	if !slices.Equal(p.Packages.Pacman, []string{"foot", "wmenu", "wl-clipboard"}) ||
		!slices.Equal(p.Packages.AUR, []string{"wlr-randr-git"}) ||
		!slices.Equal(p.Units.User, []string{"mako.service"}) || !p.Units.Now ||
		!slices.Equal(p.Requires, []string{"base"}) || !slices.Equal(p.Conflicts, []string{"x11-base"}) ||
		!slices.Equal(p.Groups, []string{"video"}) || p.Category != "desktop" {
		t.Errorf("manifest decoded wrong: %+v", p.Manifest)
	}
}

func TestLoadEmptyManifest(t *testing.T) {
	if _, err := Load(writeModule(t, t.TempDir(), "empty", "")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		manifest string
		wantErr  string
	}{
		{"unknown top-level key", "p", `packges = []`, "unknown keys: packges"},
		{"unknown nested key", "p", "[packages]\npacmn = [\"foot\"]", "packages.pacmn"},
		// A key after [units] belongs to that table; groups must come first.
		{"groups after a table", "p", "[units]\nuser = []\n\ngroups = [\"video\"]", "units.groups"},
		{"wrong type", "p", `requires = "base"`, "requires"},
		{"bad toml", "p", `requires = [`, "loading module"},
		{"uppercase dir name", "River", ``, "invalid name"},
		{"reserved name", "orig", ``, "reserved"},
		{"self requirement", "p", `requires = ["p"]`, "require itself"},
		{"self conflict", "p", `conflicts = ["p"]`, "conflict with itself"},
		{"requires and conflicts", "p", "requires = [\"a\"]\nconflicts = [\"a\"]", "both"},
		{"duplicate package", "p", "[packages]\npacman = [\"foot\", \"foot\"]", "listed twice"},
		{"option-like package", "p", "[packages]\npacman = [\"--noconfirm\"]", "invalid name"},
		{"package with space", "p", "[packages]\naur = [\"a b\"]", "invalid name"},
		{"unit without suffix", "p", "[units]\nsystem = [\"bluetooth\"]", "invalid name"},
		{"bad group", "p", `groups = ["Video"]`, "invalid name"},
		{"dir outside home", "p", `dirs = ["../etc"]`, "dirs: \"../etc\""},
		{"absolute dir", "p", `dirs = ["/srv"]`, "dirs: \"/srv\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeModule(t, t.TempDir(), tt.dir, tt.manifest))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "-lead", "_lead", "orig", "arcus-foreign", "has space", "a.b", "River"} {
		if ValidateName(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	for _, good := range []string{"river-classic", "niri", "wl-base", "a_b", "x1", "9front"} {
		if err := ValidateName(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}

func TestScan(t *testing.T) {
	user, extra := t.TempDir(), t.TempDir()
	writeModule(t, user, "wl-base", `description = "mine"`)
	writeModule(t, extra, "games", ``)
	if err := os.MkdirAll(filepath.Join(user, "not-a-module"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Module directories are often symlinks into a dotfiles repository.
	linked := writeModule(t, t.TempDir(), "linked", ``)
	if err := os.Symlink(linked, filepath.Join(user, "linked")); err != nil {
		t.Fatal(err)
	}

	set, err := Scan(user, extra, filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wl-base", "games", "linked"} {
		if set[name] == nil {
			t.Errorf("%s not loaded", name)
		}
	}
	if len(set) != 3 || set["wl-base"].Description != "mine" {
		t.Errorf("got %+v", set)
	}

	writeModule(t, extra, "games", ``) // still fine: same dir
	writeModule(t, user, "games", ``)
	if _, err := Scan(user, extra); err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Fatalf("same name in two dirs: %v", err)
	}
}

// graph builds an in-memory Set: name -> requires, conflicts.
func graph(defs map[string][2][]string) Set {
	s := Set{}
	for name, d := range defs {
		p := &Module{Name: name}
		p.Requires, p.Conflicts = d[0], d[1]
		s[name] = p
	}
	return s
}

func req(names ...string) [2][]string { return [2][]string{names, nil} }

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		graph   map[string][2][]string
		targets []string
		want    []string
		wantErr string
	}{
		{"leaf", map[string][2][]string{"a": req()}, []string{"a"}, []string{"a"}, ""},
		{"chain", map[string][2][]string{"a": req(), "b": req("a"), "c": req("b")}, []string{"c"}, []string{"a", "b", "c"}, ""},
		{"diamond visits shared base once",
			map[string][2][]string{"base": req(), "l": req("base"), "r": req("base"), "top": req("l", "r")},
			[]string{"top"}, []string{"base", "l", "r", "top"}, ""},
		{"declaration order is preserved",
			map[string][2][]string{"x": req(), "y": req(), "t": req("y", "x")}, []string{"t"}, []string{"y", "x", "t"}, ""},
		{"host file order is kept where requirements allow",
			map[string][2][]string{"base": req(), "cpu-amd": req("base"), "wl-base": req("base"), "river-classic": req("wl-base")},
			[]string{"base", "cpu-amd", "river-classic", "wl-base"},
			[]string{"base", "cpu-amd", "wl-base", "river-classic"}, ""},
		{"two-node cycle", map[string][2][]string{"a": req("b"), "b": req("a")}, []string{"a"}, nil, "circular"},
		{"long cycle", map[string][2][]string{"a": req("b"), "b": req("c"), "c": req("a")}, []string{"a"}, nil, "circular"},
		{"missing requirement", map[string][2][]string{"a": req("ghost")}, []string{"a"}, nil, `requires "ghost"`},
		{"missing target", map[string][2][]string{}, []string{"nope"}, nil, "nope"},
		{"direct conflict",
			map[string][2][]string{"wm": {nil, {"river-classic"}}, "river-classic": req()},
			[]string{"river-classic", "wm"}, nil, "conflict"},
		{"conflict declared on one side only, pulled in by a requirement",
			map[string][2][]string{"wl-base": req(), "wm": {[]string{"wl-base"}, []string{"river-classic"}}, "river-classic": req("wl-base"), "rice": req("river-classic")},
			[]string{"wm", "rice"}, nil, "conflict"},
		{"conflict with a module that is not selected is fine",
			map[string][2][]string{"wm": {nil, {"river-classic"}}}, []string{"wm"}, []string{"wm"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := graph(tt.graph).Resolve(tt.targets)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDependents(t *testing.T) {
	s := graph(map[string][2][]string{
		"wl-base":       req(),
		"river-classic": req("wl-base"),
		"rice":          req("river-classic"), // transitive dependent of wl-base
		"niri":          req("wl-base"),
		"games":         req(),
	})
	tests := []struct {
		target string
		stack  []string
		want   []string
	}{
		{"wl-base", []string{"wl-base", "river-classic", "rice", "niri", "games"}, []string{"river-classic", "rice", "niri"}},
		{"river-classic", []string{"wl-base", "river-classic", "rice"}, []string{"rice"}},
		{"rice", []string{"wl-base", "river-classic", "rice"}, nil},
		{"wl-base", []string{"wl-base", "games"}, nil},
		{"wl-base", []string{"wl-base", "rice"}, []string{"rice"}}, // via a module that is not in the stack
		{"wl-base", []string{"wl-base", "deleted", "river-classic"}, []string{"river-classic"}},
	}
	for _, tt := range tests {
		t.Run(tt.target+"/"+strings.Join(tt.stack, ","), func(t *testing.T) {
			if got := s.Dependents(tt.target, tt.stack); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLocalPackagesNeedARecipe(t *testing.T) {
	dir := writeModule(t, t.TempDir(), "x11", "[packages]\nlocal = [\"dwm-arcus\"]\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "no recipe at pkg/dwm-arcus/PKGBUILD") {
		t.Fatalf("missing recipe: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "dwm-arcus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pkg", "dwm-arcus", "PKGBUILD"), []byte("pkgname=dwm-arcus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestRecipeHash(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	hash := func() string {
		t.Helper()
		h, err := RecipeHash(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	write("PKGBUILD", "pkgname=dwm-arcus", 0o644)
	write("config.h", "#define MODKEY Mod1Mask", 0o644)
	base := hash()
	if hash() != base {
		t.Fatal("hash must be stable")
	}

	// makepkg leftovers don't count.
	write("src/dwm/dwm.c", "built", 0o644)
	write("pkg/dwm-arcus/usr/bin/dwm", "binary", 0o755)
	write("dwm-arcus-6.8-1-x86_64.pkg.tar.zst", "package", 0o644)
	if hash() != base {
		t.Error("build output changed the hash")
	}

	write("config.h", "#define MODKEY Mod4Mask", 0o644)
	if hash() == base {
		t.Error("an edited config.h must change the hash")
	}
	edited := hash()
	write("config.h", "#define MODKEY Mod4Mask", 0o755)
	if hash() == edited {
		t.Error("the executable bit must change the hash")
	}
}
