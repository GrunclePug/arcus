// Package plan computes the ordered steps that bring the system in line with
// a desired set of modules. It only reads; executing is the caller's job.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
	"github.com/grunclepug/arcus/internal/system"
)

// Stage groups steps that run together, in this order: teardown first (so a
// conflicting package or file is gone before its replacement arrives), then
// packages, then setup. Repository packages come before own builds are
// built, so a build finds the dependencies its module lists installed.
// Root stages run in one sudo call each.
type Stage int

const (
	UserTeardown   Stage = iota + 1 // user units disabled, home layers off
	SystemTeardown                  // groups left, system units disabled, system layers off
	AURRemove                       // AUR packages removed
	Packages                        // pacman packages and own builds removed, pacman packages installed
	Build                           // own builds made from their recipes (makepkg, as the user)
	LocalPackages                   // own builds installed
	AURInstall                      // AUR packages installed
	SystemSetup                     // system layers on, system units enabled, groups joined
	UserSetup                       // home layers on, user units enabled
)

var stageNames = map[Stage]string{
	UserTeardown: "user teardown", SystemTeardown: "system teardown", AURRemove: "AUR removal",
	Packages: "package", Build: "build", LocalPackages: "own build install", AURInstall: "AUR install",
	SystemSetup: "system setup", UserSetup: "user setup",
}

// Name returns a short description for messages.
func (s Stage) Name() string {
	if name, ok := stageNames[s]; ok {
		return name
	}
	return fmt.Sprintf("stage %d", int(s))
}

// Root reports whether the stage runs as root.
func (s Stage) Root() bool {
	return s == SystemTeardown || s == Packages || s == LocalPackages || s == SystemSetup
}

// Step operations.
const (
	LayerOff = "layer-off"
	LayerOn  = "layer-on"
	Restack  = "restack" // the layer of Module and every layer above it go off, then Names come on in order
	Remove   = "remove"  // packages
	Install  = "install"
	Disable  = "disable" // units
	Enable   = "enable"
	Leave    = "leave" // groups
	Join     = "join"

	BuildLocal   = "build" // own builds: makepkg into the cache
	InstallLocal = "install-local"
	RemoveLocal  = "remove-local"

	RemoveConflicting = "remove-conflicting" // installed packages that the next install conflicts with

	HandOver = "hand-over" // ownership moves to the modules that list the resources now
	Adopt    = "adopt"     // resources already there become recorded for the module that lists them
	Mkdir    = "mkdir"     // home directories
	Rmdir    = "rmdir"

	// arcus clean: install reasons follow the modules, the rest is removed.
	MarkExplicit   = "mark-explicit"
	MarkDeps       = "mark-deps"
	RemoveUnlisted = "remove-unlisted"
)

// Step is one action. Install, Enable and Join steps name the module the
// resources are recorded for; Disable steps name the module that owned them.
type Step struct {
	Stage   Stage    `json:"stage"`
	Op      string   `json:"op"`
	Module  string   `json:"module,omitempty"`
	Names   []string `json:"names,omitempty"`
	Now     bool     `json:"now,omitempty"`     // units: also start or stop them
	Changes []string `json:"changes,omitempty"` // "+ path", "- path", "~ path" (changed module), "! path" (replaced)
	Recipe  string   `json:"recipe,omitempty"`  // own builds: hash of the recipe to build or install
	Of      string   `json:"of,omitempty"`      // hand-over: the resource kind
	// Package removals: released packages that stay installed, marked as
	// dependencies, because packages outside the removal need them.
	Kept []string `json:"kept,omitempty"`
}

// Kind returns the ownership kind a resource step works on, or "" for layers.
func (s Step) Kind() string {
	switch {
	case s.Of != "":
		return s.Of
	case s.Op == BuildLocal || s.Op == InstallLocal || s.Op == RemoveLocal:
		return state.Local
	case s.Op == Remove || s.Op == Install:
		if s.Stage == Packages {
			return state.Pacman
		}
		return state.AUR
	case s.Op == Disable || s.Op == Enable:
		if s.Stage.Root() {
			return state.SystemUnits
		}
		return state.UserUnits
	case s.Op == Leave || s.Op == Join:
		return state.Groups
	case s.Op == Mkdir || s.Op == Rmdir:
		return state.HomeDirs
	}
	return ""
}

