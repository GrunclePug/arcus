package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/profile"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

func (a *app) cmdOn(args []string) error {
	var o opts
	fs := a.flags("on", &o, true, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus on [-y] [-n] <module|profile>")
	}
	name := fs.Arg(0)
	return a.change(&o, true, func(f *host.File) error {
		if err := a.swap(f, name); err != nil {
			return err
		}
		added, err := a.pick(f, name)
		if err == nil && !added {
			fmt.Fprintf(a.stdout, "%s is already in the host file; applying it.\n", name)
		}
		return err
	})
}

// swap takes out of the host file the modules that conflict with module
// name, such as another theme, so turning name on replaces them; the plan
// shows it and asks as usual. A conflicting module that a profile brings
// in can't be swapped out: the profile has to change.
func (a *app) swap(f *host.File, name string) error {
	m := a.modules[name]
	if m == nil {
		return nil
	}
	selected, via, err := a.profiles.Expand(f.Profiles, f.Modules)
	if err != nil {
		return err
	}
	for _, other := range selected {
		o := a.modules[other]
		if other == name || o == nil || !slices.Contains(m.Conflicts, other) && !slices.Contains(o.Conflicts, name) {
			continue
		}
		if from := via[other]; from != profile.Host {
			return fmt.Errorf("%q conflicts with %q, which profile %q brings in; change the profile", name, other, from)
		}
		f.RemoveModule(other)
		fmt.Fprintf(a.stdout, "Swapping %s for %s (they conflict).\n", other, name)
	}
	return nil
}

// pick puts a module or a profile into the host file.
func (a *app) pick(f *host.File, name string) (bool, error) {
	switch {
	case a.profiles[name] != nil:
		return f.AddProfile(name), nil
	case a.modules[name] != nil:
		return f.AddModule(name), nil
	}
	return false, fmt.Errorf("no module or profile named %q", name)
}

func (a *app) cmdOff(args []string) error {
	var o opts
	fs := a.flags("off", &o, true, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus off [-f] [-y] [-n] <module|profile>")
	}
	name := fs.Arg(0)
	return a.change(&o, false, func(f *host.File) error {
		if a.profiles[name] != nil {
			if !f.RemoveProfile(name) {
				return fmt.Errorf("profile %q is not in the host file", name)
			}
			return nil
		}
		return a.offModule(f, name, o.force)
	})
}

// offModule takes a module out of the host file. A module that a profile
// brings in can't be turned off on its own: edit the profile, or turn the
// profile off.
func (a *app) offModule(f *host.File, name string, force bool) error {
	selected, via, err := a.profiles.Expand(f.Profiles, f.Modules)
	if err != nil {
		return err
	}
	desired, err := a.modules.Resolve(selected)
	if err != nil {
		return err
	}
	if !slices.Contains(desired, name) {
		return fmt.Errorf("module %q is not on", name)
	}
	dependents := a.modules.Dependents(name, selected)
	if len(dependents) > 0 && !force {
		return fmt.Errorf("%s require %q; turn them off first, or use -f to turn them off too", strings.Join(dependents, ", "), name)
	}
	for _, m := range append([]string{name}, dependents...) {
		if !slices.Contains(selected, m) {
			continue // only pulled in by requires
		}
		if from := via[m]; from != profile.Host {
			return fmt.Errorf("%q comes from profile %q; remove it from %s, or turn the profile off", m, from, a.profiles[from].Path)
		}
		f.RemoveModule(m)
	}
	if in, err := a.selection(f); err == nil && slices.Contains(in.Desired, name) {
		return fmt.Errorf("module %q is still required by the host file", name)
	}
	return nil
}

func (a *app) cmdApply(args []string) error {
	var o opts
	fs := a.flags("apply", &o, true, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcus apply [-f] [-y] [-n]")
	}
	return a.change(&o, false, nil)
}

