package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/config"
	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/profile"
	"github.com/grunclepug/arcus/internal/state"
)

// Options are run's settings besides the plan.
type Options struct {
	Yes    bool // skip the typed confirmation
	Stdin  io.Reader
	Stdout io.Writer
	Log    string // the install log, copied into the new system at the end
	Arcus  string // the arcus binary copied into the new system

	// Password and Passphrase, when set (the TUI asked for them), are used
	// instead of asking on Stdin. They are never written anywhere.
	Password, Passphrase string
}

// target is where the new system is mounted.
const target = "/mnt"

// installer carries one run.
type installer struct {
	ctx      context.Context
	p        *Plan
	o        Options
	uefi     bool
	facts    machine.Facts
	desired  []string
	out      io.Writer // the terminal and the log
	in       *bufio.Reader
	devices  map[string]string // partition use -> device
	root     string            // the root filesystem's device, /dev/mapper/root when encrypted
	password string            // the user's, when the plan has no hash
	luks     string
	arcusBin []byte // read before anything is mounted on /mnt
}

// Run installs the plan. It checks the plan first and changes nothing if
// that finds a problem or the user doesn't confirm. On a failure it stops and leaves everything mounted under /mnt
// for inspection.
func Run(ctx context.Context, p *Plan, m Machine, o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("run needs root (it partitions disks)")
	}
	if _, err := os.Stat(filepath.Join(m.Root, "run/archiso")); err != nil {
		return errors.New("run only works from the Arch ISO")
	}
	if p.Dotfiles.URL != "" {
		if err := EnsureGit(ctx, o.Stdout); err != nil {
			return err
		}
	}
	r := Check(ctx, p, m)
	if len(r.Problems) > 0 {
		return fmt.Errorf("the plan can't run: %w", errors.Join(r.Problems...))
	}
	// Read now: the binaries may be on a stick mounted under /mnt, which the
	// new system's mount hides.
	arcusBin, err := os.ReadFile(o.Arcus)
	if err != nil {
		return fmt.Errorf("the arcus binary next to arcus-install: %w", err)
	}
	log, err := os.Create(o.Log)
	if err != nil {
		return err
	}
	defer log.Close()

	x := &installer{ctx: ctx, p: p, o: o, uefi: r.UEFI, facts: MachineFacts(p, r.UEFI, m.Root), desired: r.Desired,
		out: io.MultiWriter(o.Stdout, log), in: bufio.NewReader(o.Stdin), devices: map[string]string{}, arcusBin: arcusBin}
	// The plan holds no secrets; the log starts with it.
	fmt.Fprintln(log, "== plan")
	if err := p.Encode(log); err != nil {
		return err
	}
	for _, w := range r.Warnings {
		fmt.Fprintln(x.out, "warning:", w)
	}
	if err := x.secrets(); err != nil {
		return err
	}
	if !o.Yes {
		fmt.Fprintf(o.Stdout, "Everything on %s (%s) will be erased. Type the device name to continue: ", r.Disk.Path, r.Disk.Model)
		if answer, _ := x.in.ReadString('\n'); strings.TrimSpace(answer) != p.Disk.Device {
			return errors.New("cancelled, nothing was changed")
		}
	}

	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"partition", x.partition},
		{"encrypt", x.encrypt},
		{"format", x.format},
		{"mount", x.mount},
		{"pacstrap", x.pacstrap},
		{"configure", x.configure},
		{"user", x.user},
		{"arcus", x.arcus},
		{"modules", x.modules},
		{"bootloader", x.bootloader},
		{"finish", x.finish},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprintf(x.out, "\n== %s\n", step.name)
		if err := step.run(); err != nil {
			return fmt.Errorf("%s: %w (see %s)", step.name, err, o.Log)
		}
	}
	fmt.Fprintf(o.Stdout, "\nInstalled. Remove the install medium and reboot; log in as %s.\n", p.User.Name)
	return nil
}

