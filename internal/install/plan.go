// Package install reads, checks and runs install plans.
package install

import (
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/grunclepug/arcus/internal/module"
)

// Plan is the content of plan.toml.
type Plan struct {
	Version int `toml:"version"`

	Disk struct {
		Device     string `toml:"device"`
		Filesystem string `toml:"filesystem"`
		Encrypt    bool   `toml:"encrypt"`
		Swap       string `toml:"swap"`
		SwapSize   string `toml:"swap_size"`
	} `toml:"disk"`

	System struct {
		Hostname string `toml:"hostname"`
		Timezone string `toml:"timezone"`
		Locale   string `toml:"locale"`
		Keymap   string `toml:"keymap"`
		Kernel   string `toml:"kernel"`
		Multilib bool   `toml:"multilib"`
	} `toml:"system"`

	User struct {
		Name         string `toml:"name"`
		PasswordHash string `toml:"password_hash"`
		Wheel        bool   `toml:"wheel"`
	} `toml:"user"`

	Host struct {
		Profiles []string `toml:"profiles"`
		Modules  []string `toml:"modules"`
	} `toml:"host"`

	Dotfiles struct {
		URL   string `toml:"url"`
		Path  string `toml:"path"`
		Arcus string `toml:"arcus"`
	} `toml:"dotfiles"`
}

// Version is the plan file format.
const Version = 1

var (
	filesystems = []string{"btrfs", "ext4", "xfs"}
	swaps       = []string{"zram", "partition", "none"}
	kernels     = []string{"linux", "linux-lts", "linux-zen"}

	deviceRegex   = regexp.MustCompile(`^/dev/[a-zA-Z0-9/_-]+$`)
	sizeRegex     = regexp.MustCompile(`^[1-9][0-9]*[MG]$`)
	hostnameRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	timezoneRegex = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)
	localeRegex   = regexp.MustCompile(`^[a-zA-Z_]+(\.[A-Za-z0-9-]+)?(@[a-z]+)?$`)
	keymapRegex   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	userRegex     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hashRegex     = regexp.MustCompile(`^\$(6|y)\$[./A-Za-z0-9$]+$`)
	urlRegex      = regexp.MustCompile(`^(https://|git@|file:///)[^\s]+$`)
)

// Load reads and validates a plan. Unknown keys are an error.
func Load(file string) (*Plan, error) {
	p := &Plan{}
	md, err := toml.DecodeFile(file, p)
	if err != nil {
		return nil, fmt.Errorf("loading plan: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", file, strings.Join(keys, ", "))
	}
	if p.Dotfiles.Path == "" {
		p.Dotfiles.Path = "~/dotfiles"
	}
	if p.Dotfiles.Arcus == "" {
		p.Dotfiles.Arcus = "config/arcus"
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return p, nil
}

// Encode writes the plan as TOML, as Load reads it.
func (p *Plan) Encode(w io.Writer) error {
	return toml.NewEncoder(w).Encode(p)
}

// Validate checks the plan on its own; Check compares it with the machine.
func (p *Plan) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	oneOf := func(field, value string, allowed []string) {
		if !slices.Contains(allowed, value) {
			fail("%s must be one of %s, got %q", field, strings.Join(allowed, ", "), value)
		}
	}

	if p.Version != Version {
		fail("version must be %d, got %d", Version, p.Version)
	}
	if !deviceRegex.MatchString(p.Disk.Device) {
		fail("disk.device must be a path below /dev, got %q", p.Disk.Device)
	}
	oneOf("disk.filesystem", p.Disk.Filesystem, filesystems)
	oneOf("disk.swap", p.Disk.Swap, swaps)
	switch {
	case p.Disk.Swap == "partition" && !sizeRegex.MatchString(p.Disk.SwapSize):
		fail("disk.swap_size must be a size such as 16G for a swap partition, got %q", p.Disk.SwapSize)
	case p.Disk.Swap != "partition" && p.Disk.SwapSize != "":
		fail("disk.swap_size is only for swap = \"partition\"")
	case p.Disk.Swap == "partition" && p.Disk.Encrypt:
		fail("an encrypted swap partition is not supported yet; use swap = \"zram\"")
	}

	if !hostnameRegex.MatchString(p.System.Hostname) {
		fail("system.hostname %q is not a valid host name (lowercase letters, digits and '-')", p.System.Hostname)
	}
	if !timezoneRegex.MatchString(p.System.Timezone) {
		fail("system.timezone %q is not a time zone name such as Europe/Berlin", p.System.Timezone)
	}
	if !localeRegex.MatchString(p.System.Locale) {
		fail("system.locale %q is not a locale such as en_US.UTF-8", p.System.Locale)
	}
	if !keymapRegex.MatchString(p.System.Keymap) {
		fail("system.keymap %q is not a keymap name such as us", p.System.Keymap)
	}
	oneOf("system.kernel", p.System.Kernel, kernels)

	if !userRegex.MatchString(p.User.Name) || p.User.Name == "root" {
		fail("user.name %q is not a valid login name", p.User.Name)
	}
	if p.User.PasswordHash != "" && !hashRegex.MatchString(p.User.PasswordHash) {
		fail("user.password_hash must be a crypt hash starting with $6$ or $y$")
	}

	for _, list := range [][]string{p.Host.Profiles, p.Host.Modules} {
		for i, name := range list {
			if err := module.ValidateName(name); err != nil {
				fail("host: %v", err)
			} else if slices.Contains(list[:i], name) {
				fail("host: %q is listed twice", name)
			}
		}
	}

	if p.Dotfiles.URL == "" {
		if len(p.Host.Profiles) > 0 {
			fail("host.profiles needs a dotfiles repository: the installer modules have no profiles")
		}
	} else {
		if !urlRegex.MatchString(p.Dotfiles.URL) {
			fail("dotfiles.url must start with https://, git@ or file:///, got %q", p.Dotfiles.URL)
		}
		if rest, ok := strings.CutPrefix(p.Dotfiles.Path, "~/"); !ok || rest == "" || path.Clean(rest) != rest || strings.HasPrefix(rest, "..") {
			fail("dotfiles.path must be a directory below ~/, got %q", p.Dotfiles.Path)
		}
		if a := p.Dotfiles.Arcus; path.IsAbs(a) || path.Clean(a) != a || strings.HasPrefix(a, "..") {
			fail("dotfiles.arcus must be a relative path inside the repository, got %q", a)
		}
	}
	return errors.Join(errs...)
}