// Input is what both halves are planned from.
type Input struct {
	// Desired is the resolved desired modules in stack order.
	Desired   []string
	Modules   module.Set
	Protected []string
	// Rebuild forces these own builds to be built and installed again.
	Rebuild []string
}

// Declares returns what each desired module lists for kind.
func (in Input) Declares(kind string) map[string][]string {
	out := make(map[string][]string, len(in.Desired))
	for _, name := range in.Desired {
		m := in.Modules[name]
		if m == nil {
			continue
		}
		switch kind {
		case state.Pacman:
			out[name] = m.Packages.Pacman
		case state.AUR:
			out[name] = m.Packages.AUR
		case state.Local:
			out[name] = m.Packages.Local
		case state.SystemUnits:
			out[name] = m.Units.System
		case state.UserUnits:
			out[name] = m.Units.User
		case state.Groups:
			out[name] = m.Groups
		case state.HomeDirs:
			out[name] = m.Dirs
		}
	}
	return out
}

// protected applies only to packages.
func (in Input) protected(kind string) []string {
	if kind == state.Pacman || kind == state.AUR || kind == state.Local {
		return in.Protected
	}
	return nil
}

// Removals returns what reconciling kind would remove, without changing st.
func (in Input) Removals(st *state.State, kind string, present map[string]bool) []string {
	return st.Clone().Reconcile(kind, in.Desired, in.Declares(kind), in.protected(kind), present)
}

// Reconcile updates st's ownership record for kind as if the removals it
// returns were carried out.
func (in Input) Reconcile(st *state.State, kind string, present map[string]bool) []string {
	return st.Reconcile(kind, in.Desired, in.Declares(kind), in.protected(kind), present)
}

// Half is the current situation of one half.
type Half struct {
	State     *state.State
	Layer     *layer.Stacker
	Installed map[string]bool // installed packages
	// RequiredBy lists, for the installed packages this half owns, the
	// installed packages that depend on each.
	RequiredBy map[string][]string
	Units      map[string]bool // enabled units of this half
	Groups     map[string]bool // system half: the main user's groups
	Dirs       map[string]bool // user half: home directories that exist

	// Conflicts checks need these, when the half installs packages: every
	// installed package (Packages), and the packages to install (Incoming).
	// An own build not built yet may be missing from Incoming in the root
	// process; it is only installed in a later root stage, after the build.
	Packages, Incoming map[string]system.Package
}

// System plans the root half: pacman packages, system files, system units
// and groups.
func System(in Input, h Half) ([]Step, error) {
	var steps []Step
	steps = append(steps, removals(in, h.State, Leave, SystemTeardown, state.Groups, h.Groups)...)
	steps = append(steps, removals(in, h.State, Disable, SystemTeardown, state.SystemUnits, h.Units)...)
	layers, err := layerSteps(in, h, SystemTeardown, SystemSetup)
	if err != nil {
		return nil, err
	}
	steps = append(steps, layers.off...)
	steps = append(steps, packageRemovals(in, h, Remove, Packages, state.Pacman)...)
	steps = append(steps, packageRemovals(in, h, RemoveLocal, Packages, state.Local)...)
	steps = append(steps, additions(in, h.State, Install, Packages, state.Pacman, h.Installed)...)
	local, err := localInstalls(in, h)
	if err != nil {
		return nil, err
	}
	steps = append(steps, local...)
	steps = append(steps, layers.on...)
	steps = append(steps, additions(in, h.State, Enable, SystemSetup, state.SystemUnits, h.Units)...)
	steps = append(steps, additions(in, h.State, Join, SystemSetup, state.Groups, h.Groups)...)
	return steps, nil
}

