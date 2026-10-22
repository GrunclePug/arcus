package cli

import (
	"errors"
	"fmt"

	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/system"
)

// cmdClean makes the installed packages match the modules that are on:
// what they list (or what is protected) is marked explicitly installed,
// other packages that something needs are marked as dependencies, and the
// rest is removed. It runs only when the system matches the host file, so
// it never races apply over packages arcus installed.
func (a *app) cmdClean(args []string) error {
	var o opts
	fs := a.flags("clean", &o, true, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcus clean [-y] [-n]")
	}
	if err := a.load(&o); err != nil {
		return err
	}
	lock, err := a.userHalf().lock(!o.dry)
	if err != nil {
		return err
	}
	defer lock.Release()

	in, err := a.hostSelection(&o)
	if err != nil {
		return err
	}
	pending, err := a.plan(in)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf("the system doesn't match the host file ('arcus plan' shows %d step(s)); run 'arcus apply' first", len(pending))
	}
	steps, err := a.clean(in)
	if err != nil {
		return err
	}
	a.printPlan(steps)
	if o.dry || len(steps) == 0 {
		return nil
	}
	if !o.yes && !a.confirm("Proceed?") {
		return errors.New("cancelled, nothing was changed")
	}
	args = append([]string{"-clean", "-hash", plan.Hash(steps), "-home", a.paths.Home, "-config", a.paths.Config, "--"}, in.Desired...)
	if err := a.sudo(args); err != nil {
		return fmt.Errorf("clean failed: %w", err)
	}
	return nil
}

// clean queries pacman and plans arcus clean.
func (a *app) clean(in plan.Input) ([]plan.Step, error) {
	var p plan.Installed
	var err error
	if p.All, err = system.Installed(); err != nil {
		return nil, err
	}
	if p.Explicit, err = system.Explicit(); err != nil {
		return nil, err
	}
	if p.Unrequired, err = system.Unrequired(); err != nil {
		return nil, err
	}
	return plan.Clean(in, p), nil
}

// runClean carries out the steps of arcus clean, as root.
func runClean(steps []plan.Step, r *system.Runner) error {
	for _, s := range steps {
		var err error
		switch s.Op {
		case plan.MarkExplicit:
			err = r.PacmanMark(true, s.Names)
		case plan.MarkDeps:
			err = r.PacmanMark(false, s.Names)
		case plan.RemoveUnlisted:
			err = r.PacmanRemove(s.Names)
		default:
			err = fmt.Errorf("unknown clean step %q", s.Op)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
