package install

import (
	"embed"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/profile"
)

// modules are the installer modules, written to the new system when the
// plan has no dotfiles repository.
//
//go:embed modules
var modules embed.FS

// WriteModules writes the installer modules into dir.
func WriteModules(dir string) error {
	err := iofs.WalkDir(modules, "modules", func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, strings.TrimPrefix(path, "modules"))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := modules.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("writing the installer modules: %w", err)
	}
	return nil
}

// MachineFacts returns the facts of the machine module for the plan on this
// machine (root: its /proc). The installed system finds the same facts with
// 'arcus machine'.
func MachineFacts(p *Plan, uefi bool, root string) machine.Facts {
	boot := "ext4"
	if uefi {
		boot = "vfat"
	}
	filesystems := []string{p.Disk.Filesystem, boot}
	slices.Sort(filesystems)
	return machine.Facts{
		UEFI: uefi, Kernels: []string{p.System.Kernel}, Firmware: true, GRUB: true,
		Filesystems: slices.Compact(filesystems),
		CPU:         machine.CPU(filepath.Join(root, "proc/cpuinfo")),
		Zram:        p.Disk.Swap == "zram",
	}
}

// Host returns the lists of the plan's host file: its profiles and modules,
// with the machine module first unless a profile already brings it in.
func Host(p *Plan, profiles profile.Set) ([]string, []string, error) {
	selected, _, err := profiles.Expand(p.Host.Profiles, p.Host.Modules)
	if err != nil {
		return nil, nil, err
	}
	modules := slices.DeleteFunc(slices.Clone(p.Host.Modules), func(n string) bool { return n == machine.Name })
	if !slices.Contains(selected, machine.Name) || slices.Contains(p.Host.Modules, machine.Name) {
		modules = append([]string{machine.Name}, modules...)
	}
	return slices.Clone(p.Host.Profiles), modules, nil
}