// cmdPlan shows what apply would do, or on for the given modules or profiles.
func (a *app) cmdPlan(args []string) error {
	var o opts
	fs := a.flags("plan", &o, false, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	o.dry = true
	names := fs.Args()
	return a.change(&o, len(names) > 0, func(f *host.File) error {
		for _, name := range names {
			if _, err := a.pick(f, name); err != nil {
				return err
			}
		}
		return nil
	})
}

// change is the shared flow of on, off, apply and plan: edit the desired
// set, plan, show, confirm, run, and record the host file on success. A
// missing host file is created only when create is set (on); for apply it
// would mean turning everything off.
func (a *app) change(o *opts, create bool, edit func(*host.File) error) error {
	if err := a.load(o); err != nil {
		return err
	}
	lock, err := a.userHalf().lock(!o.dry)
	if err != nil {
		return err
	}
	defer lock.Release()

	path, err := a.hostPath(o)
	if err != nil {
		return err
	}
	f, err := host.Load(path)
	switch {
	case errors.Is(err, iofs.ErrNotExist) && create:
		f = host.New(path)
	case errors.Is(err, iofs.ErrNotExist):
		return fmt.Errorf("no host file at %s ('arcus on <module>' creates it)", path)
	case err != nil:
		return err
	}

	beforeProfiles, beforeModules := slices.Clone(f.Profiles), slices.Clone(f.Modules)
	if edit != nil {
		if err := edit(f); err != nil {
			return err
		}
	}
	in, err := a.selection(f)
	if err != nil {
		return err
	}
	steps, err := a.plan(in)
	if err != nil {
		return err
	}
	a.printPlan(steps)
	if o.dry {
		return nil
	}

	if len(steps) > 0 {
		if !o.yes && !a.confirm("Proceed?") {
			return errors.New("cancelled, nothing was changed")
		}
		if err := a.execute(steps, in, o); err != nil {
			return err
		}
	}
	if len(steps) > 0 {
		fmt.Fprintln(a.stdout, "Done.")
	}
	if !slices.Equal(beforeProfiles, f.Profiles) || !slices.Equal(beforeModules, f.Modules) {
		if err := f.Save(); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Updated %s.\n", path)
	}
	return nil
}

// hostSelection loads this machine's host file and resolves what it selects.
func (a *app) hostSelection(o *opts) (plan.Input, error) {
	path, err := a.hostPath(o)
	if err != nil {
		return plan.Input{}, err
	}
	f, err := host.Load(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return plan.Input{}, fmt.Errorf("no host file at %s ('arcus on <module>' creates it)", path)
	}
	if err != nil {
		return plan.Input{}, err
	}
	in, err := a.selection(f)
	if err != nil {
		return plan.Input{}, fmt.Errorf("%s: %w", path, err)
	}
	return in, nil
}

// selection expands the host file's profiles and modules and resolves
// their requirements.
func (a *app) selection(f *host.File) (plan.Input, error) {
	selected, _, err := a.profiles.Expand(f.Profiles, f.Modules)
	if err != nil {
		return plan.Input{}, err
	}
	return a.input(selected)
}

// input resolves the requirements of selected modules.
func (a *app) input(selected []string) (plan.Input, error) {
	desired, err := a.modules.Resolve(selected)
	if err != nil {
		if slices.Contains(selected, machine.Name) && a.modules[machine.Name] == nil {
			return plan.Input{}, fmt.Errorf("%w; 'arcus machine' writes the machine module", err)
		}
		return plan.Input{}, err
	}
	return plan.Input{Desired: desired, Modules: a.modules, Protected: a.cfg.Protected, Rebuild: a.rebuild}, nil
}

// needs reports whether kind has to be queried: something declares or owns it.
func needs(in plan.Input, st *state.State, kind string) bool {
	if len(st.Owned[kind]) > 0 {
		return true
	}
	for _, names := range in.Declares(kind) {
		if len(names) > 0 {
			return true
		}
	}
	return false
}

// present queries what of kind is on the system right now.
func (a *app) present(kind string) (map[string]bool, error) {
	switch kind {
	case state.Pacman, state.AUR, state.Local:
		return system.Installed()
	case state.SystemUnits:
		return system.EnabledUnits(false)
	case state.UserUnits:
		return system.EnabledUnits(true)
	case state.Groups:
		return system.Groups(a.user)
	case state.HomeDirs:
		return a.homeDirs()
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}

// homeDirs reports which of the home directories that a module lists or
// the user half records exist.
func (a *app) homeDirs() (map[string]bool, error) {
	st, err := a.userHalf().loadState()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range a.modules {
		names = append(names, m.Dirs...)
	}
	for _, owned := range st.Owned[state.HomeDirs] {
		names = append(names, owned...)
	}
	present := map[string]bool{}
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(a.paths.Home, n)); err == nil && info.IsDir() {
			present[n] = true
		}
	}
	return present, nil
}

