package state

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestLockExcludesConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")

	first, err := AcquireLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path, true); err == nil || !strings.Contains(err.Error(), "another arcus process") {
		t.Fatalf("second exclusive lock: %v", err)
	}
	if _, err := AcquireLock(path, false); err == nil {
		t.Fatal("shared lock must wait for the writer")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	r1, err := AcquireLock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := AcquireLock(path, false)
	if err != nil {
		t.Fatal("readers should share:", err)
	}
	r1.Release()
	r2.Release()
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Modules == nil || len(s.Modules) != 0 {
		t.Fatalf("missing file must load as an empty state, got %+v", s)
	}

	s.Modules = []string{"base", "wl-base"}
	s.Own(Pacman, "wl-base", []string{"foot", "wmenu"})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("got %+v, want %+v", got, s)
	}
}

func TestStateRejectsDuplicateEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := &State{Modules: []string{"a", "a"}}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("duplicate stack entries must be rejected")
	}
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestAdded(t *testing.T) {
	tests := []struct {
		name          string
		wanted        []string
		before, after map[string]bool
		want          []string
	}{
		{"packages", []string{"river", "foot", "go", "virtual-provider"},
			set("go", "glibc"), set("go", "glibc", "river", "foot"), []string{"river", "foot"}},
		{"units already enabled are not owned", []string{"bluetooth.service", "fstrim.timer"},
			set("fstrim.timer"), set("fstrim.timer", "bluetooth.service"), []string{"bluetooth.service"}},
		{"groups", []string{"video", "input"}, set("wheel", "input"), set("wheel", "input", "video"), []string{"video"}},
		{"nothing added", []string{"go"}, set("go"), set("go"), []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Added(tt.wanted, tt.before, tt.after); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// Resources that were already present (never owned), are listed by another
// module that's on, or are protected are never removed.
func TestRemovals(t *testing.T) {
	tests := []struct {
		name      string
		owned     []string
		order     []string
		others    map[string][]string
		protected []string
		present   map[string]bool
		want      Removal
	}{
		{
			name:  "sole owner removes everything",
			owned: []string{"river", "foot"},
			want:  Removal{Remove: []string{"river", "foot"}, Transfer: map[string][]string{}},
		},
		{
			name:   "shared resource transfers to the first module in stack order",
			owned:  []string{"river", "foot", "fuzzel"},
			order:  []string{"mangowc", "niri"},
			others: map[string][]string{"mangowc": {"mangowm", "foot"}, "niri": {"niri", "foot", "fuzzel"}},
			want: Removal{
				Remove:   []string{"river"},
				Transfer: map[string][]string{"mangowc": {"foot"}, "niri": {"fuzzel"}},
			},
		},
		{
			name:      "protected toolchains are never removed",
			owned:     []string{"go", "rust", "river"},
			protected: []string{"go", "rust"},
			want:      Removal{Remove: []string{"river"}, Transfer: map[string][]string{}},
		},
		{
			name:      "protected packages still move to a module that declares them",
			owned:     []string{"aurlock", "git"},
			order:     []string{"aurlock"},
			others:    map[string][]string{"aurlock": {"aurlock"}},
			protected: []string{"aurlock"},
			want:      Removal{Remove: []string{"git"}, Transfer: map[string][]string{"aurlock": {"aurlock"}}},
		},
		{
			name:    "already-removed resources are skipped",
			owned:   []string{"river", "foot"},
			present: set("river"),
			want:    Removal{Remove: []string{"river"}, Transfer: map[string][]string{}},
		},
		{
			name:   "units follow the same rules",
			owned:  []string{"bluetooth.service", "cups.socket"},
			order:  []string{"laptop"},
			others: map[string][]string{"laptop": {"bluetooth.service"}},
			want:   Removal{Remove: []string{"cups.socket"}, Transfer: map[string][]string{"laptop": {"bluetooth.service"}}},
		},
		{
			name:    "groups the user left by hand are skipped",
			owned:   []string{"video", "input"},
			present: set("video"),
			want:    Removal{Remove: []string{"video"}, Transfer: map[string][]string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Removals(tt.owned, tt.order, tt.others, tt.protected, tt.present)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestReleaseHandsOverTransfers(t *testing.T) {
	s := &State{}
	s.Own(Pacman, "river", []string{"river", "foot"})
	s.Own(Pacman, "niri", []string{"niri"})
	s.Own(Pacman, "niri", []string{"niri"}) // recorded once

	r := Removals(s.Owned[Pacman]["river"], []string{"niri"}, map[string][]string{"niri": {"niri", "foot"}}, nil, nil)
	s.Release(Pacman, "river", r)

	if got := s.Owned[Pacman]["river"]; got != nil {
		t.Errorf("river still owns %v", got)
	}
	if got, want := s.Owned[Pacman]["niri"], []string{"niri", "foot"}; !slices.Equal(got, want) {
		t.Errorf("niri owns %v, want %v", got, want)
	}

	s.Release(Pacman, "niri", Removals(s.Owned[Pacman]["niri"], nil, nil, nil, nil))
	if len(s.Owned) != 0 {
		t.Errorf("empty records must be dropped, got %v", s.Owned)
	}
}

func TestSharedLockOnMissingFileIsNoop(t *testing.T) {
	l, err := AcquireLock(filepath.Join(t.TempDir(), "missing", ".lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

// Reconcile covers every way a resource can stop being wanted: its module
// is turned off, or the module stops listing it while it stays on.
func TestReconcile(t *testing.T) {
	s := &State{}
	s.Own(Pacman, "river-classic", []string{"river", "foot"})
	s.Own(Pacman, "wl-base", []string{"wmenu", "mako", "go"})
	s.Own(Pacman, "gone", []string{"oldpkg", "vanished"})

	order := []string{"wl-base", "niri"}
	declares := map[string][]string{
		"wl-base": {"wmenu", "foot"}, // mako dropped from the module
		"niri":    {"niri", "river"},
	}
	before := s.Clone()
	remove := s.Reconcile(Pacman, order, declares, []string{"go"}, set("river", "foot", "wmenu", "mako", "go", "oldpkg"))

	if want := []string{"oldpkg", "mako"}; !slices.Equal(remove, want) {
		t.Errorf("remove = %v, want %v", remove, want)
	}
	if got := s.Owned[Pacman]["wl-base"]; !slices.Equal(got, []string{"foot", "wmenu"}) && !slices.Equal(got, []string{"wmenu", "foot"}) {
		t.Errorf("wl-base owns %v, want wmenu and the transferred foot", got)
	}
	if got := s.Owned[Pacman]["niri"]; !slices.Equal(got, []string{"river"}) {
		t.Errorf("niri owns %v, want the transferred river", got)
	}
	for _, p := range []string{"river-classic", "gone"} {
		if got := s.Owned[Pacman][p]; got != nil {
			t.Errorf("%s still owns %v", p, got)
		}
	}
	if !reflect.DeepEqual(before.Owned[Pacman]["river-classic"], []string{"river", "foot"}) {
		t.Error("Clone must not share records with the original")
	}
}

func TestPruneRenameHas(t *testing.T) {
	s := &State{Modules: []string{"a", "old"}}
	s.Own(Pacman, "old", []string{"foot", "gone"})
	s.Own(Pacman, "a", []string{"gone2"})

	if got := s.Prune(Pacman, set("foot")); !slices.Equal(got, []string{"gone", "gone2"}) {
		t.Errorf("pruned %v", got)
	}
	if s.Has("a") != true || s.Owned[Pacman]["a"] != nil {
		t.Errorf("a: has=%v owned=%v", s.Has("a"), s.Owned[Pacman]["a"])
	}

	s.Rename("old", "new")
	if s.Has("old") || !slices.Equal(s.Modules, []string{"a", "new"}) || !slices.Equal(s.Owned[Pacman]["new"], []string{"foot"}) {
		t.Errorf("after rename: %+v", s)
	}
}

func TestStateRejectsOtherFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for _, src := range []string{`{"modules": []}`, `{"version": 2, "modules": []}`} {
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "format") {
			t.Errorf("%s: got %v", src, err)
		}
	}
}
