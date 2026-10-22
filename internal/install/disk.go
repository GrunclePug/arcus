package install

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// partition is one entry of the disk layout.
type partition struct {
	use  string // esp, bios, boot, swap or root
	size string // sfdisk size; empty takes the rest of the disk
	typ  string // sfdisk type
}

// biosBoot is the GPT type of the BIOS boot partition GRUB embeds itself in.
const biosBoot = "21686148-6449-6E6F-744E-656564454649"

func layout(p *Plan, uefi bool) []partition {
	var parts []partition
	if uefi {
		parts = append(parts, partition{"esp", "1GiB", "uefi"})
	} else {
		parts = append(parts, partition{"bios", "1MiB", biosBoot}, partition{"boot", "1GiB", "linux"})
	}
	if p.Disk.Swap == "partition" {
		parts = append(parts, partition{"swap", p.Disk.SwapSize + "iB", "swap"})
	}
	return append(parts, partition{"root", "", "linux"})
}

// sfdiskScript writes a GPT with parts in order.
func sfdiskScript(parts []partition) string {
	var b strings.Builder
	b.WriteString("label: gpt\n")
	for _, part := range parts {
		if part.size != "" {
			fmt.Fprintf(&b, "size=%s, ", part.size)
		}
		fmt.Fprintf(&b, "type=%s, name=%s\n", part.typ, part.use)
	}
	return b.String()
}

// subvolumes are the btrfs subvolumes and their mount points, in mount
// order (snapper-compatible).
var subvolumes = []struct{ name, path string }{
	{"@", "/"},
	{"@home", "/home"},
	{"@log", "/var/log"},
	{"@pkg", "/var/cache/pacman/pkg"},
	{"@snapshots", "/.snapshots"},
}

// mkfs returns the command that formats the filesystem for use.
func mkfs(use, filesystem, device string) []string {
	switch use {
	case "esp":
		return []string{"mkfs.fat", "-F", "32", "-n", "BOOT", device}
	case "boot":
		return []string{"mkfs.ext4", "-F", "-L", "boot", device}
	case "swap":
		return []string{"mkswap", "-L", "swap", device}
	}
	switch filesystem {
	case "btrfs":
		return []string{"mkfs.btrfs", "-f", "-L", "root", device}
	case "ext4":
		return []string{"mkfs.ext4", "-F", "-L", "root", device}
	}
	return []string{"mkfs.xfs", "-f", "-L", "root", device}
}

// hooks are the mkinitcpio hooks: Arch's default, with sd-encrypt for an
// encrypted root and without kms for NVIDIA, which must not load nouveau
// early. Empty means the default is fine.
func hooks(encrypt, nvidia bool) string {
	if !encrypt && !nvidia {
		return ""
	}
	h := []string{"base", "systemd", "autodetect", "microcode", "modconf", "kms", "keyboard", "sd-vconsole", "block", "filesystems", "fsck"}
	if nvidia {
		h = slices.DeleteFunc(h, func(s string) bool { return s == "kms" })
	}
	if encrypt {
		h = slices.Insert(h, slices.Index(h, "filesystems"), "sd-encrypt")
	}
	return "HOOKS=(" + strings.Join(h, " ") + ")\n"
}

// enableMultilib uncomments the [multilib] repository in pacman.conf.
func enableMultilib(conf string) (string, error) {
	const off = "#[multilib]\n#Include = /etc/pacman.d/mirrorlist\n"
	if strings.Contains(conf, "\n[multilib]\n") {
		return conf, nil
	}
	if !strings.Contains(conf, off) {
		return "", errors.New("no [multilib] section in pacman.conf")
	}
	return strings.Replace(conf, off, "[multilib]\nInclude = /etc/pacman.d/mirrorlist\n", 1), nil
}

// grubCmdline sets GRUB_CMDLINE_LINUX in /etc/default/grub to cmdline.
func grubCmdline(defaults, cmdline string) (string, error) {
	var out []string
	found := false
	for line := range strings.Lines(defaults) {
		if strings.HasPrefix(line, "GRUB_CMDLINE_LINUX=") {
			line = fmt.Sprintf("GRUB_CMDLINE_LINUX=%q\n", cmdline)
			found = true
		}
		out = append(out, line)
	}
	if !found {
		return "", fmt.Errorf("no GRUB_CMDLINE_LINUX line in /etc/default/grub")
	}
	return strings.Join(out, ""), nil
}