// EnsureGit installs git on the live system if it is missing: the ISO has
// none, and the dotfiles repository is checked by cloning it.
func EnsureGit(ctx context.Context, out io.Writer) error {
	if _, err := exec.LookPath("git"); err == nil {
		return nil
	}
	fmt.Fprintln(out, "+ pacman -Sy --needed --noconfirm git")
	cmd := exec.CommandContext(ctx, "pacman", "-Sy", "--needed", "--noconfirm", "git")
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("installing git on the live system: %w", err)
	}
	return nil
}

// secrets asks for the user's password (unless the plan has a hash) and the
// LUKS passphrase, each twice. Neither is written anywhere.
func (x *installer) secrets() error {
	x.password, x.luks = x.o.Password, x.o.Passphrase
	var err error
	if x.p.User.PasswordHash == "" && x.password == "" {
		if x.password, err = x.secret("Password for " + x.p.User.Name); err != nil {
			return err
		}
	}
	if x.p.Disk.Encrypt && x.luks == "" {
		if x.luks, err = x.secret("Disk encryption passphrase"); err != nil {
			return err
		}
	}
	return nil
}

func (x *installer) secret(prompt string) (string, error) {
	if f, ok := x.o.Stdin.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			stty := func(arg string) {
				cmd := exec.Command("stty", arg)
				cmd.Stdin = f
				_ = cmd.Run()
			}
			stty("-echo")
			defer stty("echo")
		}
	}
	var answers [2]string
	for i, label := range []string{prompt + ": ", "Again: "} {
		fmt.Fprint(x.o.Stdout, label)
		line, err := x.in.ReadString('\n')
		fmt.Fprintln(x.o.Stdout)
		if err != nil && line == "" {
			return "", fmt.Errorf("reading the %s: %w", strings.ToLower(prompt), err)
		}
		answers[i] = strings.TrimRight(line, "\r\n")
	}
	if answers[0] == "" || answers[0] != answers[1] {
		return "", errors.New(prompt + ": empty, or the two entries differ")
	}
	return answers[0], nil
}

