// Package profile loads profiles: named, ordered lists of modules, one
// <name>.toml file each, that several machines can share. A host file lists
// profiles and adds its own modules on top.
package profile

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/grunclepug/arcus/internal/module"
)

// Profile is a loaded profile file. Its name is the file name without .toml.
type Profile struct {
	Name        string   `toml:"-"`
	Path        string   `toml:"-"`
	Description string   `toml:"description"`
	Modules     []string `toml:"modules"`
}

// Load reads the profile file at path.
func Load(path string) (*Profile, error) {
	p := &Profile{Name: strings.TrimSuffix(filepath.Base(path), ".toml"), Path: path}
	if err := module.ValidateName(p.Name); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	md, err := toml.DecodeFile(path, p)
	if err != nil {
		return nil, fmt.Errorf("loading profile %q: %w", p.Name, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	for i, name := range p.Modules {
		if err := module.ValidateName(name); err != nil {
			return nil, fmt.Errorf("%s: modules: %w", path, err)
		}
		if slices.Contains(p.Modules[:i], name) {
			return nil, fmt.Errorf("%s: module %q is listed twice", path, name)
		}
	}
	return p, nil
}

// Set is every available profile, keyed by name.
type Set map[string]*Profile

// Scan loads every *.toml file in dir. A missing dir has no profiles.
func Scan(dir string) (Set, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, iofs.ErrNotExist) {
		return Set{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing profiles: %w", err)
	}
	set := Set{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		p, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		set[p.Name] = p
	}
	return set, nil
}

// Host is the source recorded for modules a host file lists itself.
const Host = ""

// Expand returns the modules selected by profiles, in order, followed by the
// host's own modules, each once (first occurrence wins). via records where
// each module was selected: a profile name, or Host.
func (s Set) Expand(profiles, modules []string) (selected []string, via map[string]string, err error) {
	via = map[string]string{}
	pick := func(name, from string) {
		if _, ok := via[name]; !ok {
			via[name] = from
			selected = append(selected, name)
		}
	}
	for _, name := range profiles {
		p, ok := s[name]
		if !ok {
			return nil, nil, fmt.Errorf("profile %q does not exist", name)
		}
		for _, m := range p.Modules {
			pick(m, name)
		}
	}
	for _, m := range modules {
		pick(m, Host)
	}
	return selected, via, nil
}
