package profile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanAndLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "main.toml", "# shared by every machine\ndescription = \"daily setup\"\nmodules = [\"shell\", \"nvim\"]\n")
	write(t, dir, "README", "not a profile")
	if err := os.Mkdir(filepath.Join(dir, "sub.toml"), 0o755); err != nil {
		t.Fatal(err)
	}

	set, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || set["main"] == nil || set["main"].Description != "daily setup" ||
		!slices.Equal(set["main"].Modules, []string{"shell", "nvim"}) {
		t.Fatalf("set = %+v", set["main"])
	}
	if set, err := Scan(filepath.Join(dir, "missing")); err != nil || len(set) != 0 {
		t.Fatalf("missing dir: %v, %v", set, err)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct{ file, src, wantErr string }{
		{"main.toml", `module = ["shell"]`, "unknown keys: module"},
		{"main.toml", `modules = ["shell", "shell"]`, "listed twice"},
		{"main.toml", `modules = ["Shell"]`, "invalid name"},
		{"Main.toml", ``, "invalid name"},
		{"orig.toml", ``, "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.wantErr, func(t *testing.T) {
			_, err := Load(write(t, t.TempDir(), tt.file, tt.src))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	set := Set{
		"main":   {Name: "main", Modules: []string{"shell", "nvim", "wl-base", "river-classic"}},
		"gaming": {Name: "gaming", Modules: []string{"steam", "shell"}},
	}
	selected, via, err := set.Expand([]string{"main", "gaming"}, []string{"laptop", "nvim"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"shell", "nvim", "wl-base", "river-classic", "steam", "laptop"}; !slices.Equal(selected, want) {
		t.Fatalf("selected = %v, want %v", selected, want)
	}
	for m, from := range map[string]string{"shell": "main", "steam": "gaming", "laptop": Host, "nvim": "main"} {
		if via[m] != from {
			t.Errorf("%s via %q, want %q", m, via[m], from)
		}
	}
	if _, _, err := set.Expand([]string{"ghost"}, nil); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown profile: %v", err)
	}
}
