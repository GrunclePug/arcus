package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

// cmdRebuild builds and reinstalls own builds even if their recipes didn't
// change, for example after a library they link against changed.
func (a *app) cmdRebuild(args []string) error {
	var o opts
	fs := a.flags("rebuild", &o, true, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: arcus rebuild [-y] [-n] <package...>")
	}
	a.rebuild = fs.Args()
	return a.change(&o, false, func(f *host.File) error {
		in, err := a.selection(f)
		if err != nil {
			return err
		}
		local := in.Declares(state.Local)
		for _, pkg := range a.rebuild {
			found := false
			for _, pkgs := range local {
				found = found || slices.Contains(pkgs, pkg)
			}
			if !found {
				return fmt.Errorf("%q is not an own build of a module that is on", pkg)
			}
		}
		return nil
	})
}

// pkgDir is where the package built from one exact recipe goes.
func (a *app) pkgDir(pkg, recipe string) string {
	return filepath.Join(a.paths.Cache, "pkg", pkg, recipe[:16])
}

// pkgName returns the package name of a package file name
// (<name>-<version>-<release>-<arch>.pkg.tar.<ext>).
func pkgName(file string) string {
	base, _, _ := strings.Cut(filepath.Base(file), ".pkg.tar.")
	parts := strings.Split(base, "-")
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[:len(parts)-3], "-")
}

// packageFile finds the package for pkg built from recipe. Other packages
// the same PKGBUILD produced (such as <pkg>-debug) are ignored.
func (a *app) packageFile(pkg, recipe string) (string, error) {
	dir := a.pkgDir(pkg, recipe)
	matches, _ := filepath.Glob(filepath.Join(dir, "*.pkg.tar.*"))
	matches = slices.DeleteFunc(matches, func(m string) bool {
		return strings.HasSuffix(m, ".sig") || pkgName(m) != pkg
	})
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no package %s in %s (does the PKGBUILD build pkgname=%s?)", pkg, dir, pkg)
	}
	return "", fmt.Errorf("several packages %s in %s: %s", pkg, dir, strings.Join(matches, ", "))
}

// build makes the package of an own build in the cache, as the user. A
// package already built from this exact recipe (by an earlier run that
// failed later) is reused, unless the build is a forced rebuild.
func (a *app) build(s plan.Step, r *system.Runner) error {
	pkg := s.Names[0]
	m := a.modules[s.Module]
	if m == nil {
		return fmt.Errorf("module %q does not exist", s.Module)
	}
	dir := m.RecipeDir(pkg)
	recipe, err := module.RecipeHash(dir)
	if err != nil {
		return err
	}
	if recipe != s.Recipe {
		return fmt.Errorf("the recipe of %s changed since the plan was shown; run the command again", pkg)
	}
	if slices.Contains(a.rebuild, pkg) {
		if err := os.RemoveAll(a.pkgDir(pkg, recipe)); err != nil {
			return err
		}
	}
	if _, err := a.packageFile(pkg, recipe); err == nil {
		fmt.Fprintf(a.stdout, "%s: using the package already built from this recipe\n", pkg)
		return nil
	}

	// makepkg writes into its directory, so it works on a copy: the recipe
	// in the dotfiles repository stays clean.
	work := filepath.Join(a.paths.Cache, "build", pkg)
	if err := os.RemoveAll(work); err != nil {
		return err
	}
	if err := copyTree(dir, work); err != nil {
		return fmt.Errorf("copying the recipe of %s: %w", pkg, err)
	}
	pkgdest, srcdest := a.pkgDir(pkg, recipe), filepath.Join(a.paths.Cache, "src")
	for _, d := range []string{pkgdest, srcdest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := r.Makepkg(work, pkgdest, srcdest); err != nil {
		return fmt.Errorf("building %s: %w", pkg, err)
	}
	_, err = a.packageFile(pkg, recipe)
	return err
}

// installLocal installs the packages of own builds in one transaction and
// records them: ownership for what was absent before, and the recipe each
// one was built from.
func (a *app) installLocal(st *state.State, steps []plan.Step, r *system.Runner, save func() error) error {
	var files []string
	for _, s := range steps {
		file, err := a.packageFile(s.Names[0], s.Recipe)
		if err != nil {
			return err
		}
		files = append(files, file)
	}
	before, err := system.Installed()
	if err != nil {
		return err
	}
	runErr := r.PacmanInstallFiles(files)
	after, err := system.Installed()
	if err != nil {
		return errors.Join(runErr, err)
	}
	for _, s := range steps {
		st.Own(state.Local, s.Module, state.Added(s.Names, before, after))
		if runErr == nil {
			if st.Built == nil {
				st.Built = map[string]string{}
			}
			st.Built[s.Names[0]] = s.Recipe
		}
	}
	return errors.Join(runErr, save())
}

// copyTree copies a recipe directory: regular files with their mode, and
// symlinks as they are. Build output left by an in-place makepkg is skipped,
// as in module.RecipeHash.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel == "src" || rel == "pkg" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		case strings.Contains(d.Name(), ".pkg.tar."):
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
}
