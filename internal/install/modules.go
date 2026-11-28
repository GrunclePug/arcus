package install

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/grunclepug/arcus/examples"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/profile"
)

// WriteConfig writes the embedded starter config (config.toml, profiles/ and
// modules/) into dir, used when a plan has no dotfiles repository.
func WriteConfig(dir string) error {
	if err := os.CopyFS(dir, examples.FS); err != nil {
		return fmt.Errorf("writing the starter config: %w", err)
	}
	return nil
}

// MachineFacts returns the facts of the machine module for the plan on this
// machine (root: its /proc and /sys). The installed system finds the same facts with
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
		GPUs:        machine.GPUs(root),
		IntelSOF:    machine.IntelSOF(root),
		Zram:        p.Disk.Swap == "zram",
		SSD:         machine.SSD(root),
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
