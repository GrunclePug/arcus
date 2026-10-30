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
		{"amd and intel graphics", Facts{Kernels: []string{"linux"}, GPUs: []string{"0x8086", "0x1002", "0x1234"}},
			[]string{"linux", "mesa", "vulkan-radeon", "vulkan-intel", "intel-media-driver"}},
		{"nvidia, a prebuilt and a dkms kernel", Facts{Kernels: []string{"linux-zen", "linux"}, CPU: "AuthenticAMD", GPUs: []string{NVIDIA}},
			[]string{"linux", "linux-zen", "amd-ucode", "nvidia-open", "nvidia-open-dkms", "linux-zen-headers", "nvidia-utils"}},
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
		"proc/cpuinfo":                            "vendor_id\t: AuthenticAMD\n",
		"sys/bus/pci/devices/0000:0e:00.0/class":  "0x030000\n",
		"sys/bus/pci/devices/0000:0e:00.0/vendor": "0x10de\n",
		"proc/self/mounts":                        "/dev/mapper/root / btrfs rw,noatime,subvol=/@ 0 0\n/dev/mapper/root /home btrfs rw,noatime,subvol=/@home 0 0\n/dev/vda1 /boot vfat rw 0 0\nproc /proc proc rw 0 0\ntmpfs /tmp tmpfs rw 0 0\n",
		"sys/block/nvme0n1/device/model":          "SSD\n",
		"sys/block/nvme0n1/removable":             "0\n",
		"sys/block/nvme0n1/queue/rotational":      "0\n",
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
	want := Facts{UEFI: true, Kernels: []string{"linux"}, Firmware: true, GRUB: true, Filesystems: []string{"btrfs", "vfat"}, CPU: "AuthenticAMD", GPUs: []string{NVIDIA}, Zram: true, SSD: true}
	if got.Hash() != want.Hash() {
		t.Errorf("detected %+v, want %+v", got, want)
	}

	dir := filepath.Join(t.TempDir(), "machine")
	if err := got.Write(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "module.toml"))
	if err != nil || !strings.Contains(string(data), `"btrfs-progs", "dosfstools", "zram-generator"`) || !strings.Contains(string(data), `system = ["fstrim.timer"]`) {
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

// Only a physical, fixed, non-rotational disk counts as an SSD.
func TestSSD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"no disks", nil, false},
		{"a hard disk", map[string]string{"sda/device/model": "", "sda/removable": "0", "sda/queue/rotational": "1"}, false},
		{"a hard disk and an SSD", map[string]string{"sda/device/model": "", "sda/removable": "0", "sda/queue/rotational": "1", "nvme0n1/device/model": "", "nvme0n1/removable": "0", "nvme0n1/queue/rotational": "0"}, true},
		{"a removable stick", map[string]string{"sdb/device/model": "", "sdb/removable": "1", "sdb/queue/rotational": "0"}, false},
		{"loop, zram and device-mapper", map[string]string{"loop0/removable": "0", "loop0/queue/rotational": "0", "zram0/removable": "0", "zram0/queue/rotational": "0", "dm-0/removable": "0", "dm-0/queue/rotational": "0"}, false},
	} {
		root := t.TempDir()
		for path, content := range tc.files {
			file := filepath.Join(root, "sys/block", path)
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(content+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := SSD(root); got != tc.want {
			t.Errorf("%s: SSD = %v, want %v", tc.name, got, tc.want)
		}
	}
}
