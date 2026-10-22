package install

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
)

// Machine is the live system a plan is checked against.
type Machine struct {
	Root string // root prefix for /sys and /usr/share, "/" outside tests
	// Online reports whether the Arch repositories can be reached.
	Online func(ctx context.Context) error
}

// Disk describes the target device.
type Disk struct {
	Path  string
	Model string
	Size  int64
}

// Result is what Check found. The plan can run only without problems.
type Result struct {
	UEFI     bool
	Disk     Disk
	Desired  []string // the modules the first apply turns on, in stack order
	Problems []error
	Warnings []string
}

// MinDisk is the smallest disk the installer accepts.
const MinDisk = 20 << 30

// Check compares a valid plan with the machine it would run on. It changes
// nothing; with a dotfiles repository it clones it into a temporary
// directory.
func Check(ctx context.Context, p *Plan, m Machine) Result {
	var r Result
	fail := func(format string, args ...any) { r.Problems = append(r.Problems, fmt.Errorf(format, args...)) }

	_, err := os.Stat(filepath.Join(m.Root, "sys/firmware/efi"))
	r.UEFI = err == nil

	if disk, err := inspectDisk(p.Disk.Device, liveMedium(filepath.Join(m.Root, "proc/cmdline"))); err != nil {
		r.Problems = append(r.Problems, err)
	} else {
		r.Disk = disk
		need := int64(MinDisk)
		if p.Disk.Swap == "partition" {
			need += sizeBytes(p.Disk.SwapSize)
		}
		if disk.Size < need {
			fail("%s has %d GiB; the plan needs at least %d GiB", disk.Path, disk.Size>>30, need>>30)
		}
	}
	if err := m.Online(ctx); err != nil {
		fail("no network: %v", err)
	}

	if _, err := os.Stat(filepath.Join(m.Root, "usr/share/zoneinfo", p.System.Timezone)); err != nil {
		fail("time zone %q does not exist", p.System.Timezone)
	}
	if !supportedLocale(filepath.Join(m.Root, "usr/share/i18n/SUPPORTED"), p.System.Locale) {
		fail("locale %q is not supported", p.System.Locale)
	}
	if !keymapExists(filepath.Join(m.Root, "usr/share/kbd/keymaps"), p.System.Keymap) {
		fail("keymap %q does not exist", p.System.Keymap)
	}

	if _, err := exec.LookPath("git"); p.Dotfiles.URL != "" && err != nil {
		r.Warnings = append(r.Warnings, "git is not installed here, so the dotfiles selection is not checked; run installs git first and checks it")
		return r
	}
	desired, err := selection(ctx, p, MachineFacts(p, r.UEFI, m.Root))
	if err != nil {
		r.Problems = append(r.Problems, err)
		return r
	}
	r.Desired = desired
	if p.Dotfiles.URL == "" {
		consistent(p, desired, fail)
		for _, m := range Detect(m.Root).Modules() {
			if !slices.Contains(desired, m) {
				r.Warnings = append(r.Warnings, "this machine has hardware for "+m+", which the plan doesn't select")
			}
		}
	}
	return r
}

// selection resolves the plan's host selection against the modules the new
// system will have: the installer modules, or the dotfiles repository's,
// and the machine module.
func selection(ctx context.Context, p *Plan, f machine.Facts) ([]string, error) {
	modules, profiles, err := Modules(ctx, p, f)
	if err != nil {
		return nil, err
	}
	hostProfiles, hostModules, err := Host(p, profiles)
	if err != nil {
		return nil, err
	}
	selected, _, err := profiles.Expand(hostProfiles, hostModules)
	if err != nil {
		return nil, err
	}
	return modules.Resolve(selected)
}

// Modules loads what a plan can select: the installer modules, or the
// dotfiles repository's modules and profiles (cloned into a temporary
// directory), and the machine module for f.
func Modules(ctx context.Context, p *Plan, f machine.Facts) (module.Set, profile.Set, error) {
	tmp, err := os.MkdirTemp("", "arcus-modules-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	local := filepath.Join(tmp, "machine")
	if err := f.Write(filepath.Join(local, machine.Name)); err != nil {
		return nil, nil, err
	}

	if p.Dotfiles.URL == "" {
		dir := filepath.Join(tmp, "modules")
		if err := WriteModules(dir); err != nil {
			return nil, nil, err
		}
		modules, err := module.Scan(dir, local)
		return modules, profile.Set{}, err
	}
	repo := filepath.Join(tmp, "dotfiles")
	cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--", p.Dotfiles.URL, repo)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("cloning %s: %w: %s", p.Dotfiles.URL, err, strings.TrimSpace(string(out)))
	}
	dir := filepath.Join(repo, p.Dotfiles.Arcus)
	modules, err := module.Scan(filepath.Join(dir, "modules"), local)
	if err != nil {
		return nil, nil, err
	}
	profiles, err := profile.Scan(filepath.Join(dir, "profiles"))
	return modules, profiles, err
}

// consistent reports installer modules that contradict the plan.
func consistent(p *Plan, desired []string, fail func(string, ...any)) {
	on := func(name string) bool { return slices.Contains(desired, name) }
	if on("btrfs-snapshots") && p.Disk.Filesystem != "btrfs" {
		fail("btrfs-snapshots needs filesystem = \"btrfs\"")
	}
	if on("gpu-nvidia") && p.System.Kernel != "linux" {
		fail("gpu-nvidia installs nvidia-open, which is built for kernel = \"linux\"")
	}
}

