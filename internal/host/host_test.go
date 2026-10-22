package host

import (
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		modules []string
		want    string
	}{
		{
			name:    "comments outside the array survive",
			src:     "# Desired modules for this machine.\n# Edit freely.\n\nmodules = [\n  \"base\",\n]\n\n# trailing note\n",
			modules: []string{"base", "wl-base"},
			want:    "# Desired modules for this machine.\n# Edit freely.\n\nmodules = [\n  \"base\",\n  \"wl-base\",\n]\n\n# trailing note\n",
		},
		{
			name:    "single-line array becomes one entry per line",
			src:     "modules = [\"base\", 'cpu-amd'] # same line comment\n",
			modules: []string{"base", "cpu-amd", "laptop"},
			want:    "modules = [\n  \"base\",\n  \"cpu-amd\",\n  \"laptop\",\n] # same line comment\n",
		},
		{
			name:    "brackets in comments and the commented-out key are not the array",
			src:     "# modules = [\"old\"]\nmodules = [\n  \"base\", # see [notes]\n  # \"river-classic\", ] disabled\n]\n",
			modules: []string{"base"},
			want:    "# modules = [\"old\"]\nmodules = [\n  \"base\",\n]\n",
		},
		{
			name:    "removing the last entry leaves an empty array",
			src:     "modules=[\"base\"]",
			modules: []string{},
			want:    "modules = [\n]",
		},
		{
			name:    "missing key is appended",
			src:     "# new machine",
			modules: []string{"base"},
			want:    "# new machine\nmodules = [\n  \"base\",\n]\n",
		},
		{
			name:    "empty file",
			src:     "",
			modules: []string{"base"},
			want:    "modules = [\n  \"base\",\n]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := render([]byte(tt.src), "modules", tt.modules)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestRenderUnclosedArray(t *testing.T) {
	if _, err := render([]byte("modules = [\n  \"base\",\n"), "modules", nil); err == nil {
		t.Fatal("want error for an unclosed array")
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct{ name, src, wantErr string }{
		{"unknown key", "modules = []\nmodule = []", "unknown keys: module"},
		{"duplicate", `modules = ["base", "base"]`, "listed twice"},
		{"bad name", `modules = ["Base"]`, "invalid name"},
		{"bad toml", `modules = [`, "toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "h.toml")
			if err := os.WriteFile(path, []byte(tt.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLoadMissingIsNotEmpty(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestAddRemoveSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, err := Path(dir, "arch-e14")
	if err != nil {
		t.Fatal(err)
	}

	f := New(path)
	f.AddModule("base")
	f.AddModule("wl-base")
	if f.AddModule("base") {
		t.Error("Add of a present module must report false")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	f, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.RemoveModule("base") || f.RemoveModule("base") {
		t.Error("Remove must report whether the module was there")
	}
	f.AddModule("river-classic")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	f, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"wl-base", "river-classic"}; !slices.Equal(f.Modules, want) {
		t.Fatalf("modules = %v, want %v", f.Modules, want)
	}
}

// The host file usually lives in the dotfiles repository; saving must write
// through a symlink, not replace it, and keep the file's mode.
func TestSaveWritesThroughSymlink(t *testing.T) {
	repo, config := t.TempDir(), t.TempDir()
	real := filepath.Join(repo, "h.toml")
	if err := os.WriteFile(real, []byte("# mine\nmodules = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(config, "h.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	f, err := Load(link)
	if err != nil {
		t.Fatal(err)
	}
	f.AddModule("base")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced: %v, %v", info, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(real)
	if !strings.HasPrefix(string(data), "# mine\n") || !strings.Contains(string(data), `"base"`) {
		t.Errorf("content = %q", data)
	}
}

func TestPathRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "-x"} {
		if _, err := Path("/cfg", bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if p, err := Path("/cfg", "arch-e14"); err != nil || p != "/cfg/hosts/arch-e14.toml" {
		t.Errorf("Path = %q, %v", p, err)
	}
}

func TestProfilesAndModules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.toml")
	src := "# laptop\nprofiles = [\"main\"]\n\n# machine extras\nmodules = []\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Profiles, []string{"main"}) || len(f.Modules) != 0 {
		t.Fatalf("loaded %+v", f)
	}
	f.AddModule("laptop")
	f.AddProfile("gaming")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "# laptop\nprofiles = [\n  \"main\",\n  \"gaming\",\n]\n\n# machine extras\nmodules = [\n  \"laptop\",\n]\n"
	if string(data) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", data, want)
	}

	// A missing key with an empty list is not added.
	f = New(filepath.Join(t.TempDir(), "new.toml"))
	f.AddModule("shell")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(f.Path)
	if string(data) != "modules = [\n  \"shell\",\n]\n" {
		t.Fatalf("new file:\n%s", data)
	}
}
