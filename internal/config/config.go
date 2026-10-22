// Package config loads ~/.config/arcus/config.toml and resolves the
// directories arcus works with.
package config

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/grunclepug/arcus/internal/machine"
)

// DefaultProtected are packages arcus never removes, even when it installed
// them; the configured AUR helper (and its -bin package) is added to them. A
// protected list in config.toml extends this one.
var DefaultProtected = []string{"base", "base-devel", "linux", "linux-firmware", "sudo", "pacman", "systemd"}

// AURHelpers are the supported AUR helpers. Both take pacman's -S --needed
// and -Rns and call sudo themselves.
var AURHelpers = []string{"yay", "paru"}

// Config is the content of config.toml. Every field is optional.
type Config struct {
	AURHelper  string   `toml:"aur_helper"`
	Protected  []string `toml:"protected"`
	ModuleDirs []string `toml:"module_dirs"`
}

// Load reads path. A missing file gives the defaults. home expands a
// leading "~/" in module_dirs.
func Load(path, home string) (*Config, error) {
	c := &Config{}
	md, err := toml.DecodeFile(path, c)
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}

	if c.AURHelper == "" {
		c.AURHelper = "yay"
	}
	if !slices.Contains(AURHelpers, c.AURHelper) {
		return nil, fmt.Errorf("%s: aur_helper must be one of %s, got %q", path, strings.Join(AURHelpers, ", "), c.AURHelper)
	}
	for _, p := range slices.Concat(DefaultProtected, []string{c.AURHelper, c.AURHelper + "-bin"}) {
		if !slices.Contains(c.Protected, p) {
			c.Protected = append(c.Protected, p)
		}
	}
	for i, dir := range c.ModuleDirs {
		if rest, ok := strings.CutPrefix(dir, "~/"); ok {
			dir = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("%s: module_dirs entry %q must be absolute or start with ~/", path, c.ModuleDirs[i])
		}
		c.ModuleDirs[i] = filepath.Clean(dir)
	}
	return c, nil
}

// Paths are the directories arcus reads and writes.
type Paths struct {
	Home        string // the main user's $HOME, where home/ trees deploy
	Config      string // ~/.config/arcus: config.toml, hosts/, modules/
	UserState   string // ~/.local/state/arcus: user-half state and ledgers
	SystemState string // <root>/var/lib/arcus: system-half state and ledgers
	Cache       string // ~/.cache/arcus: own builds (build/, pkg/, src/)
	Machine     string // <root>/etc/arcus/modules: machine-local modules
}

// UserPaths resolves the paths for the user home, honouring XDG_CONFIG_HOME,
// XDG_STATE_HOME and XDG_CACHE_HOME through getenv. root is the root prefix
// ("/" outside tests).
func UserPaths(home, root string, getenv func(string) string) Paths {
	p := Paths{
		Home:        home,
		Config:      filepath.Join(home, ".config", "arcus"),
		UserState:   filepath.Join(home, ".local", "state", "arcus"),
		Cache:       filepath.Join(home, ".cache", "arcus"),
		SystemState: filepath.Join(root, "var", "lib", "arcus"),
		Machine:     filepath.Join(root, machine.Dir),
	}
	if dir := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		p.Config = filepath.Join(dir, "arcus")
	}
	if dir := getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		p.UserState = filepath.Join(dir, "arcus")
	}
	if dir := getenv("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
		p.Cache = filepath.Join(dir, "arcus")
	}
	return p
}

// ModuleDirs returns the module directories: config/modules, the extra
// ones from config.toml, then the machine-local ones.
func (p Paths) ModuleDirs(c *Config) []string {
	return slices.Concat([]string{filepath.Join(p.Config, "modules")}, c.ModuleDirs, []string{p.Machine})
}