// situation loads a half's state and queries what its plan depends on.
func (a *app) situation(in plan.Input, h *half) (plan.Half, error) {
	st, err := h.loadState()
	if err != nil {
		return plan.Half{}, err
	}
	s := plan.Half{State: st, Layer: h.layer}
	// Each query runs only if one of its kinds is declared or owned.
	type query struct {
		kinds []string
		into  *map[string]bool
	}
	queries := []query{{[]string{state.AUR}, &s.Installed}, {[]string{state.UserUnits}, &s.Units}, {[]string{state.HomeDirs}, &s.Dirs}}
	if h.name == "system" {
		queries = []query{
			{[]string{state.Pacman, state.Local}, &s.Installed},
			{[]string{state.SystemUnits}, &s.Units},
			{[]string{state.Groups}, &s.Groups},
		}
	}
	for _, q := range queries {
		if !slices.ContainsFunc(q.kinds, func(k string) bool { return needs(in, st, k) }) {
			continue
		}
		m, err := a.present(q.kinds[0])
		if err != nil {
			return plan.Half{}, err
		}
		*q.into = m
	}
	return s, nil
}

// plan computes the steps of both halves. The system half is read under a
// shared lock, which is released again before any root step runs.
func (a *app) plan(in plan.Input) ([]plan.Step, error) {
	sh := a.systemHalf()
	lock, err := sh.lock(false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	ss, err := a.situation(in, sh)
	if err != nil {
		return nil, err
	}
	sys, err := plan.System(in, ss)
	if err != nil {
		return nil, err
	}
	us, err := a.situation(in, a.userHalf())
	if err != nil {
		return nil, err
	}
	usr, err := plan.User(in, us)
	if err != nil {
		return nil, err
	}
	return plan.Merge(sys, usr), nil
}

func (a *app) printPlan(steps []plan.Step) {
	if len(steps) == 0 {
		fmt.Fprintln(a.stdout, "Nothing to do.")
		return
	}
	for _, s := range steps {
		who := "user"
		if s.Stage.Root() {
			who = "root"
		}
		fmt.Fprintf(a.stdout, "%s  %s\n", who, describe(s))
		for _, c := range s.Changes {
			fmt.Fprintf(a.stdout, "        %s\n", c)
			if s.Stage.Root() && c[0] == '~' {
				a.printDiff(c[2:])
			}
		}
	}
}

var kindNames = map[string]string{
	state.Pacman: "packages", state.AUR: "AUR packages", state.Local: "own builds",
	state.SystemUnits: "system units", state.UserUnits: "user units", state.Groups: "groups", state.HomeDirs: "home directories",
}

func describe(s plan.Step) string {
	files, units := "home files", "user units"
	if s.Stage.Root() {
		files, units = "system files", "system units"
	}
	pkgs := "AUR packages"
	if s.Kind() == state.Pacman {
		pkgs = "packages"
	}
	names := strings.Join(s.Names, " ")
	now := ""
	switch {
	case s.Now && s.Op == plan.Enable:
		now = " (and start them)"
	case s.Now && s.Op == plan.Disable:
		now = " (and stop them)"
	}

	switch s.Op {
	case plan.LayerOff:
		return fmt.Sprintf("remove the %s of %s", files, s.Module)
	case plan.LayerOn:
		return fmt.Sprintf("deploy the %s of %s", files, s.Module)
	case plan.Restack:
		return fmt.Sprintf("redeploy the %s from %s up, in order: %s", files, s.Module, names)
	case plan.Remove:
		return fmt.Sprintf("remove %s: %s", pkgs, names)
	case plan.Install:
		return fmt.Sprintf("install %s for %s: %s", pkgs, s.Module, names)
	case plan.Disable:
		return fmt.Sprintf("disable %s of %s: %s%s", units, s.Module, names, now)
	case plan.Enable:
		return fmt.Sprintf("enable %s for %s: %s%s", units, s.Module, names, now)
	case plan.Leave:
		return fmt.Sprintf("remove you from groups: %s", names)
	case plan.Join:
		return fmt.Sprintf("add you to groups for %s: %s", s.Module, names)
	case plan.Mkdir:
		return fmt.Sprintf("create home directories for %s: %s", s.Module, names)
	case plan.Rmdir:
		return fmt.Sprintf("remove home directories that are empty: %s", names)
	case plan.BuildLocal:
		return fmt.Sprintf("build %s from %s/pkg/%s", names, s.Module, names)
	case plan.InstallLocal:
		return fmt.Sprintf("install own build for %s: %s", s.Module, names)
	case plan.RemoveLocal:
		return fmt.Sprintf("remove own builds: %s", names)
	case plan.HandOver:
		return fmt.Sprintf("record %s under the modules that list them now:", kindNames[s.Of])
	case plan.Adopt:
		return fmt.Sprintf("take over %s for %s (already there): %s", kindNames[s.Of], s.Module, names)
	case plan.MarkExplicit:
		return fmt.Sprintf("mark as explicitly installed (a module lists them or they are protected): %s", names)
	case plan.MarkDeps:
		return fmt.Sprintf("mark as dependencies (no module lists them; other packages need them): %s", names)
	case plan.RemoveUnlisted:
		return fmt.Sprintf("remove packages no module lists, with the dependencies nothing else needs: %s", names)
	}
	return s.Op
}

// execute runs the steps stage by stage: user stages in this process, root
// stages through one sudo call each.
func (a *app) execute(steps []plan.Step, in plan.Input, o *opts) error {
	uh := a.userHalf()
	for _, group := range plan.ByStage(steps) {
		if err := a.ctx.Err(); err != nil {
			return fmt.Errorf("interrupted before %s steps: %w", group[0].Stage.Name(), err)
		}
		stage := group[0].Stage
		if stage.Root() {
			args := []string{
				"-stage", strconv.Itoa(int(stage)), "-hash", plan.Hash(group),
				"-home", a.paths.Home, "-config", a.paths.Config, "-cache", a.paths.Cache,
			}
			if len(a.rebuild) > 0 {
				args = append(args, "-rebuild", strings.Join(a.rebuild, ","))
			}
			if o.force {
				args = append(args, "-f")
			}
			args = append(append(args, "--"), in.Desired...)
			if err := a.sudo(args); err != nil {
				return fmt.Errorf("%s steps failed: %w", stage.Name(), err)
			}
			continue
		}

		st, err := uh.loadState()
		if err != nil {
			return err
		}
		r := &system.Runner{Stdin: a.stdin, Stdout: a.stdout, Stderr: a.stderr, NoConfirm: o.yes}
		if err := a.runSteps(uh, st, group, in, r, o.force); err != nil {
			return fmt.Errorf("%s steps failed: %w", stage.Name(), err)
		}
	}
	return nil
}

// runSteps carries out the steps of one stage in one half, saving the state
// after every change.
func (a *app) runSteps(h *half, st *state.State, steps []plan.Step, in plan.Input, r *system.Runner, force bool) error {
	save := func() error { return st.Save(h.statePath()) }
	off := func(name string) error {
		if err := h.layer.Deactivate(a.ctx, name, st.Modules, force); err != nil {
			return err
		}
		st.Modules = slices.DeleteFunc(st.Modules, func(p string) bool { return p == name })
		return save()
	}
	on := func(name string) error {
		if err := h.layer.Activate(a.ctx, name, st.Modules); err != nil {
			return err
		}
		st.Modules = append(st.Modules, name)
		return save()
	}

	for i := 0; i < len(steps); i++ {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		s := steps[i]
		// Consecutive steps of one operation run together.
		j := i + 1
		for j < len(steps) && steps[j].Op == s.Op {
			j++
		}

		switch s.Op {
		case plan.LayerOff:
			if err := off(s.Module); err != nil {
				return err
			}
		case plan.LayerOn:
			if err := on(s.Module); err != nil {
				return err
			}
		case plan.Restack:
			pos := slices.Index(st.Modules, s.Module)
			if pos < 0 {
				return fmt.Errorf("module %q has no %s layer to redeploy", s.Module, h.name)
			}
			upper := slices.Clone(st.Modules[pos:])
			for k := len(upper) - 1; k >= 0; k-- {
				if err := off(upper[k]); err != nil {
					return err
				}
			}
			for _, name := range s.Names {
				if err := on(name); err != nil {
					return err
				}
			}
		case plan.BuildLocal:
			if err := a.build(s, r); err != nil {
				return err
			}
		case plan.InstallLocal:
			if err := a.installLocal(st, steps[i:j], r, save); err != nil {
				return err
			}
			i = j - 1
		case plan.Install, plan.Enable, plan.Join, plan.Mkdir:
			if err := a.add(st, steps[i:j], r, save); err != nil {
				return err
			}
			i = j - 1
		case plan.Remove, plan.RemoveLocal, plan.Disable, plan.Leave, plan.Rmdir:
			if err := a.remove(st, steps[i:j], in, r, save); err != nil {
				return err
			}
			i = j - 1
		case plan.Adopt:
			present, err := a.present(s.Kind())
			if err != nil {
				return err
			}
			st.Own(s.Kind(), s.Module, slices.DeleteFunc(slices.Clone(s.Names), func(n string) bool { return !present[n] }))
			if err := save(); err != nil {
				return err
			}
		case plan.HandOver:
			present, err := a.present(s.Kind())
			if err != nil {
				return err
			}
			if got := in.Removals(st, s.Kind(), present); len(got) > 0 {
				return fmt.Errorf("the %s to remove changed since the plan was shown (now %v); run the command again", s.Kind(), got)
			}
			in.Reconcile(st, s.Kind(), present)
			if err := save(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown step %q", s.Op)
		}
	}
	return nil
}

// add installs, enables or joins, then records what was actually added
// (absent before, present after) for each step's module. Packages go in
// one transaction. What was added is recorded even if the command failed
// halfway, so nothing arcus added is ever lost track of.
func (a *app) add(st *state.State, steps []plan.Step, r *system.Runner, save func() error) error {
	kind := steps[0].Kind()
	before, err := a.present(kind)
	if err != nil {
		return err
	}

	var all []string
	for _, s := range steps {
		all = append(all, s.Names...)
	}
	var runErr error
	switch steps[0].Op {
	case plan.Install:
		if kind == state.Pacman {
			runErr = r.PacmanInstall(all)
		} else {
			runErr = r.AURInstall(a.cfg.AURHelper, all)
		}
	case plan.Enable:
		for _, s := range steps {
			if runErr = r.Enable(kind == state.UserUnits, s.Names, s.Now); runErr != nil {
				break
			}
		}
	case plan.Join:
		for _, g := range all {
			if runErr = r.JoinGroup(a.user, g); runErr != nil {
				break
			}
		}
	case plan.Mkdir:
		for _, d := range all {
			if runErr = os.MkdirAll(filepath.Join(a.paths.Home, d), 0o755); runErr != nil {
				break
			}
		}
	}

	after, err := a.present(kind)
	if err != nil {
		return errors.Join(runErr, err)
	}
	for _, s := range steps {
		st.Own(kind, s.Module, state.Added(s.Names, before, after))
	}
	return errors.Join(runErr, save())
}

// remove uninstalls, disables or leaves what reconciling ownership releases.
// It refuses if that is no longer exactly what the plan showed.
func (a *app) remove(st *state.State, steps []plan.Step, in plan.Input, r *system.Runner, save func() error) error {
	kind := steps[0].Kind()
	present, err := a.present(kind)
	if err != nil {
		return err
	}
	var want []string
	for _, s := range steps {
		want = append(want, s.Names...)
	}
	got := in.Removals(st, kind, present)
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		return fmt.Errorf("the %s to remove changed since the plan was shown (now %v); run the command again", kind, got)
	}

	switch steps[0].Op {
	case plan.Remove, plan.RemoveLocal:
		if kind == state.Pacman || kind == state.Local {
			err = r.PacmanRemove(want)
		} else {
			err = r.AURRemove(a.cfg.AURHelper, want)
		}
	case plan.Disable:
		for _, s := range steps {
			if err = r.Disable(kind == state.UserUnits, s.Names, s.Now); err != nil {
				break
			}
		}
	case plan.Leave:
		for _, g := range want {
			if err = r.LeaveGroup(a.user, g); err != nil {
				break
			}
		}
	case plan.Rmdir:
		// Only empty ones go; a directory with files in it is left alone.
		for _, d := range want {
			if rmErr := os.Remove(filepath.Join(a.paths.Home, d)); rmErr != nil && !errors.Is(rmErr, syscall.ENOTEMPTY) && !errors.Is(rmErr, syscall.EEXIST) {
				err = rmErr
				break
			}
		}
	}
	if err != nil {
		// The record stays; the next run sees what is gone and converges.
		return err
	}
	in.Reconcile(st, kind, present)
	if kind == state.Local {
		for _, n := range want {
			delete(st.Built, n)
		}
	}
	return save()
}
