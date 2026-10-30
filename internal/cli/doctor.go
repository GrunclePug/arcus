package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
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
	fmt.Fprintln(a.stdout, "Machine:")
	a.doctorMachine(&o, r)
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
	// Packages to mark explicitly installed need no hint: a module lists them.
	noted := false
	for _, s := range clean {
		if s.Op == plan.RemoveUnlisted || s.Op == plan.MarkDeps {
			fmt.Fprintf(r.w, "  · %s: %s\n", notes[s.Op], strings.Join(s.Names, " "))
			noted = true
		}
	}
	if noted {
		fmt.Fprintln(r.w, "    → add what you want to keep to a module; 'arcus clean' shows its plan for the rest and asks first")
	}
}

// doctorMachine reports what the machine has that its modules don't say: a
// machine module the system has moved on from, units and groups no module
// that's on lists, and aurlock approvals no module lists at all. It only
// reports. Writing the machine module changes packages, and disabling a
// unit or leaving a group can break what was set up on purpose, so -fix
// leaves all of it alone.
func (a *app) doctorMachine(o *opts, r *report) {
	a.doctorMachineModule(r)
	if in, err := a.hostSelection(o); err == nil { // doctorHost reports the error
		a.doctorUnits(in, r)
		a.doctorGroups(in, r)
	}
	a.doctorApprovals(r)
}

// doctorMachineModule compares the machine module with what 'arcus machine'
// would write now, such as drivers for hardware it learned to detect since.
func (a *app) doctorMachineModule(r *report) {
	dir := filepath.Join(a.paths.Machine, machine.Name)
	if _, err := os.Stat(dir); errors.Is(err, iofs.ErrNotExist) {
		return
	}
	facts, err := a.machineFacts()
	if err != nil {
		r.problem("detecting the machine: %v", err)
		return
	}
	changes, err := fileChanges(dir, facts.Files())
	if err != nil {
		r.problem("reading the machine module: %v", err)
		return
	}
	if len(changes) > 0 {
		r.problem("the machine module is out of date: run 'arcus machine'")
	}
}

// doctorUnits reports units enabled outside their vendor preset that no
// module that's on lists, nor pulls in through Also= in its unit files.
func (a *app) doctorUnits(in plan.Input, r *report) {
	found := false
	for _, kind := range []string{state.SystemUnits, state.UserUnits} {
		user := kind == state.UserUnits
		enabled, err := system.EnabledOffPreset(user)
		if err != nil {
			r.problem("querying %s: %v", kindNames[kind], err)
			continue
		}
		var declared []string
		for _, names := range in.Declares(kind) {
			declared = append(declared, names...)
		}
		listed := map[string]bool{}
		for _, u := range system.WithAlso(system.UnitDirs(a.root, a.xdg("XDG_CONFIG_HOME", ".config"), user), declared) {
			listed[u], listed[system.Template(u)] = true, true
		}
		extra := slices.DeleteFunc(enabled, func(u string) bool { return listed[u] })
		if len(extra) > 0 {
			r.problem("%s enabled that no module lists: %s", kindNames[kind], strings.Join(extra, " "))
			found = true
		}
	}
	if found {
		fmt.Fprintln(r.w, "    → list the ones to keep in a module's [units]; arcus disables nothing it didn't enable")
	}
}

// doctorGroups reports the user's groups that no module that's on lists,
// apart from those the user has from the start: their own group, wheel
// (the installer's) and users (Arch's default).
func (a *app) doctorGroups(in plan.Input, r *report) {
	groups, err := system.Groups(a.user)
	if err != nil {
		r.problem("querying groups: %v", err)
		return
	}
	listed := map[string]bool{a.user: true, "wheel": true, "users": true}
	for _, names := range in.Declares(state.Groups) {
		for _, g := range names {
			listed[g] = true
		}
	}
	var extra []string
	for g := range groups {
		if !listed[g] {
			extra = append(extra, g)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		r.problem("groups no module lists: %s", strings.Join(extra, " "))
		fmt.Fprintln(r.w, "    → list the ones to keep in a module's groups")
	}
}

// doctorApprovals reports aurlock approvals for packages no module lists,
// whether it's on or off: approvals sync between machines, so one may be
// for a package only another machine installs. Approvals are by package
// base and modules list package names; aurlock's record of what it
// installed here links the two where they differ.
func (a *app) doctorApprovals(r *report) {
	if a.cfg.AURHelper != "aurlock" {
		return
	}
	approved, err := readColumns(filepath.Join(a.xdg("XDG_CONFIG_HOME", ".config"), "aurlock", "approved"))
	if errors.Is(err, iofs.ErrNotExist) {
		return
	}
	if err != nil {
		r.problem("reading the aurlock approvals: %v", err)
		return
	}
	installed, err := readColumns(filepath.Join(a.xdg("XDG_STATE_HOME", ".local/state"), "aurlock", "installed"))
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		r.problem("reading what aurlock installed: %v", err)
		return
	}

	listed := map[string]bool{}
	for _, m := range a.modules {
		for _, p := range m.Packages.AUR {
			listed[p] = true
		}
	}
	for _, f := range installed { // pkgname pkgbase ...
		if len(f) > 1 && listed[f[0]] {
			listed[f[1]] = true
		}
	}
	var extra []string
	for _, f := range approved { // pkgbase commit ...
		if !listed[f[0]] && !slices.Contains(extra, f[0]) {
			extra = append(extra, f[0])
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		r.problem("aurlock approvals no module lists: %s", strings.Join(extra, " "))
		fmt.Fprintln(r.w, "    → list them in a module's aur, or drop the approvals with 'aurlock forget'")
	}
}

// xdg returns the directory an XDG variable names, or fallback under $HOME
// when it is unset or relative.
func (a *app) xdg(variable, fallback string) string {
	if dir := a.getenv(variable); filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(a.paths.Home, fallback)
}

// readColumns reads a file of whitespace-separated columns, skipping blank
// lines and # comments.
func readColumns(path string) ([][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for line := range strings.Lines(string(data)) {
		if f := strings.Fields(line); len(f) > 0 && !strings.HasPrefix(f[0], "#") {
			rows = append(rows, f)
		}
	}
	return rows, nil
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
