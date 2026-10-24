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
)

const moduleTemplate = `# Files in home/ are linked into $HOME. Files in system/etc and
# system/usr/local are copied to /etc and /usr/local, owned by root.
# Each own build in packages.local has its PKGBUILD in pkg/<name>/.
description = ""
category  = ""

requires  = []
conflicts = []
groups    = []
dirs      = []

[packages]
pacman = []
aur    = []
local  = []

[units]
system = []
user   = []
now    = false
`

// cmdNew creates a module skeleton in ~/.config/arcus/modules.
func (a *app) cmdNew(args []string) error {
	var o opts
	fs := a.flags("new", &o, false, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus new <name>")
	}
	name := fs.Arg(0)
	if err := module.ValidateName(name); err != nil {
		return err
	}
	if err := a.load(&o); err != nil {
		return err
	}
	if a.profiles[name] != nil {
		return fmt.Errorf("%q is already a profile (%s)", name, a.profiles[name].Path)
	}
	if m := a.modules[name]; m != nil {
		return fmt.Errorf("module %q already exists at %s", name, m.Dir)
	}

	dir := filepath.Join(a.paths.Config, "modules", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, module.ManifestFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("creating module %q: %w", name, err)
	}
	if _, err := f.WriteString(moduleTemplate); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Created %s.\n", dir)
	return nil
}

// module returns the user module called name; machine-local ones belong to
// arcus machine.
func (a *app) module(name string) (*module.Module, error) {
	m := a.modules[name]
	switch {
	case m == nil:
		return nil, fmt.Errorf("module %q does not exist", name)
	case filepath.Dir(m.Dir) == a.paths.Machine:
		return nil, fmt.Errorf("%q is machine-local (%s); 'arcus machine' writes it", name, m.Dir)
	}
	return m, nil
}

// references lists the modules (requires, conflicts) and profiles that name
// a module.
func (a *app) references(name string) []string {
	var refs []string
	for other, m := range a.modules {
		if slices.Contains(m.Requires, name) || slices.Contains(m.Conflicts, name) {
			refs = append(refs, "module "+other)
		}
	}
	for other, p := range a.profiles {
		if slices.Contains(p.Modules, name) {
			refs = append(refs, "profile "+other)
		}
	}
	slices.Sort(refs)
	return refs
}

// inUse fails if the host file wants name on, a profile or another module
// names it, or either half still records anything for it.
func (a *app) inUse(o *opts, name string) error {
	if refs := a.references(name); len(refs) > 0 {
		return fmt.Errorf("%s refer to %q; remove those references first", strings.Join(refs, ", "), name)
	}
	path, err := a.hostPath()
	if err != nil {
		return err
	}
	if f, err := host.Load(path); err == nil {
		if in, err := a.selection(f); err == nil && slices.Contains(in.Desired, name) {
			return fmt.Errorf("%q is on; turn it off first ('arcus off %s')", name, name)
		}
	} else if !errors.Is(err, iofs.ErrNotExist) {
		return err
	}

	for _, h := range []*half{a.userHalf(), a.systemHalf()} {
		st, err := h.loadState()
		if err != nil {
			return err
		}
		if used, err := hasRecords(h, st, name); err != nil || used {
			return errors.Join(err, fmt.Errorf("the %s half still records files or resources of %q; run 'arcus apply' first", h.name, name))
		}
	}
	return nil
}

// cmdRm deletes a user module that is off and leaves nothing behind.
func (a *app) cmdRm(args []string) error {
	var o opts
	fs := a.flags("rm", &o, true, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus rm [-y] <name>")
	}
	name := fs.Arg(0)
	if err := a.load(&o); err != nil {
		return err
	}
	lock, err := a.userHalf().lock(true)
	if err != nil {
		return err
	}
	defer lock.Release()

	m, err := a.module(name)
	if err != nil {
		return err
	}
	if err := a.inUse(&o, name); err != nil {
		return err
	}
	if o.dry {
		fmt.Fprintf(a.stdout, "Would delete %s.\n", m.Dir)
		return nil
	}
	if !o.yes && !a.confirm(fmt.Sprintf("Delete %s?", m.Dir)) {
		return errors.New("cancelled, nothing was deleted")
	}

	// A module directory that is a symlink (into a dotfiles repository, say)
	// loses only the link.
	if info, err := os.Lstat(m.Dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(m.Dir); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed the link %s; its target was left alone.\n", m.Dir)
	} else {
		if err := os.RemoveAll(m.Dir); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Deleted %s.\n", m.Dir)
	}
	return nil
}

// cmdMv renames a user module, including its deployed layers, ownership
// records and its entry in the host file.
func (a *app) cmdMv(args []string) error {
	var o opts
	fs := a.flags("mv", &o, false, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: arcus mv <old> <new>")
	}
	oldName, newName := fs.Arg(0), fs.Arg(1)
	if err := module.ValidateName(newName); err != nil {
		return err
	}
	if err := a.load(&o); err != nil {
		return err
	}
	uh := a.userHalf()
	lock, err := uh.lock(true)
	if err != nil {
		return err
	}
	defer lock.Release()

	m, err := a.module(oldName)
	if err != nil {
		return err
	}
	if a.modules[newName] != nil || a.profiles[newName] != nil {
		return fmt.Errorf("a module or profile named %q already exists", newName)
	}
	if refs := a.references(oldName); len(refs) > 0 {
		return fmt.Errorf("%s refer to %q; change them to %q first", strings.Join(refs, ", "), oldName, newName)
	}
	oldDir, newDir := m.Dir, filepath.Join(filepath.Dir(m.Dir), newName)
	if _, err := os.Lstat(newDir); err == nil {
		return fmt.Errorf("%s already exists", newDir)
	}

	path, err := a.hostPath()
	if err != nil {
		return err
	}
	f, err := host.Load(path)
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return err
	}
	sh := a.systemHalf()
	sst, err := sh.loadState()
	if err != nil {
		return err
	}
	inSystem, err := hasRecords(sh, sst, oldName)
	if err != nil {
		return err
	}

	if err := os.Rename(oldDir, newDir); err != nil {
		return fmt.Errorf("renaming the module directory: %w", err)
	}
	delete(a.modules, oldName)
	m.Name, m.Dir = newName, newDir
	a.modules[newName] = m

	if err := a.renameHalf(a.userHalf(), oldName, newName, oldDir, newDir); err != nil {
		return err
	}
	if inSystem {
		args := []string{"-mv", "-home", a.paths.Home, "-config", a.paths.Config, "--", oldName, newName, oldDir, newDir}
		if err := a.sudo(args); err != nil {
			return fmt.Errorf("renaming the system layer (run 'arcus doctor'): %w", err)
		}
	}
	if f != nil {
		if i := slices.Index(f.Modules, oldName); i >= 0 {
			f.Modules[i] = newName
			if err := f.Save(); err != nil {
				return err
			}
		}
	}
	fmt.Fprintf(a.stdout, "Renamed %q to %q.\n", oldName, newName)
	return nil
}
