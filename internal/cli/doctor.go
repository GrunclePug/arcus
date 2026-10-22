package cli

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
)

// report collects doctor findings.
type report struct {
	w             io.Writer
	issues, fixed int
}

func (r *report) problem(format string, args ...any) {
	r.issues++
	fmt.Fprintf(r.w, "  ✗ "+format+"\n", args...)
}

func (r *report) repaired(format string, args ...any) {
	r.fixed++
	fmt.Fprintf(r.w, "    → fixed: "+format+"\n", args...)
}

func (r *report) failed(err error) {
	fmt.Fprintf(r.w, "    → fix failed: %v\n", err)
}

func (r *report) err() error {
	if r.issues > r.fixed {
		return fmt.Errorf("%d problem(s) need attention", r.issues-r.fixed)
	}
	return nil
}

// cmdDoctor audits both halves and the host file. With -fix it applies only
// repairs that are provably safe: rolling back interrupted activations,
// finishing interrupted teardowns and forgetting resources removed outside
// arcus. It never deletes a file arcus did not create.
func (a *app) cmdDoctor(args []string) error {
	var o opts
	fs := a.flags("doctor", &o, false, false)
	fs.BoolVar(&o.fix, "fix", false, "apply safe repairs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.load(&o); err != nil {
		return err
	}
	uh := a.userHalf()
	lock, err := uh.lock(o.fix)
	if err != nil {
		return err
	}
	defer lock.Release()

	r := &report{w: a.stdout}
	fmt.Fprintln(a.stdout, "User half:")
	if st, err := uh.loadState(); err != nil {
		r.problem("%v", err)
	} else {
		a.doctorHalf(uh, st, o.fix, r)
	}

	// The system half is checked read-only first; sudo is only asked for
	// when there is something to repair.
	var buf bytes.Buffer
	sr := &report{w: &buf}
	fmt.Fprintln(&buf, "System half:")
	sh := a.systemHalf()
	func() {
		lock, err := sh.lock(false)
		if err != nil {
			sr.problem("%v", err)
			return
		}
		defer lock.Release()
		if st, err := sh.loadState(); err != nil {
			sr.problem("%v", err)
		} else {
			a.doctorHalf(sh, st, false, sr)
		}
	}()
	r.issues += sr.issues
	if o.fix && sr.issues > 0 {
		err := a.sudo([]string{"-doctor", "-home", a.paths.Home, "-config", a.paths.Config})
		if err == nil {
			r.fixed += sr.issues
		} else {
			fmt.Fprintf(a.stdout, "    → system repairs incomplete: %v\n", err)
		}
	} else {
		a.stdout.Write(buf.Bytes())
	}

	fmt.Fprintln(a.stdout, "Host file:")
	a.doctorHost(&o, r)
	a.doctorVersion(r)

	switch {
	case r.issues == 0:
		fmt.Fprintln(a.stdout, "\nNo problems found.")
		return nil
	case o.fix:
		fmt.Fprintf(a.stdout, "\n%d problem(s), %d fixed.\n", r.issues, r.fixed)
		return r.err()
	default:
		fmt.Fprintf(a.stdout, "\n%d problem(s). 'arcus doctor -fix' applies the safe repairs.\n", r.issues)
		return fmt.Errorf("doctor found %d problem(s)", r.issues)
	}
}

// doctorHost reports a host file that doesn't resolve or that the system
// doesn't match. Making it match is apply's job, not doctor's. Packages no
// module lists are a note, not a problem: 'arcus clean' handles them.
func (a *app) doctorHost(o *opts, r *report) {
	in, err := a.hostSelection(o)
	if err != nil {
		r.problem("%v", err)
		return
	}
	steps, err := a.plan(in)
	if err != nil {
		r.problem("planning: %v", err)
		return
	}
	if len(steps) > 0 {
		r.problem("the system doesn't match the host file: 'arcus plan' shows %d step(s); run 'arcus apply'", len(steps))
	}

	clean, err := a.clean(in)
	if err != nil {
		r.problem("querying packages: %v", err)
		return
	}
	for _, s := range clean {
		if s.Op == plan.RemoveUnlisted || s.Op == plan.MarkDeps {
			fmt.Fprintf(r.w, "  · %s: %s\n", notes[s.Op], strings.Join(s.Names, " "))
		}
	}
	if len(clean) > 0 {
		fmt.Fprintln(r.w, "    → add what you want to keep to a module; 'arcus clean' shows its plan for the rest and asks first")
	}
}

