package install

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/module"
)

const validPlan = `version = 1

[disk]
device     = "/dev/vda"
filesystem = "btrfs"
swap       = "zram"

[system]
hostname = "box"
timezone = "Europe/Berlin"
locale   = "en_US.UTF-8"
keymap   = "us"
kernel   = "linux"

[user]
name  = "me"
wheel = true

[host]
modules = ["base", "btrfs-snapshots"]
`

func writePlan(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	if _, err := Load(writePlan(t, validPlan)); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{"unknown key", "[user]", "[user]\nshell = \"zsh\"", "unknown keys: user.shell"},
		{"version", "version = 1", "version = 2", "version must be 1"},
		{"device", `"/dev/vda"`, `"vda"`, "disk.device"},
		{"filesystem", `"btrfs"`, `"zfs"`, "disk.filesystem must be one of"},
		{"swap size missing", `swap       = "zram"`, `swap = "partition"`, "disk.swap_size must be a size"},
		{"swap size unused", `swap       = "zram"`, "swap = \"zram\"\nswap_size = \"8G\"", "only for swap"},
		{"hostname", `"box"`, `"Box_1"`, "system.hostname"},
		{"kernel", `kernel   = "linux"`, `kernel = "linux-hardened"`, "system.kernel"},
		{"root user", `name  = "me"`, `name = "root"`, "user.name"},
		{"password hash", "wheel = true", "wheel = true\npassword_hash = \"secret\"", "crypt hash"},
		{"module twice", `"btrfs-snapshots"]`, `"btrfs-snapshots", "base"]`, "listed twice"},
		{"profiles need dotfiles", "[host]", "[host]\nprofiles = [\"laptop\"]", "needs a dotfiles repository"},
		{"dotfiles url", "[host]", "[dotfiles]\nurl = \"http://example.com/dot\"\n\n[host]", "dotfiles.url"},
		{"dotfiles path", "[host]", "[dotfiles]\nurl = \"https://example.com/dot\"\npath = \"/srv/dot\"\n\n[host]", "dotfiles.path"},
		{"encrypted swap partition", `swap       = "zram"`, "swap = \"partition\"\nswap_size = \"4G\"\nencrypt = true", "encrypted swap partition"},
		{"dotfiles arcus", "[host]", "[dotfiles]\nurl = \"https://example.com/dot\"\narcus = \"../x\"\n\n[host]", "dotfiles.arcus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := strings.Replace(validPlan, tc.from, tc.to, 1)
			if content == validPlan {
				t.Fatalf("%q not in the plan", tc.from)
			}
			if _, err := Load(writePlan(t, content)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

// Every installer module loads, and the machine module follows the plan,
// the firmware and the CPU.
func TestModulesAndMachineFacts(t *testing.T) {
	p, err := Load(writePlan(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := WriteModules(dir); err != nil {
		t.Fatal(err)
	}
	if set, err := module.Scan(dir); err != nil || len(set) != 7 {
		t.Fatalf("%d installer modules (%v)", len(set), err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cpuinfo"), []byte("vendor_id\t: AuthenticAMD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fs, swap string
		uefi     bool
		want     []string
	}{
		{"btrfs", "zram", true, []string{"linux", "linux-firmware", "amd-ucode", "grub", "efibootmgr", "btrfs-progs", "dosfstools", "zram-generator"}},
		{"ext4", "none", true, []string{"linux", "linux-firmware", "amd-ucode", "grub", "efibootmgr", "dosfstools", "e2fsprogs"}},
		{"xfs", "none", false, []string{"linux", "linux-firmware", "amd-ucode", "grub", "e2fsprogs", "xfsprogs"}},
	} {
		p.Disk.Filesystem, p.Disk.Swap = tc.fs, tc.swap
		if got := MachineFacts(p, tc.uefi, root).Packages(); !slices.Equal(got, tc.want) {
			t.Errorf("%s, uefi=%v: %v, want %v", tc.fs, tc.uefi, got, tc.want)
		}
	}
}

// live fakes the live system: lsblk on PATH, a root with firmware,
// zoneinfo, locales and keymaps.
func live(t *testing.T, lsblk string) Machine {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		"usr/share/zoneinfo/Europe/Berlin":            "",
		"usr/share/i18n/SUPPORTED":                    "en_US.UTF-8 UTF-8\nde_DE.UTF-8 UTF-8\n",
		"usr/share/kbd/keymaps/i386/qwerty/us.map.gz": "",
	} {
		file := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cmdline"), []byte("archisobasedir=arch archisosearchuuid=2026-10-01-14-49-03-00 script=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys/firmware/efi"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' '" + lsblk + "'\n" // printf is built in: no PATH needed
	if err := os.WriteFile(filepath.Join(bin, "lsblk"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return Machine{Root: root, Online: func(context.Context) error { return nil }}
}

const freeDisk = `{"blockdevices": [{"path": "/dev/vda", "type": "disk", "size": 42949672960, "model": "QEMU HARDDISK", "mountpoints": [null],
  "children": [{"path": "/dev/vda1", "type": "part", "size": 1073741824, "model": null, "mountpoints": [null]}]}]}`

func TestCheck(t *testing.T) {
	p, err := Load(writePlan(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	r := Check(context.Background(), p, live(t, freeDisk))
	if len(r.Problems) > 0 {
		t.Fatalf("problems: %v", r.Problems)
	}
	if !r.UEFI || r.Disk.Model != "QEMU HARDDISK" || r.Disk.Size>>30 != 40 {
		t.Errorf("result = %+v", r)
	}
	if want := []string{"machine", "base", "btrfs-snapshots"}; !slices.Equal(r.Desired, want) {
		t.Errorf("desired = %v, want %v", r.Desired, want)
	}

	for _, tc := range []struct {
		name  string
		lsblk string
		edit  func(*Plan)
		want  string
	}{
		{"mounted partition", strings.Replace(freeDisk, `"mountpoints": [null]}]`, `"mountpoints": ["/run/archiso/bootmnt"]}]`, 1), nil, "in use: /dev/vda1 (/run/archiso/bootmnt)"},
		{"swap in use", strings.Replace(freeDisk, `"mountpoints": [null]}]`, `"mountpoints": ["[SWAP]"]}]`, 1), nil, "in use"},
		{"live medium in RAM", strings.Replace(freeDisk, `"mountpoints": [null]}]`, `"mountpoints": [null], "uuid": "2026-10-01-14-49-03-00"}]`, 1), nil, "the live medium"},
		{"partition", strings.Replace(freeDisk, `"type": "disk"`, `"type": "part"`, 1), nil, "not a whole disk"},
		{"small disk", strings.Replace(freeDisk, "42949672960", "10737418240", 1), nil, "needs at least 20 GiB"},
		{"time zone", freeDisk, func(p *Plan) { p.System.Timezone = "Mars/Olympus" }, "time zone"},
		{"locale", freeDisk, func(p *Plan) { p.System.Locale = "xx_XX.UTF-8" }, "locale"},
		{"keymap", freeDisk, func(p *Plan) { p.System.Keymap = "dvorak" }, "keymap"},
		{"unknown module", freeDisk, func(p *Plan) { p.Host.Modules = append(p.Host.Modules, "nope") }, "nope"},
		{"snapshots need btrfs", freeDisk, func(p *Plan) { p.Disk.Filesystem = "xfs" }, "btrfs-snapshots needs"},
		{"nvidia needs linux", freeDisk, func(p *Plan) {
			p.System.Kernel = "linux-lts"
			p.Host.Modules = append(p.Host.Modules, "gpu-nvidia")
		}, "gpu-nvidia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := Load(writePlan(t, validPlan))
			if tc.edit != nil {
				tc.edit(p)
			}
			r := Check(context.Background(), p, live(t, tc.lsblk))
			if err := errors.Join(r.Problems...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("problems = %v, want %q", r.Problems, tc.want)
			}
		})
	}

	m := live(t, freeDisk)
	m.Online = func(context.Context) error { return errors.New("unreachable") }
	if r := Check(context.Background(), p, m); len(r.Problems) != 1 || !strings.Contains(r.Problems[0].Error(), "no network") {
		t.Errorf("offline: %v", r.Problems)
	}
}

// With a dotfiles repository, its modules and profiles are what resolves,
// and the installer's packages that none of them lists are a warning.
func TestCheckDotfiles(t *testing.T) {
	repo := t.TempDir()
	for path, content := range map[string]string{
		"config/arcus/modules/shell/module.toml":   "",
		"config/arcus/modules/boot/module.toml":    "[packages]\npacman = [\"grub\", \"efibootmgr\"]\n",
		"config/arcus/profiles/laptop.toml":        "modules = [\"boot\", \"shell\"]\n",
		"config/arcus/modules/amd-cpu/module.toml": "[packages]\npacman = [\"amd-ucode\"]\n",
	} {
		file := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "dot"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	plan := strings.Replace(validPlan, "[host]\nmodules = [\"base\", \"btrfs-snapshots\"]",
		"[host]\nprofiles = [\"laptop\"]\nmodules = [\"amd-cpu\"]\n\n[dotfiles]\nurl = \"file://"+repo+"\"", 1)
	if plan == validPlan {
		t.Fatal("the plan was not changed")
	}
	p, err := Load(writePlan(t, plan))
	if err != nil {
		t.Fatal(err)
	}
	r := Check(context.Background(), p, live(t, freeDisk))
	if len(r.Problems) > 0 {
		t.Fatalf("problems: %v", r.Problems)
	}
	if want := []string{"boot", "shell", "machine", "amd-cpu"}; !slices.Equal(r.Desired, want) {
		t.Errorf("desired = %v, want %v", r.Desired, want)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v", r.Warnings)
	}
}

func TestLayout(t *testing.T) {
	p, err := Load(writePlan(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sfdiskScript(layout(p, true)), "label: gpt\nsize=1GiB, type=uefi, name=esp\ntype=linux, name=root\n"; got != want {
		t.Errorf("UEFI:\n%s\nwant\n%s", got, want)
	}
	p.Disk.Swap, p.Disk.SwapSize = "partition", "4G"
	want := "label: gpt\nsize=1MiB, type=" + biosBoot + ", name=bios\nsize=1GiB, type=linux, name=boot\nsize=4GiB, type=swap, name=swap\ntype=linux, name=root\n"
	if got := sfdiskScript(layout(p, false)); got != want {
		t.Errorf("BIOS with swap:\n%s\nwant\n%s", got, want)
	}
}

func TestHooks(t *testing.T) {
	for _, tc := range []struct {
		encrypt, nvidia bool
		want            string
	}{
		{false, false, ""},
		{true, false, "HOOKS=(base systemd autodetect microcode modconf kms keyboard sd-vconsole block sd-encrypt filesystems fsck)\n"},
		{false, true, "HOOKS=(base systemd autodetect microcode modconf keyboard sd-vconsole block filesystems fsck)\n"},
		{true, true, "HOOKS=(base systemd autodetect microcode modconf keyboard sd-vconsole block sd-encrypt filesystems fsck)\n"},
	} {
		if got := hooks(tc.encrypt, tc.nvidia); got != tc.want {
			t.Errorf("encrypt=%v nvidia=%v: %q, want %q", tc.encrypt, tc.nvidia, got, tc.want)
		}
	}
}

func TestEnableMultilib(t *testing.T) {
	conf := "[extra]\nInclude = /etc/pacman.d/mirrorlist\n\n#[multilib]\n#Include = /etc/pacman.d/mirrorlist\n"
	got, err := enableMultilib(conf)
	if want := "[extra]\nInclude = /etc/pacman.d/mirrorlist\n\n[multilib]\nInclude = /etc/pacman.d/mirrorlist\n"; err != nil || got != want {
		t.Fatalf("got %q (%v)", got, err)
	}
	if again, err := enableMultilib(got); err != nil || again != got {
		t.Errorf("enabling twice: %q (%v)", again, err)
	}
	if _, err := enableMultilib("[extra]\n"); err == nil {
		t.Error("a pacman.conf without multilib must be an error")
	}
}

func TestGrubCmdline(t *testing.T) {
	defaults := "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"loglevel=3\"\nGRUB_CMDLINE_LINUX=\"\"\n"
	got, err := grubCmdline(defaults, "rd.luks.name=u=root root=/dev/mapper/root")
	if want := "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"loglevel=3\"\nGRUB_CMDLINE_LINUX=\"rd.luks.name=u=root root=/dev/mapper/root\"\n"; err != nil || got != want {
		t.Errorf("got %q (%v), want %q", got, err, want)
	}
	if _, err := grubCmdline("GRUB_DEFAULT=0\n", "x"); err == nil {
		t.Error("a file without GRUB_CMDLINE_LINUX must be an error")
	}
}

func TestLocaleCharset(t *testing.T) {
	file := filepath.Join(t.TempDir(), "SUPPORTED")
	if err := os.WriteFile(file, []byte("de_DE.UTF-8 UTF-8\nen_US ISO-8859-1\nen_US.UTF-8 UTF-8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := localeCharset(file, "en_US"); err != nil || got != "ISO-8859-1" {
		t.Errorf("en_US: %q, %v", got, err)
	}
	if _, err := localeCharset(file, "fr_FR.UTF-8"); err == nil {
		t.Error("an unsupported locale must be an error")
	}
}

// Without git, check can't clone the dotfiles repository: it warns instead
// of failing, since run installs git first.
func TestCheckDotfilesWithoutGit(t *testing.T) {
	plan := strings.Replace(validPlan, "[host]\nmodules = [\"base\", \"btrfs-snapshots\"]",
		"[host]\nprofiles = [\"laptop\"]\n\n[dotfiles]\nurl = \"https://example.com/dot\"", 1)
	if plan == validPlan {
		t.Fatal("the plan was not changed")
	}
	p, err := Load(writePlan(t, plan))
	if err != nil {
		t.Fatal(err)
	}
	m := live(t, freeDisk)
	t.Setenv("PATH", filepath.Dir(must(exec.LookPath("lsblk"))))
	r := Check(context.Background(), p, m)
	if len(r.Problems) > 0 || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "git is not installed") {
		t.Errorf("problems %v, warnings %v", r.Problems, r.Warnings)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"proc/cpuinfo":                    "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD Ryzen 7\n",
		"sys/bus/pci/devices/a/class":     "0x030000\n", // AMD display controller
		"sys/bus/pci/devices/a/vendor":    "0x1002\n",
		"sys/bus/pci/devices/b/class":     "0x038000\n", // Intel display controller (hybrid laptop)
		"sys/bus/pci/devices/b/vendor":    "0x8086\n",
		"sys/bus/pci/devices/c/class":     "0x020000\n", // Intel network card: not a GPU
		"sys/bus/pci/devices/c/vendor":    "0x8086\n",
		"sys/bus/pci/devices/d/class":     "0x030000\n", // QEMU's standard VGA: no module
		"sys/bus/pci/devices/d/vendor":    "0x1234\n",
		"sys/class/dmi/id/sys_vendor":     "QEMU\n",
		"sys/class/bluetooth/hci0/uevent": "",
	}
	for path, content := range files {
		file := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := Detect(root)
	if want := []string{"gpu-amd", "gpu-intel", "vm-qemu", "bluetooth"}; !slices.Equal(h.Modules(), want) {
		t.Errorf("modules = %v, want %v (%+v)", h.Modules(), want, h)
	}
	if got := Detect(t.TempDir()).Modules(); len(got) != 0 {
		t.Errorf("nothing to detect: %v", got)
	}

	// check warns about detected hardware the plan leaves out.
	m := live(t, freeDisk)
	if err := os.MkdirAll(filepath.Join(m.Root, "sys/class/dmi/id"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root, "sys/class/dmi/id/sys_vendor"), []byte("QEMU\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(writePlan(t, validPlan))
	if err != nil {
		t.Fatal(err)
	}
	if r := Check(context.Background(), p, m); len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "vm-qemu") {
		t.Errorf("warnings = %v", r.Warnings)
	}
}
