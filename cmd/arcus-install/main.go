// Command arcus-install installs Arch Linux from the live ISO.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/grunclepug/arcus/internal/install"
	"github.com/grunclepug/arcus/internal/install/tui"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
)

var Version = "dev"

const usage = `usage: arcus-install [command] [arguments]

  (none)                   edit an install plan in the TUI, then save or run it
  detect                   show the hardware and the modules it needs
  check <plan.toml>        validate a plan against this machine; changes nothing
  run [-yes] <plan.toml>   install the plan; erases the plan's disk
  -version                 print the version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "arcus-install:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return editor(ctx)
	}
	switch args[0] {
	case "-version", "--version":
		fmt.Println("arcus-install", Version)
		return nil
	case "detect":
		h := install.Detect("/")
		fmt.Printf("cpu        %s\ngpus       %s\nvm         %s\nbluetooth  %v\nmodules    %s\n",
			cmp.Or(machine.CPU("/proc/cpuinfo"), "-"), cmp.Or(strings.Join(h.GPUs, " "), "-"), cmp.Or(h.VM, "-"), h.Bluetooth, cmp.Or(strings.Join(h.Modules(), " "), "-"))
		return nil
	case "check":
		if len(args) != 2 {
			return errors.New("usage: arcus-install check <plan.toml>")
		}
		return check(ctx, args[1])
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		yes := fs.Bool("yes", false, "don't ask to type the device name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: arcus-install run [-yes] <plan.toml>")
		}
		return runPlan(ctx, fs.Arg(0), *yes)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return nil
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func check(ctx context.Context, file string) error {
	p, err := install.Load(file)
	if err != nil {
		return err
	}
	r := install.Check(ctx, p, install.Machine{Root: "/", Online: online})

	firmware := "BIOS"
	if r.UEFI {
		firmware = "UEFI"
	}
	if r.Disk.Path != "" {
		fmt.Printf("disk      %s, %d GiB, wiped\n", strings.TrimSpace(r.Disk.Path+" "+r.Disk.Model), r.Disk.Size>>30)
	}
	fmt.Printf("system    %s, %s, %s root, swap %s\n", firmware, p.System.Kernel, p.Disk.Filesystem, p.Disk.Swap)
	if len(r.Desired) > 0 {
		fmt.Printf("modules   %s\n", strings.Join(r.Desired, " "))
	}
	for _, w := range r.Warnings {
		fmt.Println("warning  ", w)
	}
	for _, e := range r.Problems {
		fmt.Println("problem  ", e)
	}
	if len(r.Problems) > 0 {
		return fmt.Errorf("%d problem(s); the plan can't run", len(r.Problems))
	}
	fmt.Println("The plan can run on this machine.")
	return nil
}

func runPlan(ctx context.Context, file string, yes bool) error {
	p, err := install.Load(file)
	if err != nil {
		return err
	}
	return install.Run(ctx, p, live, options(yes))
}

// live is the machine arcus-install runs on.
var live = install.Machine{Root: "/", Online: online}

func options(yes bool) install.Options {
	exe, _ := os.Executable()
	return install.Options{
		Yes: yes, Stdin: os.Stdin, Stdout: os.Stdout,
		Log: "/tmp/arcus-install.log", Arcus: filepath.Join(filepath.Dir(exe), "arcus"),
	}
}

// planFile is where the TUI saves the plan, in the live system's RAM.
const planFile = "/root/plan.toml"

// editor runs the TUI on what this machine offers, then saves the plan and,
// if asked, runs it.
func editor(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("the installer needs root; on the Arch ISO you are root")
	}
	_, err := os.Stat("/sys/firmware/efi")
	uefi := err == nil
	disks, err := install.Disks(live)
	if err != nil {
		return err
	}
	env := tui.Env{
		UEFI: uefi, Hardware: install.Detect("/"), Disks: disks,
		Timezones: lines("timedatectl", "list-timezones"),
		Locales:   locales("/usr/share/i18n/SUPPORTED"),
		Keymaps:   lines("localectl", "list-keymaps"),
		Modules: func(p *install.Plan) (module.Set, profile.Set, error) {
			if p.Dotfiles.URL != "" {
				if err := install.EnsureGit(ctx, io.Discard); err != nil {
					return nil, nil, err
				}
			}
			return install.Modules(ctx, p, install.MachineFacts(p, uefi, "/"))
		},
		Save: save,
	}
	r, err := tui.Run(env, tui.Defaults())
	if err != nil {
		return err
	}
	if !r.Run {
		return nil
	}
	if _, err := save(r.Plan); err != nil {
		return err
	}
	o := options(false)
	o.Password, o.Passphrase = r.Password, r.Passphrase
	return install.Run(ctx, r.Plan, live, o)
}

func save(p *install.Plan) (string, error) {
	var buf bytes.Buffer
	if err := p.Encode(&buf); err != nil {
		return "", err
	}
	return planFile, os.WriteFile(planFile, buf.Bytes(), 0o600)
}

// lines returns a command's output lines, or nil if it fails.
func lines(name string, args ...string) []string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// locales returns the locale names of glibc's SUPPORTED file.
func locales(file string) []string {
	data, _ := os.ReadFile(file)
	var names []string
	for line := range strings.Lines(string(data)) {
		if name, _, ok := strings.Cut(line, " "); ok {
			names = append(names, name)
		}
	}
	return names
}

// online reports whether the Arch repositories can be reached.
func online(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", "archlinux.org:443")
	if err != nil {
		return err
	}
	return conn.Close()
}
