package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/history"
	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

// report collects doctor findings.
type report struct {
	w io.Writer
	// issues are the findings, repairable those -fix has a repair for;
	// fixed and failures count the repairs done and failed.
	issues, repairable, fixed, failures int
	// beforeRepair runs once, before the first repair: the history's
	// point of no return.
	beforeRepair func()
	// ctx stops the repairs not yet started when it is cancelled.
	ctx context.Context
}

func (r *report) problem(format string, args ...any) {
	r.issues++
	fmt.Fprintf(r.w, "  ✗ "+format+"\n", args...)
}

// fixable is a problem -fix has a repair for.
func (r *report) fixable(format string, args ...any) {
	r.repairable++
	r.problem(format, args...)
}

// repairing comes right before a repair changes anything; it reports
// whether to go ahead, which a cancelled run doesn't.
func (r *report) repairing() bool {
	cancelled := func() bool {
		if r.ctx != nil && r.ctx.Err() != nil {
			r.failed(errors.New("cancelled"))
			return true
		}
		return false
	}
	if cancelled() {
		return false
	}
	if r.beforeRepair != nil {
		r.beforeRepair()
		r.beforeRepair = nil
	}
	return !cancelled()
}

func (r *report) repaired(format string, args ...any) {
	r.fixed++
	fmt.Fprintf(r.w, "    → fixed: "+format+"\n", args...)
}

func (r *report) failed(err error) {
	r.failures++
	fmt.Fprintf(r.w, "    → fix failed: %v\n", err)
}

// err fails a repair run in which a repair failed; findings no repair
// covers are not a failure of the run.
func (r *report) err() error {
	if r.failures > 0 {
		return fmt.Errorf("%d repair(s) failed", r.failures)
	}
	return nil
}

// cmdDoctor audits both halves, the host file, the machine and the state
// history. With -fix it applies only repairs that are provably safe:
// releasing what an interrupted restack held, rolling back interrupted
// activations, finishing interrupted teardowns, forgetting resources
// removed outside arcus and removing the history's stale locks. It never
// deletes a file arcus did not create, except a home-half bytecode cache
// Python rewrote over one arcus deployed. What is left afterwards comes
// from a second, read-only audit, and decides the exit status.
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
	lock, err := a.lockUser(o.fix)
	if err != nil {
		return err
	}
	defer lock.Release()
	failed := false
	defer func() { a.record(failed) }()

	// Before any repair, so the history can record it; not once cancelled.
	var unlocked []string
	if o.fix && a.ctx.Err() == nil {
		unlocked = a.fixHistory()
	}

	ur := &report{w: a.stdout, beforeRepair: a.beginHistory, ctx: a.ctx}
	fmt.Fprintln(a.stdout, "User half:")
	if st, err := uh.loadState(); err != nil {
		ur.problem("%v", err)
	} else {
		a.doctorHalf(uh, st, o.fix, ur)
	}
	userLeft := ur.issues
	if o.fix && ur.repairable > 0 {
		userLeft = a.auditHalf(uh, io.Discard, false).issues // under the lock held
	}

	// The system half is checked read-only first; sudo is only asked for
	// when there is something to repair.
	var buf bytes.Buffer
	sr := a.auditHalf(a.systemHalf(), &buf, true)
	systemLeft, systemFixed, sudoFailed := sr.issues, 0, false
	if o.fix && sr.repairable > 0 {
		err := a.begin()
		if err == nil {
			err = a.sudo([]string{"-doctor", "-home", a.paths.Home, "-config", a.paths.Config})
		}
		if err != nil {
			sudoFailed = true
			fmt.Fprintf(a.stdout, "    → system repairs incomplete: %v\n", err)
		}
		var after bytes.Buffer
		systemLeft = a.auditHalf(a.systemHalf(), &after, true).issues
		if systemLeft > 0 {
			fmt.Fprint(a.stdout, strings.Replace(after.String(), "System half:", "System half, what is left:", 1))
		}
		systemFixed = max(0, sr.issues-systemLeft)
	} else {
		a.stdout.Write(buf.Bytes())
	}
	failed = ur.failures > 0 || sudoFailed

	r := &report{w: a.stdout}
	fmt.Fprintln(a.stdout, "Host file:")
	a.doctorHost(&o, r)
	fmt.Fprintln(a.stdout, "Machine:")
	a.doctorMachine(&o, r)
	a.doctorVersion(r)
	fmt.Fprintln(a.stdout, "History:")
	a.doctorHistory(r, unlocked)

	issues := ur.issues + sr.issues + r.issues
	left := userLeft + systemLeft + r.issues
	switch {
	case issues == 0:
		fmt.Fprintln(a.stdout, "\nNo problems found.")
		return nil
	case o.fix:
		fmt.Fprintf(a.stdout, "\n%d problem(s), %d fixed.\n", issues, ur.fixed+systemFixed)
		if left > 0 {
			return fmt.Errorf("%d problem(s) need attention", left)
		}
		return nil
	default:
		fmt.Fprintf(a.stdout, "\n%d problem(s). 'arcus doctor -fix' applies the safe repairs.\n", issues)
		return fmt.Errorf("doctor found %d problem(s)", issues)
	}
}

