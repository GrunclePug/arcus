package install

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/grunclepug/arcus/internal/machine"
)

// Hardware is what Detect found. The CPU (microcode) and the GPUs (drivers)
// are the machine module's, not modules of their own.
type Hardware struct {
	GPUs      []string // PCI vendor IDs of the display controllers
	VM        string   // the DMI system vendor when it is a known hypervisor
	Bluetooth bool
}

var vmModules = map[string]string{"QEMU": "vm-qemu"}

// Detect reads the hardware from /proc and /sys below root.
func Detect(root string) Hardware {
	h := Hardware{GPUs: machine.GPUs(root)}

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
	if m := vmModules[h.VM]; m != "" {
		mods = append(mods, m)
	}
	if h.Bluetooth {
		mods = append(mods, "bluetooth")
	}
	return mods
}