var notes = map[string]string{
	plan.RemoveUnlisted: "no module lists these and nothing needs them",
	plan.MarkDeps:       "no module lists these, but other packages need them",
}

// doctorHalf checks one half's ledgers, deployed files and ownership records.
func (a *app) doctorHalf(h *half, st *state.State, fix bool, r *report) {
	save := func() {
		if err := st.Save(h.statePath()); err != nil {
			r.failed(err)
		}
	}
	dropLayer := func(name string) {
		st.Modules = slices.DeleteFunc(st.Modules, func(p string) bool { return p == name })
		save()
	}

	ledgers, err := h.layer.ListLedgers()
	if err != nil {
		r.problem("%v", err)
		return
	}
	for _, name := range ledgers {
		l, err := h.layer.LoadLedger(name)
		if err != nil {
			r.problem("%v", err)
			continue
		}
		switch on := slices.Contains(st.Modules, name); {
		case !on:
			r.problem("%s has a %s ledger but no layer in the state (interrupted turn-on or orphan)", name, l.Status)
			if fix {
				if err := h.layer.Rollback(name); err != nil {
					r.failed(err)
				} else {
					r.repaired("rolled back the %s files of %s", h.layer.Subdir, name)
				}
			}
		case l.Status == layer.StatusDeactivating:
			r.problem("removing the %s files of %s was interrupted", h.layer.Subdir, name)
			if fix {
				if err := h.layer.Deactivate(a.ctx, name, st.Modules, false); err != nil {
					r.failed(err)
				} else {
					dropLayer(name)
					r.repaired("finished removing them")
				}
			}
		case l.Status != layer.StatusActive:
			r.problem("%s is in the %s stack but its ledger says %q", name, h.name, l.Status)
			if fix {
				if err := h.layer.Rollback(name); err != nil {
					r.failed(err)
				} else {
					dropLayer(name)
					r.repaired("rolled back the %s files of %s", h.layer.Subdir, name)
				}
			}
		}
	}

	for _, name := range slices.Clone(st.Modules) {
		l, err := h.layer.LoadLedger(name)
		if err != nil {
			continue
		}
		if l == nil {
			r.problem("%s is in the %s stack but has no ledger", name, h.name)
			if fix {
				dropLayer(name)
				r.repaired("removed it from the stack (nothing was recorded to undo)")
			}
			continue
		}
		if a.modules[name] == nil {
			r.problem("%s has deployed %s files but its module directory is gone; 'arcus apply' still removes them", name, h.layer.Subdir)
		}
		findings, err := h.layer.Audit(name, st.Modules)
		if err != nil {
			r.problem("auditing %s: %v", name, err)
			continue
		}
		for _, f := range findings {
			r.problem("%s: %s", name, f)
		}
	}

	dangling, err := h.layer.DanglingBackups()
	if err != nil {
		r.problem("%v", err)
	}
	for _, d := range dangling {
		r.problem("dangling backup %s is not referenced by any ledger (inspect it and restore or delete it by hand)", d)
	}

	for _, kind := range h.kinds {
		if len(st.Owned[kind]) == 0 {
			continue
		}
		present, err := a.present(kind)
		if err != nil {
			r.problem("querying %s: %v", kind, err)
			continue
		}
		if gone := st.Clone().Prune(kind, present); len(gone) > 0 {
			r.problem("%s recorded as added by arcus are gone (removed outside arcus): %s", kind, strings.Join(gone, " "))
			if fix {
				st.Prune(kind, present)
				save()
				r.repaired("forgot them")
			}
		}
	}
}