// liveMedium returns how archiso finds its boot medium, from the kernel
// command line: "UUID=<uuid>" or "LABEL=<label>", or "" off the ISO.
func liveMedium(cmdline string) string {
	data, _ := os.ReadFile(cmdline)
	for _, field := range strings.Fields(string(data)) {
		if uuid, ok := strings.CutPrefix(field, "archisosearchuuid="); ok {
			return "UUID=" + uuid
		}
		if label, ok := strings.CutPrefix(field, "archisolabel="); ok {
			return "LABEL=" + label
		}
	}
	return ""
}

// inspectDisk reads the device with lsblk and refuses anything but a whole
// disk that nothing uses: a mounted partition or active swap means a
// running system. The live medium is refused even when it isn't mounted
// (archiso copies itself to RAM when there is room): live is how archiso
// finds it, see liveMedium.
func inspectDisk(device, live string) (Disk, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("lsblk", "--json", "--bytes", "--output", "PATH,TYPE,SIZE,MODEL,MOUNTPOINTS,UUID,LABEL", device)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Disk{}, fmt.Errorf("%s: %s", device, strings.TrimSpace(stderr.String()))
	}
	var out lsblkOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out.Devices) == 0 {
		return Disk{}, fmt.Errorf("%s: unexpected lsblk output", device)
	}

	d := out.Devices[0]
	if d.Type != "disk" {
		return Disk{}, fmt.Errorf("%s is a %s, not a whole disk", device, d.Type)
	}
	disk := Disk{Path: d.Path, Size: d.Size}
	if d.Model != nil {
		disk.Model = strings.TrimSpace(*d.Model)
	}
	// lsblk lists the partitions flat after the disk for these columns.
	return disk, inUse(device, out.Devices, live)
}

// inUse reports whether device, given as its lsblk nodes, is the live
// medium or has something mounted or swapped on.
func inUse(device string, nodes []blockDevice, live string) error {
	var used []string
	isLive := false
	var walk func(nodes []blockDevice)
	walk = func(nodes []blockDevice) {
		for _, n := range nodes {
			isLive = isLive || live != "" && (live == "UUID="+n.UUID || live == "LABEL="+n.Label)
			for _, mp := range n.Mountpoints {
				if mp != nil && *mp != "" {
					used = append(used, n.Path+" ("+*mp+")")
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	switch {
	case isLive:
		return fmt.Errorf("%s is the live medium this system booted from", device)
	case len(used) > 0:
		return fmt.Errorf("%s is in use: %s", device, strings.Join(used, ", "))
	}
	return nil
}

// DiskChoice is a disk the installer can offer; Problem says why it can't
// be used, if it can't.
type DiskChoice struct {
	Disk
	Problem string
}

// Disks lists the whole disks of the machine for the TUI.
func Disks(m Machine) ([]DiskChoice, error) {
	data, err := exec.Command("lsblk", "--json", "--bytes", "--tree", "--output", "PATH,TYPE,SIZE,MODEL,MOUNTPOINTS,UUID,LABEL").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var out lsblkOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	live := liveMedium(filepath.Join(m.Root, "proc/cmdline"))
	var disks []DiskChoice
	for _, d := range out.Devices {
		if d.Type != "disk" {
			continue
		}
		c := DiskChoice{Disk: Disk{Path: d.Path, Size: d.Size}}
		if d.Model != nil {
			c.Model = strings.TrimSpace(*d.Model)
		}
		if err := inUse(d.Path, []blockDevice{d}, live); err != nil {
			c.Problem = err.Error()
		} else if d.Size < MinDisk {
			c.Problem = fmt.Sprintf("%d GiB is too small", d.Size>>30)
		}
		disks = append(disks, c)
	}
	return disks, nil
}

// blockDevice is one device in lsblk's JSON output.
type blockDevice struct {
	Path        string        `json:"path"`
	Type        string        `json:"type"`
	Size        int64         `json:"size"`
	Model       *string       `json:"model"`
	Mountpoints []*string     `json:"mountpoints"`
	UUID        string        `json:"uuid"`
	Label       string        `json:"label"`
	Children    []blockDevice `json:"children"`
}

type lsblkOutput struct {
	Devices []blockDevice `json:"blockdevices"`
}

// partitions lists the partitions of device in order. lsblk prints them
// nested or flat depending on the columns, so both are read.
func partitions(device string) ([]string, error) {
	data, err := exec.Command("lsblk", "--json", "--output", "PATH,TYPE", device).Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk %s: %w", device, err)
	}
	var out lsblkOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("lsblk %s: %w", device, err)
	}
	var parts []string
	var walk func(nodes []blockDevice)
	walk = func(nodes []blockDevice) {
		for _, n := range nodes {
			if n.Type == "part" {
				parts = append(parts, n.Path)
			}
			walk(n.Children)
		}
	}
	walk(out.Devices)
	return parts, nil
}

// sizeBytes converts a validated size such as 16G or 512M.
func sizeBytes(size string) int64 {
	n, _ := strconv.ParseInt(size[:len(size)-1], 10, 64)
	if strings.HasSuffix(size, "G") {
		return n << 30
	}
	return n << 20
}

func supportedLocale(file, locale string) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if name, _, _ := strings.Cut(s.Text(), " "); name == locale {
			return true
		}
	}
	return false
}

func keymapExists(dir, keymap string) bool {
	found := errors.New("found")
	err := filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name := d.Name(); name == keymap+".map.gz" || name == keymap+".map" {
			return found
		}
		return nil
	})
	return errors.Is(err, found)
}