// auditHalf checks a half read-only into w, under its shared lock when
// lock is set.
func (a *app) auditHalf(h *half, w io.Writer, lock bool) *report {
	r := &report{w: w}
	fmt.Fprintf(w, "%s half:\n", strings.ToUpper(h.name[:1])+h.name[1:])
	if lock {
		l, err := h.lock(false)
		if err != nil {
			r.problem("%v", err)
			return r
		}
		defer l.Release()
	}
	if st, err := h.loadState(); err != nil {
		r.problem("%v", err)
	} else {
		a.doctorHalf(h, st, false, r)
	}
	return r
}

// fixHistory removes the stale locks a killed arcus left in the history
// and sets its mode back, under the exclusive user lock (a git child of a
// killed arcus would still hold it). It returns the locks it removed.
func (a *app) fixHistory() []string {
	if !a.history {
		return nil
	}
	repo := a.repo()
	if _, err := repo.Check(true); err != nil {
		return nil // doctorHistory reports it
	}
	removed, _ := repo.RemoveStaleLocks()
	return removed
}

// doctorHistory reports a history arcus can't write to.
func (a *app) doctorHistory(r *report, unlocked []string) {
	if !a.history {
		fmt.Fprintf(r.w, "  · %v\n", history.ErrNoGit)
		return
	}
	for _, path := range unlocked {
		fmt.Fprintf(r.w, "  · removed the stale lock %s\n", path)
	}
	repo := a.repo()
	wrongMode, err := repo.Check(false)
	if err != nil {
		r.problem("%v", err)
		return
	}
	if wrongMode {
		r.problem("%s should be readable by you only (mode 0700); the next command that records, or 'arcus doctor -fix', sets it", repo.Dir)
	}
	for _, path := range repo.StaleLocks() {
		r.problem("stale lock %s, left by a killed arcus: 'arcus doctor -fix' removes it", path)
	}
	if _, err := repo.Status(); err != nil {
		r.problem("%v", err)
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
	steps, err := a.plan(in, nil)
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

// doctorUnits reports enabled system or user units that no module that's on
// lists, nor pulls in through Also= in its unit files.
func (a *app) doctorUnits(in plan.Input, r *report) {
	found := false
	for _, kind := range []string{state.SystemUnits, state.UserUnits} {
		extra, err := a.unlistedUnits(in, kind == state.UserUnits)
		if err != nil {
			r.problem("querying %s: %v", kindNames[kind], err)
			continue
		}
		if len(extra) > 0 {
			r.problem("%s enabled that no module lists: %s", kindNames[kind], strings.Join(extra, " "))
			found = true
		}
	}
	if found {
		fmt.Fprintln(r.w, "    → list the ones to keep in a module's [units]; arcus disables nothing it didn't enable")
	}
}

// unlistedUnits returns the enabled system or user units that no module
// that's on lists, nor pulls in through Also= in its unit files, sorted.
func (a *app) unlistedUnits(in plan.Input, user bool) ([]string, error) {
	dirs := a.unitDirs(user)
	enabledSet, err := system.EnabledUnits(a.root, dirs)
	if err != nil {
		return nil, err
	}
	enabled := slices.Sorted(maps.Keys(enabledSet))
	kind := state.SystemUnits
	if user {
		kind = state.UserUnits
	}
	var declared []string
	for _, names := range in.Declares(kind) {
		declared = append(declared, names...)
	}
	withAlso, err := system.WithAlso(a.root, dirs, declared)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, u := range withAlso {
		listed[u] = true
	}
	return slices.DeleteFunc(enabled, func(u string) bool { return listed[u] }), nil
}

// unitDirs are the directories systemd reads system or user unit files from.
func (a *app) unitDirs(user bool) []string {
	return system.UnitDirs(a.root, a.xdg("XDG_CONFIG_HOME", ".config"), a.xdg("XDG_DATA_HOME", ".local/share"), user)
}

// doctorGroups reports the user's groups that no module that's on lists.
func (a *app) doctorGroups(in plan.Input, r *report) {
	extra, err := a.unlistedGroups(in)
	if err != nil {
		r.problem("querying groups: %v", err)
		return
	}
	if len(extra) > 0 {
		r.problem("groups no module lists: %s", strings.Join(extra, " "))
		fmt.Fprintln(r.w, "    → list the ones to keep in a module's groups")
	}
}

// unlistedGroups returns the user's groups that no module that's on lists,
// sorted, apart from those the user has from the start: their own group,
// their primary group, wheel (the installer's) and users (Arch's default).
func (a *app) unlistedGroups(in plan.Input) ([]string, error) {
	groups, err := system.Groups(a.user)
	if err != nil {
		return nil, err
	}
	primary, err := system.PrimaryGroup(a.user)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{a.user: true, primary: true, "wheel": true, "users": true}
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
	slices.Sort(extra)
	return extra, nil
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
	// A repair counts as done only once the state recording it is saved.
	save := func() bool {
		if err := st.Save(h.statePath()); err != nil {
			r.failed(err)
			return false
		}
		return true
	}
	dropLayer := func(name string) bool {
		st.Modules = slices.DeleteFunc(st.Modules, func(p string) bool { return p == name })
		return save()
	}

	ledgers, err := h.layer.ListLedgers()
	if err != nil {
		r.problem("%v", err)
		return
	}
	// The hold goes first: a turn-off finished before it would find a held
	// subdirectory keeping its own directory non-empty and lose track of it.
	if slices.Contains(ledgers, layer.HoldLedger) {
		r.fixable("a restack of the %s layers was interrupted and kept the directories they created", h.name)
		if fix && r.repairing() {
			if err := h.layer.Release(); err != nil {
				r.failed(fmt.Errorf("%w (removing %s by hand gives the held directories up)", err, h.layer.HoldPath()))
				return
			}
			r.repaired("gave them to the layers using them and removed the empty ones")
		}
	}
	for _, name := range ledgers {
		if name == layer.HoldLedger {
			continue
		}
		l, err := h.layer.LoadLedger(name)
		if err != nil {
			r.problem("%v", err)
			continue
		}
		switch on := slices.Contains(st.Modules, name); {
		case !on:
			r.fixable("%s has a %s ledger but no layer in the state (interrupted turn-on or orphan)", name, l.Status)
			if fix && r.repairing() {
				if err := h.layer.Rollback(name); err != nil {
					r.failed(err)
				} else {
					r.repaired("rolled back the %s files of %s", h.layer.Subdir, name)
				}
			}
		case l.Status == layer.StatusDeactivating:
			r.fixable("removing the %s files of %s was interrupted", h.layer.Subdir, name)
			if fix && r.repairing() {
				if err := h.layer.Deactivate(a.ctx, name, st.Modules, false); err != nil {
					r.failed(err)
				} else if dropLayer(name) {
					r.repaired("finished removing them")
				}
			}
		case l.Status != layer.StatusActive:
			r.fixable("%s is in the %s stack but its ledger says %q", name, h.name, l.Status)
			if fix && r.repairing() {
				if err := h.layer.Rollback(name); err != nil {
					r.failed(err)
				} else if dropLayer(name) {
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
			r.fixable("%s is in the %s stack but has no ledger", name, h.name)
			if fix && r.repairing() {
				if dropLayer(name) {
					r.repaired("removed it from the stack (nothing was recorded to undo)")
				}
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
			r.fixable("%s recorded as added by arcus are gone (removed outside arcus): %s", kind, strings.Join(gone, " "))
			if fix && r.repairing() {
				st.Prune(kind, present)
				if save() {
					r.repaired("forgot them")
				}
			}
		}
	}
}
