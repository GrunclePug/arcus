package install

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Hardware is what Detect found. The CPU is the machine module's
// (microcode), not a module of its own.
type Hardware struct {
	GPUs      []string // PCI vendor IDs of the display controllers
	VM        string   // the DMI system vendor when it is a known hypervisor
	Bluetooth bool
}

var (
	gpuModules = map[string]string{"0x1002": "gpu-amd", "0x8086": "gpu-intel", "0x10de": "gpu-nvidia"}
	vmModules  = map[string]string{"QEMU": "vm-qemu"}
)

// Detect reads the hardware from /proc and /sys below root.
func Detect(root string) Hardware {
	var h Hardware
	devices, _ := filepath.Glob(filepath.Join(root, "sys/bus/pci/devices/*"))
	for _, dev := range devices {
		class, _ := os.ReadFile(filepath.Join(dev, "class"))
		vendor, _ := os.ReadFile(filepath.Join(dev, "vendor"))
		if v := strings.TrimSpace(string(vendor)); strings.HasPrefix(string(class), "0x03") && !slices.Contains(h.GPUs, v) {
			h.GPUs = append(h.GPUs, v)
		}
	}

	if vendor, err := os.ReadFile(filepath.Join(root, "sys/class/dmi/id/sys_vendor")); err == nil {
		if v := strings.TrimSpace(string(vendor)); vmModules[v] != "" {
			h.VM = v
		}
	}
	controllers, _ := os.ReadDir(filepath.Join(root, "sys/class/bluetooth"))
	h.Bluetooth = len(controllers) > 0
	return h
}

// Modules returns the installer modules the hardware needs.
func (h Hardware) Modules() []string {
	var mods []string
	for _, v := range h.GPUs {
		if m := gpuModules[v]; m != "" && !slices.Contains(mods, m) {
			mods = append(mods, m)
		}
	}
	if m := vmModules[h.VM]; m != "" {
		mods = append(mods, m)
	}
	if h.Bluetooth {
		mods = append(mods, "bluetooth")
	}
	return mods
}
