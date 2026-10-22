package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.toml"), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if c.AURHelper != "yay" || !slices.Equal(c.Protected, append(slices.Clone(DefaultProtected), "yay", "yay-bin")) || len(c.ModuleDirs) != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name, src, wantErr string
		check              func(*Config) bool
	}{
		{"extends protected", `protected = ["neovim", "go"]`, "", func(c *Config) bool {
			return slices.Contains(c.Protected, "neovim") && slices.Contains(c.Protected, "base") &&
				len(c.Protected) == len(DefaultProtected)+4
		}},
		{"expands home", `module_dirs = ["~/dot/modules", "/srv/p/"]`, "", func(c *Config) bool {
			return slices.Equal(c.ModuleDirs, []string{"/home/u/dot/modules", "/srv/p"})
		}},
		{"paru", `aur_helper = "paru"`, "", func(c *Config) bool { return c.AURHelper == "paru" && slices.Contains(c.Protected, "paru") }},
		{"unknown helper", `aur_helper = "pikaur"`, "aur_helper must be", nil},
		{"relative dir", `module_dirs = ["modules"]`, "must be absolute", nil},
		{"unknown key", `aur = "yay"`, "unknown keys: aur", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.src), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path, "/home/u")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(c) {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestUserPaths(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	p := UserPaths("/home/u", "/", getenv)
	if p.Config != "/home/u/.config/arcus" || p.UserState != "/home/u/.local/state/arcus" || p.Cache != "/home/u/.cache/arcus" ||
		p.SystemState != "/var/lib/arcus" {
		t.Fatalf("defaults = %+v", p)
	}

	env["XDG_CONFIG_HOME"], env["XDG_STATE_HOME"] = "/x/config", "relative/ignored"
	p = UserPaths("/home/u", "/tmp/root", getenv)
	if p.Config != "/x/config/arcus" || p.UserState != "/home/u/.local/state/arcus" || p.SystemState != "/tmp/root/var/lib/arcus" {
		t.Fatalf("xdg = %+v", p)
	}
	if dirs := p.ModuleDirs(&Config{ModuleDirs: []string{"/extra"}}); !slices.Equal(dirs, []string{"/x/config/arcus/modules", "/extra", "/tmp/root/etc/arcus/modules"}) {
		t.Fatalf("module dirs = %v", dirs)
	}
}
