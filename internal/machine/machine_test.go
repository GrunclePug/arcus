package machine

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPackages(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    Facts
		want []string
	}{
		{"uefi btrfs zram amd", Facts{UEFI: true, Kernels: []string{"linux"}, Firmware: true, GRUB: true, Filesystems: []string{"btrfs", "vfat"}, CPU: "AuthenticAMD", Zram: true},
			[]string{"linux", "linux-firmware", "amd-ucode", "grub", "efibootmgr", "btrfs-progs", "dosfstools", "zram-generator"}},
		{"bios xfs intel two kernels", Facts{Kernels: []string{"linux-lts", "linux"}, Firmware: true, GRUB: true, Filesystems: []string{"ext4", "xfs"}, CPU: "GenuineIntel"},
			[]string{"linux", "linux-lts", "linux-firmware", "intel-ucode", "grub", "e2fsprogs", "xfsprogs"}},
		{"systemd-boot, no grub", Facts{UEFI: true, Kernels: []string{"linux"}, Filesystems: []string{"ext4", "vfat"}},
			[]string{"linux", "dosfstools", "e2fsprogs"}},
	} {
		if got := tc.f.Packages(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Detect on a running system finds what the installer would have written.
func TestDetect(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"proc/cpuinfo":     "vendor_id\t: AuthenticAMD\n",
		"proc/self/mounts": "/dev/mapper/root / btrfs rw,noatime,subvol=/@ 0 0\n/dev/mapper/root /home btrfs rw,noatime,subvol=/@home 0 0\n/dev/vda1 /boot vfat rw 0 0\nproc /proc proc rw 0 0\ntmpfs /tmp tmpfs rw 0 0\n",
	} {
		file := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sys/firmware/efi"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed := map[string]bool{"linux": true, "linux-firmware": true, "grub": true, "zram-generator": true, "xfsprogs": true}
	got := Detect(root, installed)
	want := Facts{UEFI: true, Kernels: []string{"linux"}, Firmware: true, GRUB: true, Filesystems: []string{"btrfs", "vfat"}, CPU: "AuthenticAMD", Zram: true}
	if got.Hash() != want.Hash() {
		t.Errorf("detected %+v, want %+v", got, want)
	}

	dir := filepath.Join(t.TempDir(), "machine")
	if err := got.Write(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "module.toml"))
	if err != nil || !strings.Contains(string(data), `"btrfs-progs", "dosfstools", "zram-generator"`) {
		t.Errorf("module.toml: %s (%v)", data, err)
	}
	got.Zram = false
	if err := got.Write(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "system")); !os.IsNotExist(err) {
		t.Error("rewriting without zram must drop its files")
	}
}