// run runs a command with its output in the terminal and the log. stdin,
// when given, is passed to it but not logged.
func (x *installer) run(stdin string, name string, args ...string) error {
	fmt.Fprintf(x.out, "+ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(x.ctx, name, args...)
	cmd.Stdout, cmd.Stderr = x.out, x.out
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (x *installer) cmd(name string, args ...string) error { return x.run("", name, args...) }

// chroot runs a command inside the new system.
func (x *installer) chroot(args ...string) error {
	return x.cmd("arch-chroot", append([]string{target}, args...)...)
}

// asUser runs a command in the new system as the user, with a clean
// environment.
func (x *installer) asUser(args ...string) error {
	u := x.p.User.Name
	env := []string{"env", "-i", "HOME=/home/" + u, "USER=" + u, "LOGNAME=" + u, "PATH=/usr/local/sbin:/usr/local/bin:/usr/bin", "LANG=C.UTF-8"}
	return x.chroot(append(append([]string{"runuser", "-u", u, "--"}, env...), args...)...)
}

func write(path, content string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}

func (x *installer) partition() error {
	dev := x.p.Disk.Device
	parts := layout(x.p, x.uefi)
	if err := x.cmd("wipefs", "--all", "--force", dev); err != nil {
		return err
	}
	script := sfdiskScript(parts)
	fmt.Fprint(x.out, script)
	if err := x.run(script, "sfdisk", "--wipe", "always", "--wipe-partitions", "always", dev); err != nil {
		return err
	}
	if err := x.cmd("udevadm", "settle"); err != nil {
		return err
	}
	devices, err := partitions(dev)
	if err != nil {
		return err
	}
	if len(devices) != len(parts) {
		return fmt.Errorf("%s has %d partitions after partitioning, want %d", dev, len(devices), len(parts))
	}
	for i, part := range parts {
		x.devices[part.use] = devices[i]
	}
	x.root = x.devices["root"]
	return nil
}

func (x *installer) encrypt() error {
	if !x.p.Disk.Encrypt {
		fmt.Fprintln(x.out, "not encrypted")
		return nil
	}
	if err := x.run(x.luks, "cryptsetup", "luksFormat", "--type", "luks2", "--batch-mode", "--key-file", "-", x.root); err != nil {
		return err
	}
	if err := x.run(x.luks, "cryptsetup", "open", "--key-file", "-", x.root, "root"); err != nil {
		return err
	}
	x.root = "/dev/mapper/root"
	return nil
}

func (x *installer) format() error {
	for _, use := range []string{"esp", "boot", "swap", "root"} {
		dev := x.devices[use]
		if use == "root" {
			dev = x.root
		}
		if dev == "" {
			continue
		}
		args := mkfs(use, x.p.Disk.Filesystem, dev)
		if err := x.cmd(args[0], args[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// mount mounts the new system under /mnt. Types are named, not detected:
// right after mkfs, blkid may not know the new filesystem yet.
func (x *installer) mount() error {
	fs := x.p.Disk.Filesystem
	if fs == "btrfs" {
		if err := x.cmd("mount", "-t", fs, x.root, target); err != nil {
			return err
		}
		for _, sv := range subvolumes {
			if err := x.cmd("btrfs", "subvolume", "create", filepath.Join(target, sv.name)); err != nil {
				return err
			}
		}
		if err := x.cmd("umount", target); err != nil {
			return err
		}
		for _, sv := range subvolumes {
			dir := filepath.Join(target, sv.path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := x.cmd("mount", "-t", fs, "-o", "noatime,compress=zstd:1,subvol="+sv.name, x.root, dir); err != nil {
				return err
			}
		}
	} else if err := x.cmd("mount", "-t", fs, "-o", "noatime", x.root, target); err != nil {
		return err
	}

	boot, bootFS := x.devices["esp"], "vfat"
	if boot == "" {
		boot, bootFS = x.devices["boot"], "ext4"
	}
	if err := os.MkdirAll(filepath.Join(target, "boot"), 0o755); err != nil {
		return err
	}
	if err := x.cmd("mount", "-t", bootFS, boot, filepath.Join(target, "boot")); err != nil {
		return err
	}
	if swap := x.devices["swap"]; swap != "" {
		return x.cmd("swapon", swap)
	}
	return nil
}

// pacstrap installs only what the first boot and the first apply need: the
// machine module's packages and what apply runs with. The first apply takes
// those over and installs the rest.
func (x *installer) pacstrap() error {
	pkgs := append([]string{"base", "base-devel", "sudo", "git"}, x.facts.Packages()...)
	if err := x.cmd("pacstrap", append([]string{"-K", target}, pkgs...)...); err != nil {
		return err
	}
	if !x.p.System.Multilib {
		return nil
	}
	path := filepath.Join(target, "etc/pacman.conf")
	conf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := enableMultilib(string(conf))
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(x.out, "enabled [multilib]")
	return x.chroot("pacman", "-Sy")
}

func (x *installer) configure() error {
	var fstab bytes.Buffer
	cmd := exec.CommandContext(x.ctx, "genfstab", "-U", target)
	cmd.Stdout, cmd.Stderr = &fstab, x.out
	fmt.Fprintln(x.out, "+ genfstab -U", target)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("genfstab: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(target, "etc/fstab"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(fstab.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	s := x.p.System
	localtime := filepath.Join(target, "etc/localtime")
	_ = os.Remove(localtime)
	if err := os.Symlink("/usr/share/zoneinfo/"+s.Timezone, localtime); err != nil {
		return err
	}
	if err := x.chroot("hwclock", "--systohc"); err != nil {
		return err
	}
	charset, err := localeCharset("/usr/share/i18n/SUPPORTED", s.Locale)
	if err != nil {
		return err
	}
	gen, err := os.OpenFile(filepath.Join(target, "etc/locale.gen"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(gen, "%s %s\n", s.Locale, charset)
	if err := errors.Join(err, gen.Close()); err != nil {
		return err
	}
	if err := x.chroot("locale-gen"); err != nil {
		return err
	}
	for path, content := range map[string]string{
		"etc/locale.conf":   "LANG=" + s.Locale + "\n",
		"etc/vconsole.conf": "KEYMAP=" + s.Keymap + "\n",
		"etc/hostname":      s.Hostname + "\n",
		"etc/hosts":         fmt.Sprintf("127.0.0.1 localhost\n::1 localhost\n127.0.1.1 %s.localdomain %s\n", s.Hostname, s.Hostname),
	} {
		if err := write(filepath.Join(target, path), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func localeCharset(supported, locale string) (string, error) {
	data, err := os.ReadFile(supported)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		if name, charset, ok := strings.Cut(strings.TrimSpace(line), " "); ok && name == locale {
			return charset, nil
		}
	}
	return "", fmt.Errorf("locale %q is not supported", locale)
}

func (x *installer) user() error {
	u := x.p.User
	args := []string{"useradd", "-m", "-s", "/bin/bash"}
	if u.Wheel {
		args = append(args, "-G", "wheel")
	}
	if err := x.chroot(append(args, u.Name)...); err != nil {
		return err
	}
	if u.PasswordHash != "" {
		err := x.run(u.Name+":"+u.PasswordHash+"\n", "arch-chroot", target, "chpasswd", "-e")
		if err != nil {
			return err
		}
	} else if err := x.run(u.Name+":"+x.password+"\n", "arch-chroot", target, "chpasswd"); err != nil {
		return err
	}
	if !u.Wheel {
		return nil
	}
	if err := write(filepath.Join(target, "etc/sudoers.d/10-wheel"), "%wheel ALL=(ALL:ALL) ALL\n", 0o440); err != nil {
		return err
	}
	return x.chroot("passwd", "--lock", "root")
}

// arcus copies the arcus binary into the new system.
func (x *installer) arcus() error {
	return write(filepath.Join(target, "usr/local/bin/arcus"), string(x.arcusBin), 0o755)
}

// modules writes the arcus config of the new user (the installer modules,
// or a clone of the dotfiles repository) and runs the first apply as the
// user, through the same sudo path as on any other day. A temporary sudo
// rule lets it run unattended.
func (x *installer) modules() error {
	u := x.p.User.Name
	home := "/home/" + u
	if err := x.facts.Write(filepath.Join(target, machine.Dir, machine.Name)); err != nil {
		return err
	}
	// config is the arcus config as seen from here; inside the new system it
	// is ~/.config/arcus, which links into the dotfiles clone when there is one.
	config := filepath.Join(home, ".config/arcus")
	if x.p.Dotfiles.URL == "" {
		if err := WriteModules(filepath.Join(target, config, "modules")); err != nil {
			return err
		}
	} else {
		repo := filepath.Join(home, strings.TrimPrefix(x.p.Dotfiles.Path, "~/"))
		if err := x.asUser("git", "clone", "--", x.p.Dotfiles.URL, repo); err != nil {
			return err
		}
		if err := x.asUser("mkdir", "-p", home+"/.config"); err != nil {
			return err
		}
		if err := x.asUser("ln", "-s", filepath.Join(repo, x.p.Dotfiles.Arcus), config); err != nil {
			return err
		}
		config = filepath.Join(repo, x.p.Dotfiles.Arcus)
	}
	if err := x.hostFile(filepath.Join(target, config), filepath.Join(target, home, ".local/state/arcus/host.toml")); err != nil {
		return err
	}
	if err := x.chroot("chown", "-R", u+":", home); err != nil {
		return err
	}
	set, err := module.Scan(filepath.Join(target, config, "modules"))
	if err != nil {
		return err
	}

	// sudo applies the last matching rule, and sudoers.d is read in order.
	rule := filepath.Join(target, "etc/sudoers.d/99-arcus-install")
	if err := write(rule, u+" ALL=(ALL:ALL) NOPASSWD: ALL\n", 0o440); err != nil {
		return err
	}
	defer os.Remove(rule)
	if err := x.aurHelper(set, filepath.Join(target, config)); err != nil {
		return err
	}
	// A mirror that fails a download aborts the whole pacman transaction;
	// apply picks up where it stopped, so a failed first apply gets two
	// more tries.
	for attempt := 1; ; attempt++ {
		err := x.asUser("/usr/local/bin/arcus", "apply", "-y")
		if err == nil || attempt == 3 {
			return err
		}
		fmt.Fprintf(x.out, "the first apply failed (try %d of 3); a download may have failed, trying again\n", attempt)
	}
}

// hostFile writes the host file at path from the plan, which always
// selects the machine module (Host).
func (x *installer) hostFile(config, path string) error {
	profiles, err := profile.Scan(filepath.Join(config, "profiles"))
	if err != nil {
		return err
	}
	hostProfiles, hostModules, err := Host(x.p, profiles)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f := host.New(path)
	f.Profiles, f.Modules = hostProfiles, hostModules
	return f.Save()
}

// aurHelper builds the configured AUR helper from the AUR when a selected
// module lists AUR packages; apply needs it before it can install them.
func (x *installer) aurHelper(set module.Set, configDir string) error {
	in := plan.Input{Desired: x.desired, Modules: set}
	needed := false
	for _, names := range in.Declares(state.AUR) {
		needed = needed || len(names) > 0
	}
	if !needed {
		return nil
	}
	cfg, err := config.Load(filepath.Join(configDir, "config.toml"), filepath.Join(target, "home", x.p.User.Name))
	if err != nil {
		return err
	}
	// aurlock comes from an own build, which apply installs before the AUR
	// packages.
	if cfg.AURHelper == "aurlock" {
		return nil
	}
	pkg := cfg.AURHelper + "-bin"
	// Not /tmp: arch-chroot mounts a fresh one for every command.
	build := "/var/tmp/arcus-" + pkg
	defer os.RemoveAll(filepath.Join(target, build))
	if err := x.asUser("git", "clone", "--depth", "1", "https://aur.archlinux.org/"+pkg+".git", build); err != nil {
		return err
	}
	if err := x.asUser("sh", "-c", "cd "+build+" && makepkg --noconfirm"); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(target, build, pkg+"-*.pkg.tar.*"))
	files = slices.DeleteFunc(files, func(f string) bool { return strings.Contains(f, "-debug-") })
	if len(files) != 1 {
		return fmt.Errorf("building %s gave %d packages", pkg, len(files))
	}
	return x.chroot("pacman", "-U", "--noconfirm", strings.TrimPrefix(files[0], target))
}

func (x *installer) bootloader() error {
	nvidia := slices.Contains(x.facts.GPUs, machine.NVIDIA)
	if h := hooks(x.p.Disk.Encrypt, nvidia); h != "" {
		if err := write(filepath.Join(target, "etc/mkinitcpio.conf.d/10-arcus.conf"), h, 0o644); err != nil {
			return err
		}
	}
	if err := x.chroot("mkinitcpio", "-P"); err != nil {
		return err
	}
	if x.p.Disk.Encrypt {
		uuid, err := exec.CommandContext(x.ctx, "blkid", "-s", "UUID", "-o", "value", x.devices["root"]).Output()
		if err != nil {
			return fmt.Errorf("blkid: %w", err)
		}
		path := filepath.Join(target, "etc/default/grub")
		defaults, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated, err := grubCmdline(string(defaults), "rd.luks.name="+strings.TrimSpace(string(uuid))+"=root root=/dev/mapper/root")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return err
		}
	}
	if x.uefi {
		if err := x.chroot("grub-install", "--target=x86_64-efi", "--efi-directory=/boot", "--bootloader-id=GRUB"); err != nil {
			return err
		}
	} else if err := x.chroot("grub-install", "--target=i386-pc", x.p.Disk.Device); err != nil {
		return err
	}
	return x.chroot("grub-mkconfig", "-o", "/boot/grub/grub.cfg")
}

func (x *installer) finish() error {
	log, err := os.ReadFile(x.o.Log)
	if err != nil {
		return err
	}
	if err := write(filepath.Join(target, "var/log/arcus-install.log"), string(log), 0o600); err != nil {
		return err
	}
	if swap := x.devices["swap"]; swap != "" {
		if err := x.cmd("swapoff", swap); err != nil {
			return err
		}
	}
	if err := x.cmd("umount", "-R", target); err != nil {
		return err
	}
	if x.p.Disk.Encrypt {
		return x.cmd("cryptsetup", "close", "root")
	}
	return nil
}
