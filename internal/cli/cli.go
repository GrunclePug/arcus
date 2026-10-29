// Package cli implements the arcus commands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/grunclepug/arcus/internal/config"
	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
	"github.com/grunclepug/arcus/internal/state"
)

const usage = `usage: arcus <command> [flags] [arguments]

  ls                       list profiles and modules: status, stack rank, files
  plan [name...]           show what apply (or on <name...>) would do
  on <name>                turn a module or profile on; record it in the host file
  off [-f] <name>          turn a module or profile off; remove it from the host file
  apply [-f]               make the system match the host file
  doctor [-fix]            check state, ledgers and deployed files; -fix applies safe repairs
  new <name>               create a user module
  rm <name>                delete a user module that is off
  mv <old> <new>           rename a user module
  rebuild <package...>     build and reinstall own builds even if their recipes didn't change
  clean                    remove packages no module lists; mark the rest's install reasons
  machine                  write this machine's module (kernel, bootloader, filesystems, swap)
  update                   replace arcus with the latest release from GitHub
  -version                 print the version

Flags (before the arguments):
  -y              don't ask for confirmation
  -n              only show the plan
  -f              off: also turn off modules that require it; off, apply: move
                  files edited since deployment aside to <path>.arcus-foreign

arcus runs as your user and asks sudo for the root steps.
`

// app carries one invocation. Tests build it with a root prefix, fake
// tools on PATH and an in-process sudo.
type app struct {
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	root           string // root prefix, "/" outside tests
	euid           int
	getenv         func(string) string
	hostname       func() (string, error)
	// sudo runs "arcus exec-system <args>" as root.
	sudo func(args []string) error
	// exe returns the path of the running binary, which update replaces.
	exe      func() (string, error)
	version  string // the running version, "dev" unless built by make
	releases string // the latest-release URL; empty skips doctor's check

	user     string // the main user's login name
	paths    config.Paths
	cfg      *config.Config
	modules  module.Set
	profiles profile.Set
	rebuild  []string // own builds to build again even if unchanged
}

// Run runs one arcus command; version is the running arcus's.
func Run(ctx context.Context, version string, args []string) error {
	a := &app{
		ctx:   ctx,
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		root:     "/",
		euid:     os.Geteuid(),
		getenv:   os.Getenv,
		hostname: os.Hostname,
		exe:      os.Executable,
		version:  version,
		releases: Releases,
	}
	a.sudo = a.execSudo
	return a.run(args)
}

func (a *app) run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return errors.New("no command given")
	}
	cmd, args := args[0], args[1:]

	if cmd == "exec-system" {
		return a.execSystem(args)
	}
	if a.euid == 0 {
		return errors.New("don't run arcus as root; it runs as your user and asks sudo for the root steps")
	}
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("looking up the current user: %w", err)
	}
	a.user = u.Username

	switch cmd {
	case "ls":
		return a.cmdLs(args)
	case "plan":
		return a.cmdPlan(args)
	case "on":
		return a.cmdOn(args)
	case "off":
		return a.cmdOff(args)
	case "apply":
		return a.cmdApply(args)
	case "doctor":
		return a.cmdDoctor(args)
	case "new":
		return a.cmdNew(args)
	case "rm":
		return a.cmdRm(args)
	case "mv":
		return a.cmdMv(args)
	case "rebuild":
		return a.cmdRebuild(args)
	case "clean":
		return a.cmdClean(args)
	case "machine":
		return a.cmdMachine(args)
	case "update":
		return a.cmdUpdate(args)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(a.stdout, usage)
		return nil
	}
	fmt.Fprint(a.stderr, usage)
	return fmt.Errorf("unknown command %q", cmd)
}

// opts are the flags shared by the commands.
type opts struct {
	yes, dry, force, fix bool
}

// flags returns a flag set with -y and -n for changing commands and -f when
// force is wanted.
func (a *app) flags(name string, o *opts, changing, force bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	if changing {
		fs.BoolVar(&o.yes, "y", false, "don't ask for confirmation")
		fs.BoolVar(&o.dry, "n", false, "only show the plan")
	}
	if force {
		fs.BoolVar(&o.force, "f", false, "force (see 'arcus help')")
	}
	return fs
}

// load resolves the paths and loads config.toml and every module.
func (a *app) load(o *opts) error {
	home := a.getenv("HOME")
	if !filepath.IsAbs(home) {
		return errors.New("HOME must be set to an absolute path")
	}
	return a.loadPaths(config.UserPaths(home, a.root, a.getenv))
}