// User plans the user half: AUR packages, home files and user units.
func User(in Input, h Half) ([]Step, error) {
	var steps []Step
	steps = append(steps, removals(in, h.State, Disable, UserTeardown, state.UserUnits, h.Units)...)
	layers, err := layerSteps(in, h, UserTeardown, UserSetup)
	if err != nil {
		return nil, err
	}
	steps = append(steps, layers.off...)
	// After the layers, so a directory that held only deployed files is empty.
	steps = append(steps, removals(in, h.State, Rmdir, UserTeardown, state.HomeDirs, h.Dirs)...)
	steps = append(steps, packageRemovals(in, h, Remove, AURRemove, state.AUR)...)
	steps = append(steps, additions(in, h.State, Install, AURInstall, state.AUR, h.Installed)...)
	steps = append(steps, additions(in, h.State, Mkdir, UserSetup, state.HomeDirs, h.Dirs)...)
	steps = append(steps, layers.on...)
	steps = append(steps, additions(in, h.State, Enable, UserSetup, state.UserUnits, h.Units)...)
	return steps, nil
}

// Merge orders the steps of both halves by stage, keeping the order within
// a stage. Every own build the system half installs is built before, as the
// user, in the build stage.
func Merge(system, user []Step) []Step {
	steps := append(slices.Clone(system), user...)
	for _, s := range system {
		if s.Op == InstallLocal {
			steps = append(steps, Step{Stage: Build, Op: BuildLocal, Module: s.Module, Names: s.Names, Recipe: s.Recipe})
		}
	}
	slices.SortStableFunc(steps, func(a, b Step) int { return int(a.Stage) - int(b.Stage) })
	return steps
}

// ByStage splits ordered steps into runs of the same stage.
func ByStage(steps []Step) [][]Step {
	var groups [][]Step
	for i := 0; i < len(steps); {
		j := i + 1
		for j < len(steps) && steps[j].Stage == steps[i].Stage {
			j++
		}
		groups = append(groups, steps[i:j])
		i = j
	}
	return groups
}

// Filter returns the steps of one stage.
func Filter(steps []Step, stage Stage) []Step {
	var out []Step
	for _, s := range steps {
		if s.Stage == stage {
			out = append(out, s)
		}
	}
	return out
}

