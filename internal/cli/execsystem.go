package cli

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/config"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

var userNameRegex = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

// execSystem is the root side of arcus. The user's arcus runs it through
// sudo for one root stage: it recomputes the system half of the plan from
// the same inputs and refuses unless the stage's steps hash to what the user
// confirmed. With -doctor it repairs the system half; with -mv it renames a
// module's system layer and records; with -update it replaces its own binary
// with the release the user's arcus downloaded and verified.
func (a *app) execSystem(args []string) error {
	fs := flag.NewFlagSet("exec-system", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	stage := fs.Int("stage", 0, "root stage to run")
	hash := fs.String("hash", "", "hash of the confirmed steps")
	home := fs.String("home", "", "the user's home")
	configDir := fs.String("config", "", "the user's arcus config directory")
	cacheDir := fs.String("cache", "", "the user's arcus cache directory (own builds)")
	rebuild := fs.String("rebuild", "", "comma-separated own builds to rebuild")
	force := fs.Bool("f", false, "move edited files aside")
	doctor := fs.Bool("doctor", false, "repair the system half")
	mv := fs.Bool("mv", false, "rename: <old> <new> <old dir> <new dir>")
	clean := fs.Bool("clean", false, "run arcus clean")
	writeMachine := fs.Bool("machine", false, "write the machine module")
	update := fs.Bool("update", false, "replace this binary: <file>")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sudoUser := a.getenv("SUDO_USER")
	if a.euid != 0 || sudoUser == "" {
		return errors.New("exec-system is internal; arcus runs it through sudo")
	}
	if !userNameRegex.MatchString(sudoUser) || sudoUser == "root" {
		return fmt.Errorf("exec-system: invalid SUDO_USER %q", sudoUser)
	}
	a.user = sudoUser
	if *update {
		return a.installUpdate(*hash, fs.Args())
	}
	for _, dir := range []string{*home, *configDir} {
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("exec-system: %q is not an absolute path", dir)
		}
	}
	paths := config.UserPaths(*home, a.root, func(string) string { return "" })
	paths.Config = *configDir
	if *cacheDir != "" {
		if !filepath.IsAbs(*cacheDir) {
			return fmt.Errorf("exec-system: %q is not an absolute path", *cacheDir)
		}
		paths.Cache = *cacheDir
	}
	if *rebuild != "" {
		a.rebuild = strings.Split(*rebuild, ",")
	}
	if err := a.loadPaths(paths); err != nil {
		return err
	}

	h := a.systemHalf()
	lock, err := h.lock(true)
	if err != nil {
		return err
	}
	defer lock.Release()

	switch {
	case *doctor:
		st, err := h.loadState()
		if err != nil {
			return err
		}
		r := &report{w: a.stdout}
		fmt.Fprintln(a.stdout, "System half:")
		a.doctorHalf(h, st, true, r)
		return r.err()
	case *writeMachine:
		return a.writeMachine(*hash)
	case *mv:
		if fs.NArg() != 4 {
			return errors.New("exec-system -mv: want <old> <new> <old dir> <new dir>")
		}
		return a.renameHalf(h, fs.Arg(0), fs.Arg(1), fs.Arg(2), fs.Arg(3))
	}

	in, err := a.input(fs.Args())
	if err != nil {
		return err
	}
	if !slices.Equal(in.Desired, fs.Args()) {
		return errors.New("exec-system: the desired modules must be passed resolved")
	}
	r := &system.Runner{Stdin: a.stdin, Stdout: a.stdout, Stderr: a.stderr, NoConfirm: true}
	if *clean {
		steps, err := a.clean(in)
		if err != nil {
			return err
		}
		if plan.Hash(steps) != *hash {
			return errors.New("the packages to clean changed since the plan was shown; nothing was done, run the command again")
		}
		return runClean(steps, r)
	}

	s := plan.Stage(*stage)
	if !s.Root() {
		return fmt.Errorf("exec-system: %d is not a root stage", *stage)
	}
	sit, steps, err := a.systemPlan(in, h)
	if err != nil {
		return err
	}
	steps = plan.Filter(steps, s)
	if plan.Hash(steps) != *hash {
		return errors.New("the root steps changed since the plan was shown; nothing was done, run the command again")
	}
	return a.runSteps(h, sit.State, steps, in, r, *force)
}

// renameHalf moves a module's layer and records in one half after its
// directory was renamed.
func (a *app) renameHalf(h *half, oldName, newName, oldDir, newDir string) error {
	for _, name := range []string{oldName, newName} {
		if err := module.ValidateName(name); err != nil {
			return err
		}
	}
	st, err := h.loadState()
	if err != nil {
		return err
	}
	if err := h.layer.Rename(oldName, newName, oldDir, newDir, st.Modules); err != nil {
		return fmt.Errorf("renaming the %s layer (run 'arcus doctor'): %w", h.name, err)
	}
	st.Rename(oldName, newName)
	return st.Save(h.statePath())
}

// hasRecords reports whether a half still records anything for a module.
func hasRecords(h *half, st *state.State, name string) (bool, error) {
	l, err := h.layer.LoadLedger(name)
	return st.Has(name) || l != nil, err
}