func (a *app) loadPaths(paths config.Paths) error {
	cfg, err := config.Load(filepath.Join(paths.Config, "config.toml"), paths.Home)
	if err != nil {
		return err
	}
	modules, err := module.Scan(paths.ModuleDirs(cfg)...)
	if err != nil {
		return err
	}
	profiles, err := profile.Scan(filepath.Join(paths.Config, "profiles"))
	if err != nil {
		return err
	}
	// One namespace, so `arcus on <name>` is never ambiguous.
	for name := range profiles {
		if modules[name] != nil {
			return fmt.Errorf("%q is both a module (%s) and a profile (%s); rename one", name, modules[name].Dir, profiles[name].Path)
		}
	}
	a.paths, a.cfg, a.modules, a.profiles = paths, cfg, modules, profiles
	return nil
}

// hostPath returns the host file. Before 0.2.0 it was
// hosts/<hostname>.toml in the config directory; one left there is refused
// with the command that moves it, so on doesn't start a new, empty one.
func (a *app) hostPath() (string, error) {
	path := a.paths.HostFile()
	if _, err := os.Stat(path); !errors.Is(err, iofs.ErrNotExist) {
		return path, nil
	}
	name, err := a.hostname()
	if err != nil {
		return path, nil
	}
	old := filepath.Join(a.paths.Config, "hosts", name+".toml")
	if _, err := os.Stat(old); err == nil {
		return "", fmt.Errorf("the host file moved out of the config directory; move it with: mkdir -p %s && mv %s %s", filepath.Dir(path), old, path)
	}
	return path, nil
}

// half is the user or the system half: its state directory and layer.
type half struct {
	name  string
	dir   string
	layer *layer.Stacker
	kinds []string // ownership kinds recorded in this half
}

func (h *half) statePath() string { return filepath.Join(h.dir, "state.json") }

func (h *half) loadState() (*state.State, error) { return state.Load(h.statePath()) }

// lock takes the half's lock. An exclusive lock creates the state directory.
func (h *half) lock(exclusive bool) (*state.Lock, error) {
	if exclusive {
		if err := os.MkdirAll(h.dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", h.dir, err)
		}
	}
	return state.AcquireLock(filepath.Join(h.dir, ".lock"), exclusive)
}

func (a *app) dirs() map[string]string {
	dirs := make(map[string]string, len(a.modules))
	for name, m := range a.modules {
		dirs[name] = m.Dir
	}
	return dirs
}

func (a *app) userHalf() *half {
	return &half{
		name:  "user",
		dir:   a.paths.UserState,
		layer: layer.NewHome(a.paths.Home, a.paths.UserState, a.dirs()),
		kinds: []string{state.AUR, state.UserUnits, state.HomeDirs, state.HomeLinks},
	}
}

func (a *app) systemHalf() *half {
	return &half{
		name:  "system",
		dir:   a.paths.SystemState,
		layer: layer.NewSystem(a.root, a.paths.SystemState, a.dirs()),
		kinds: []string{state.Pacman, state.Local, state.SystemUnits, state.Groups},
	}
}

// execSudo runs this binary's exec-system through sudo with the terminal
// attached. sudo passes a terminal Ctrl-C on to it.
func (a *app) execSudo(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the arcus binary: %w", err)
	}
	cmd := exec.Command("sudo", append([]string{"--", exe, "exec-system"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	return cmd.Run()
}

// confirm asks a yes/no question. It reads one byte at a time so nothing
// meant for a later prompt (pacman, sudo) is swallowed by a buffer, and it
// gives up when the context is cancelled: arcus traps Ctrl-C, so the prompt
// must not wait for Enter after one.
func (a *app) confirm(question string) bool {
	fmt.Fprintf(a.stdout, "%s [y/N] ", question)
	answer := make(chan string, 1)
	go func() {
		var line []byte
		b := make([]byte, 1)
		for {
			n, err := a.stdin.Read(b)
			if n == 1 && b[0] != '\n' {
				line = append(line, b[0])
				continue
			}
			if n == 1 || err != nil {
				break
			}
		}
		answer <- strings.ToLower(strings.TrimSpace(string(line)))
	}()
	select {
	case got := <-answer:
		return got == "y" || got == "yes"
	case <-a.ctx.Done():
		fmt.Fprintln(a.stdout)
		return false
	}
}