// Hash identifies a list of steps. The root process recomputes its steps and
// refuses to run them if the hash differs from the one the user confirmed.
// Replacement lines ("! path") are left out: they describe files that an
// earlier stage of the same run may create, such as a package's default
// config that a module's file replaces, and the original is kept either way.
func Hash(steps []Step) string {
	hashed := make([]Step, len(steps))
	for i, s := range steps {
		s.Changes = slices.DeleteFunc(slices.Clone(s.Changes), func(c string) bool { return strings.HasPrefix(c, "! ") })
		hashed[i] = s
	}
	data, _ := json.Marshal(hashed) // plain strings, ints and bools can't fail to encode
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// removals plans what reconciling kind removes, one step per owner so units
// are stopped according to their own module's `now`. When nothing is
// removed but resources move to another module, a hand-over step records
// that; a removal step records it along with the removal.
func removals(in Input, st *state.State, op string, stage Stage, kind string, present map[string]bool) []Step {
	remove := in.Removals(st, kind, present)
	if len(remove) == 0 {
		if moves := handOvers(in, st, kind, present); len(moves) > 0 {
			return []Step{{Stage: stage, Op: HandOver, Of: kind, Changes: moves}}
		}
		return nil
	}
	if op != Disable {
		return []Step{{Stage: stage, Op: op, Names: remove}}
	}

	byOwner := map[string][]string{}
	for owner, names := range st.Owned[kind] {
		for _, n := range names {
			if slices.Contains(remove, n) && !slices.Contains(byOwner[owner], n) {
				byOwner[owner] = append(byOwner[owner], n)
			}
		}
	}
	var steps []Step
	for _, owner := range slices.Sorted(maps.Keys(byOwner)) {
		now := false
		if m := in.Modules[owner]; m != nil {
			now = m.Units.Now
		}
		steps = append(steps, Step{Stage: stage, Op: op, Module: owner, Names: byOwner[owner], Now: now})
	}
	return steps
}

// packageRemovals plans the removal of the packages of kind that
// reconciling releases. Those that an installed package outside the removal
// still needs stay, marked as dependencies, so pacman removes them with the
// last package that needs them; removing them would fail the whole
// transaction.
func packageRemovals(in Input, h Half, op string, stage Stage, kind string) []Step {
	steps := removals(in, h.State, op, stage, kind, h.Installed)
	if len(steps) != 1 || steps[0].Op != op {
		return steps // nothing to remove, or only a hand-over
	}
	s := &steps[0]
	kept := Needed(s.Names, h.RequiredBy)
	if len(kept) > 0 {
		s.Names = slices.DeleteFunc(s.Names, func(n string) bool { return slices.Contains(kept, n) })
		s.Kept = kept
		for _, n := range kept {
			s.Changes = append(s.Changes, fmt.Sprintf("= %s: needed by %s", n, strings.Join(h.RequiredBy[n], " ")))
		}
	}
	return steps
}

// Needed returns the packages of remove that an installed package needs
// which isn't being removed, directly or through another needed one.
func Needed(remove []string, requiredBy map[string][]string) []string {
	needed := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, n := range remove {
			if needed[n] {
				continue
			}
			for _, by := range requiredBy[n] {
				if !slices.Contains(remove, by) || needed[by] {
					needed[n], changed = true, true
					break
				}
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(remove), func(n string) bool { return !needed[n] })
}

// Conflicts adds, before the packages a root stage installs, the removal of
// the installed packages they conflict with: pacman with --noconfirm
// refuses to replace them itself. A conflict counts when either package
// names the other, or something it provides, without a version
// constraint; one with a constraint is left to pacman, which then refuses
// rather than having arcus remove a package that may not conflict. It is
// an error if a desired module lists the conflicting package, or it is
// protected.
func Conflicts(in Input, h Half, steps []Step) ([]Step, error) {
	listed := map[string]string{}
	for _, kind := range []string{state.Pacman, state.AUR, state.Local} {
		for _, name := range in.Desired {
			for _, n := range in.Declares(kind)[name] {
				if listed[n] == "" {
					listed[n] = name
				}
			}
		}
	}
	gone := map[string]bool{}
	for _, s := range steps {
		if s.Op == Remove || s.Op == RemoveLocal {
			for _, n := range s.Names {
				gone[n] = true
			}
		}
	}

	var out []Step
	for i, s := range steps {
		installs := (s.Op == Install && s.Stage == Packages) || s.Op == InstallLocal
		if !installs || (i > 0 && steps[i-1].Op == s.Op && steps[i-1].Stage == s.Stage) {
			out = append(out, s)
			continue
		}
		var targets []string
		for _, t := range steps[i:] {
			if t.Op != s.Op || t.Stage != s.Stage {
				break
			}
			targets = append(targets, t.Names...)
		}
		slices.Sort(targets)
		var names, changes []string
		for _, q := range slices.Sorted(maps.Keys(h.Packages)) {
			if gone[q] {
				continue
			}
			for _, t := range targets {
				p, ok := h.Incoming[t]
				if !ok || t == q || !(conflicts(p, q, h.Packages[q]) || conflicts(h.Packages[q], t, p)) {
					continue
				}
				if module := listed[q]; module != "" {
					return nil, fmt.Errorf("%s conflicts with %s, which module %s lists", t, q, module)
				}
				if slices.Contains(in.Protected, q) {
					return nil, fmt.Errorf("%s conflicts with %s, which is protected", t, q)
				}
				names = append(names, q)
				line := fmt.Sprintf("x %s: conflicts with %s", q, t)
				if by := h.Packages[q].RequiredBy; len(by) > 0 {
					line += fmt.Sprintf("; needed by %s", strings.Join(by, " "))
				}
				changes = append(changes, line)
				gone[q] = true
				break
			}
		}
		if len(names) > 0 {
			out = append(out, Step{Stage: s.Stage, Op: RemoveConflicting, Names: names, Changes: changes})
		}
		out = append(out, s)
	}
	return out, nil
}

// conflicts reports whether p, by its conflicts, rules out the package
// named other.
func conflicts(p system.Package, other string, o system.Package) bool {
	for _, c := range p.Conflicts {
		if strings.ContainsAny(c, "<>=") {
			continue
		}
		if c == other || slices.ContainsFunc(o.Provides, func(prov string) bool {
			name, _, _ := strings.Cut(prov, "=")
			return name == c
		}) {
			return true
		}
	}
	return false
}

// handOvers lists the resources of kind that reconciling moves to another
// module, as "<name>: <old> → <new>".
func handOvers(in Input, st *state.State, kind string, present map[string]bool) []string {
	after := st.Clone()
	in.Reconcile(after, kind, present)
	var moves []string
	for _, owner := range slices.Sorted(maps.Keys(st.Owned[kind])) {
		for _, n := range st.Owned[kind][owner] {
			for to, names := range after.Owned[kind] {
				if to != owner && slices.Contains(names, n) {
					moves = append(moves, fmt.Sprintf("%s: %s → %s", n, owner, to))
				}
			}
		}
	}
	return moves
}

// additions plans what is declared but not present, recorded for the first
// desired module that declares it. What is declared and already present but
// recorded for no module is taken over (Adopt), so a system set up by hand
// ends up recorded like one arcus installed. Protected packages are
// never removed, so there is nothing to take over.
func additions(in Input, st *state.State, op string, stage Stage, kind string, present map[string]bool) []Step {
	declares := in.Declares(kind)
	owned := map[string]bool{}
	for _, names := range st.Owned[kind] {
		for _, n := range names {
			owned[n] = true
		}
	}
	covered := map[string]bool{}
	var adopt, steps []Step
	for _, name := range in.Desired {
		var missing, taken []string
		for _, n := range declares[name] {
			if covered[n] {
				continue
			}
			covered[n] = true
			switch {
			case !present[n]:
				missing = append(missing, n)
			case !owned[n] && !slices.Contains(in.protected(kind), n):
				taken = append(taken, n)
			}
		}
		if len(taken) > 0 {
			adopt = append(adopt, Step{Stage: stage, Op: Adopt, Of: kind, Module: name, Names: taken})
		}
		if len(missing) > 0 {
			s := Step{Stage: stage, Op: op, Module: name, Names: missing}
			if op == Enable {
				s.Now = in.Modules[name].Units.Now
			}
			steps = append(steps, s)
		}
	}
	return append(adopt, steps...)
}

// localInstalls plans one install per own build that is missing, was built
// from a different recipe, or is to be rebuilt.
func localInstalls(in Input, h Half) ([]Step, error) {
	var steps []Step
	covered := map[string]bool{}
	for _, name := range in.Desired {
		m := in.Modules[name]
		if m == nil {
			continue
		}
		for _, pkg := range m.Packages.Local {
			if covered[pkg] {
				continue
			}
			covered[pkg] = true
			hash, err := module.RecipeHash(m.RecipeDir(pkg))
			if err != nil {
				return nil, err
			}
			if h.Installed[pkg] && h.State.Built[pkg] == hash && !slices.Contains(in.Rebuild, pkg) {
				continue
			}
			steps = append(steps, Step{Stage: LocalPackages, Op: InstallLocal, Module: name, Names: []string{pkg}, Recipe: hash})
		}
	}
	return steps, nil
}

type layerPlan struct{ off, on []Step }

// layerSteps plans the layers of one half. Layers that are no longer
// desired go off, top first. Then the stack is brought to the desired order:
// from the lowest kept layer whose module changed or that sits out of order,
// every layer goes off and the desired ones come on again in order (a
// restack); new layers above all kept ones just go on top.
func layerSteps(in Input, h Half, teardown, setup Stage) (layerPlan, error) {
	var p layerPlan
	var kept []string
	for i := len(h.State.Modules) - 1; i >= 0; i-- {
		name := h.State.Modules[i]
		if slices.Contains(in.Desired, name) {
			kept = append([]string{name}, kept...)
		} else {
			p.off = append(p.off, Step{Stage: teardown, Op: LayerOff, Module: name})
		}
	}

	// A module only gets a layer in a half it has a tree for, so a module
	// with home files alone never needs sudo for an empty system layer.
	var target []string
	for _, name := range in.Desired {
		if slices.Contains(kept, name) || h.Layer.HasTree(name) {
			target = append(target, name)
		}
	}
	from := len(kept)
	for i, name := range kept {
		if target[i] != name {
			from = i
			break
		}
	}

	// freed holds the paths that the steps before a new layer leave empty:
	// what the layers going off deploy and what changed modules dropped.
	freed := map[string]bool{}
	for _, s := range p.off {
		if err := h.Layer.Frees(s.Module, nil, freed); err != nil {
			return p, err
		}
	}
	var changes []string
	for i, name := range kept {
		stale, err := h.Layer.Stale(name)
		if err != nil {
			return p, err
		}
		var dropped []string
		for _, c := range stale {
			line := fmt.Sprintf("%c %s", c.Op, c.Host)
			if _, err := os.Lstat(c.Host); c.Op == '+' && err == nil && !freed[c.Host] {
				line = fmt.Sprintf("! %s (the file there is kept as a backup)", c.Host)
			}
			changes = append(changes, line)
			if c.Op == '-' {
				dropped = append(dropped, c.Host)
			}
		}
		if len(stale) > 0 {
			from = min(from, i)
		}
		if err := h.Layer.Frees(name, dropped, freed); err != nil {
			return p, err
		}
	}

	// New layers are previewed against the layers below them, which runs
	// the pre-flight checks now and shows which existing files get
	// replaced. Kept layers above a new one are off when it goes on.
	for j := from; j < len(target); j++ {
		name := target[j]
		if slices.Contains(kept, name) {
			continue
		}
		free := maps.Clone(freed)
		for _, above := range target[j+1:] {
			if slices.Contains(kept[from:], above) {
				if err := h.Layer.Frees(above, nil, free); err != nil {
					return p, err
				}
			}
		}
		entries, err := h.Layer.Preview(name, target[:j], free)
		if err != nil {
			return p, err
		}
		var replaced []string
		for _, e := range entries {
			if e.Backup != "" {
				replaced = append(replaced, fmt.Sprintf("! %s (kept as %s)", e.Host, filepath.Base(e.Backup)))
			}
		}
		if from < len(kept) {
			changes = append(changes, replaced...)
		} else {
			p.on = append(p.on, Step{Stage: setup, Op: LayerOn, Module: name, Changes: replaced})
		}
	}
	if from < len(kept) {
		p.on = []Step{{Stage: setup, Op: Restack, Module: kept[from], Names: target[from:], Changes: changes}}
	}
	return p, nil
}

// Installed is what pacman reports about the installed packages.
type Installed struct {
	All, Explicit, Unrequired map[string]bool
}

// Clean plans arcus clean: packages that a desired module lists, or that
// are protected, become explicitly installed; others that an installed
// package needs become dependencies; the rest is removed, with the
// dependencies nothing needs any more.
func Clean(in Input, p Installed) []Step {
	keep := map[string]bool{}
	for _, kind := range []string{state.Pacman, state.AUR, state.Local} {
		for _, names := range in.Declares(kind) {
			for _, n := range names {
				keep[n] = true
			}
		}
	}
	for _, n := range in.Protected {
		keep[n] = true
	}

	var explicit, deps, remove []string
	for _, n := range slices.Sorted(maps.Keys(p.All)) {
		switch {
		case keep[n] && !p.Explicit[n]:
			explicit = append(explicit, n)
		case keep[n]:
		case p.Unrequired[n]:
			remove = append(remove, n)
		case p.Explicit[n]:
			deps = append(deps, n)
		}
	}
	var steps []Step
	for _, s := range []Step{{Op: MarkExplicit, Names: explicit}, {Op: MarkDeps, Names: deps}, {Op: RemoveUnlisted, Names: remove}} {
		if len(s.Names) > 0 {
			s.Stage = Packages
			steps = append(steps, s)
		}
	}
	return steps
}
