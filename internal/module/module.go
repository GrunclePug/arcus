// Package module loads module manifests and resolves requires and conflicts.
package module

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// ManifestFile is the file that makes a directory a module.
const ManifestFile = "module.toml"

var (
	moduleNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	// Arch package names: no leading hyphen or dot, so a name can never be
	// read as an option by pacman or the AUR helper.
	packageNameRegex = regexp.MustCompile(`^[a-zA-Z0-9@_+][a-zA-Z0-9@._+-]*$`)
	// Units need their type suffix so ownership compares exact names.
	unitNameRegex  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9:_.@\\-]*\.(service|socket|timer|path|target|mount|automount|swap)$`)
	groupNameRegex = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
)

// reservedNames cannot be module names: "orig" is the backup tag for host
// originals, and the others are suffixes arcus itself puts next to host files.
var reservedNames = map[string]bool{"orig": true, "arcus-foreign": true, "arcus-tmp": true}

// ValidateName rejects names that are unsafe as a directory name or
// ambiguous as a backup suffix.
func ValidateName(name string) error {
	if !moduleNameRegex.MatchString(name) {
		return fmt.Errorf("invalid name %q (use lowercase letters, digits, '-' and '_', starting with a letter or digit)", name)
	}
	if reservedNames[name] {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// Manifest is the content of module.toml. Every field is optional.
type Manifest struct {
	Description string   `toml:"description"`
	Category    string   `toml:"category"` // groups modules in ls and the installer
	Requires    []string `toml:"requires"`
	Conflicts   []string `toml:"conflicts"`
	Groups      []string `toml:"groups"`
	Dirs        []string `toml:"dirs"` // directories in $HOME that must exist
	// Links are symlinks in $HOME to other places in $HOME, both relative:
	// {".config/aurlock/approved" = "Sync/aurlock/approved"}.
	Links map[string]string `toml:"links"`

	Packages struct {
		Pacman []string `toml:"pacman"`
		AUR    []string `toml:"aur"`
		Local  []string `toml:"local"` // own builds, from pkg/<name>/
	} `toml:"packages"`

	Units struct {
		System []string `toml:"system"`
		User   []string `toml:"user"`
		Now    bool     `toml:"now"`
	} `toml:"units"`
}

// Module is a loaded module directory. Its name is the directory name.
type Module struct {
	Name string
	Dir  string
	Manifest
}

// Load reads the module in dir.
func Load(dir string) (*Module, error) {
	name := filepath.Base(dir)
	if err := ValidateName(name); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}

	path := filepath.Join(dir, ManifestFile)
	m := &Module{Name: name, Dir: dir}
	md, err := toml.DecodeFile(path, &m.Manifest)
	if err != nil {
		return nil, fmt.Errorf("loading module %q: %w", name, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

func (m *Module) validate() error {
	lists := []struct {
		field string
		names []string
		re    *regexp.Regexp
	}{
		{"requires", m.Requires, moduleNameRegex},
		{"conflicts", m.Conflicts, moduleNameRegex},
		{"groups", m.Groups, groupNameRegex},
		{"packages.pacman", m.Packages.Pacman, packageNameRegex},
		{"packages.aur", m.Packages.AUR, packageNameRegex},
		{"packages.local", m.Packages.Local, packageNameRegex},
		{"units.system", m.Units.System, unitNameRegex},
		{"units.user", m.Units.User, unitNameRegex},
	}
	for i, d := range m.Dirs {
		if !homeRelative(d) {
			return fmt.Errorf("dirs: %q must be a relative path inside $HOME", d)
		}
		if slices.Contains(m.Dirs[:i], d) {
			return fmt.Errorf("dirs: %q is listed twice", d)
		}
	}
	for p, target := range m.Links {
		for _, s := range []string{p, target} {
			if !homeRelative(s) {
				return fmt.Errorf("links: %q must be a relative path inside $HOME", s)
			}
		}
		if p == target {
			return fmt.Errorf("links: %q points at itself", p)
		}
	}
	if m.Category != "" && !moduleNameRegex.MatchString(m.Category) {
		return fmt.Errorf("category: invalid name %q (lowercase letters, digits, '-' and '_')", m.Category)
	}
	for _, l := range lists {
		for i, n := range l.names {
			if !l.re.MatchString(n) {
				return fmt.Errorf("%s: invalid name %q", l.field, n)
			}
			if slices.Contains(l.names[:i], n) {
				return fmt.Errorf("%s: %q is listed twice", l.field, n)
			}
		}
	}

	for _, n := range m.Requires {
		if n == m.Name {
			return fmt.Errorf("requires: module cannot require itself")
		}
		if slices.Contains(m.Conflicts, n) {
			return fmt.Errorf("%q is in both requires and conflicts", n)
		}
	}
	if slices.Contains(m.Conflicts, m.Name) {
		return fmt.Errorf("conflicts: module cannot conflict with itself")
	}
	for _, n := range m.Packages.Local {
		if info, err := os.Stat(filepath.Join(m.RecipeDir(n), "PKGBUILD")); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("packages.local: %q has no recipe at pkg/%s/PKGBUILD", n, n)
		}
	}
	return nil
}

// RecipeDir returns the directory of the recipe for the own build pkg.
func (m *Module) RecipeDir(pkg string) string {
	return filepath.Join(m.Dir, "pkg", pkg)
}

// RecipeHash identifies the content of a recipe directory: every file's
// path, executable bit and content, or a symlink's target. Build output that
// makepkg leaves when run in place (src/, pkg/, *.pkg.tar.*) is ignored.
func RecipeHash(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "src" || rel == "pkg" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(d.Name(), ".pkg.tar.") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00link\x00%s\x00", rel, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%t\x00%d\x00", rel, info.Mode()&0o111 != 0, len(data))
			h.Write(data)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hashing recipe %s: %w", dir, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Set is every available module, keyed by name.
type Set map[string]*Module

// Scan loads the modules in dirs. The same name in two directories is an
// error. Missing directories and subdirectories without a module.toml are
// skipped.
func Scan(dirs ...string) (Set, error) {
	set := Set{}
	for _, dir := range dirs {
		if err := set.scan(dir); err != nil {
			return nil, err
		}
	}
	return set, nil
}

func (s Set) scan(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing modules: %w", err)
	}

	for _, e := range entries {
		mdir := filepath.Join(dir, e.Name())
		// Stat follows symlinks: module directories are often links into a
		// dotfiles repository.
		if info, err := os.Stat(mdir); err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(mdir, ManifestFile)); errors.Is(err, iofs.ErrNotExist) {
			continue
		}

		m, err := Load(mdir)
		if err != nil {
			return err
		}
		if prev, ok := s[m.Name]; ok {
			return fmt.Errorf("module %q is defined twice: %s and %s", m.Name, prev.Dir, m.Dir)
		}
		s[m.Name] = m
	}
	return nil
}

// CompareCategories orders categories for display: system, then hardware
// (the installer's own), then the rest by name, uncategorized ("") last.
func CompareCategories(a, b string) int {
	rank := func(c string) int {
		switch c {
		case "system":
			return 0
		case "hardware":
			return 1
		case "":
			return 3
		}
		return 2
	}
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
}

func homeRelative(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != "." && !strings.HasPrefix(p, "..")
}

// LinkNames are the module's links as resource names, "path -> target", in
// order. The name holds both, so changing a link's target releases the old
// link and adds the new one.
func (m *Manifest) LinkNames() []string {
	var names []string
	for _, p := range slices.Sorted(maps.Keys(m.Links)) {
		names = append(names, p+linkArrow+m.Links[p])
	}
	return names
}

const linkArrow = " -> "

// SplitLink returns the path and target of a link name.
func SplitLink(name string) (p, target string, ok bool) {
	return strings.Cut(name, linkArrow)
}
