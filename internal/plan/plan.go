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
// Own builds that need other pending own builds alternate between Build
// and LocalPackages, one pair per batch (see Local). Each run of a root
// stage is one sudo call.
type Stage int

const (
	UserTeardown   Stage = iota + 1 // user units disabled, home layers off
	SystemTeardown                  // groups left, system units disabled, system layers off
	AURRemove                       // AUR packages removed
	Packages                        // pacman packages and own builds removed, pacman packages installed
	Build                           // own builds of one batch made from their recipes (makepkg, as the user)
	LocalPackages                   // own builds of one batch installed
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

	HandOver = "hand-over" // ownership moves to the modules that list the resources now, or is dropped
	Adopt    = "adopt"     // resources already there become recorded for the module that lists them
	Mkdir    = "mkdir"     // home directories
	Rmdir    = "rmdir"
	Link     = "link" // home links
	Unlink   = "unlink"

	// Packages taken over that were installed as dependencies become
	// explicitly installed, like the ones arcus installs.
	MarkAdopted = "mark-adopted"

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
	case s.Op == Link || s.Op == Unlink:
		return state.HomeLinks
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
		case state.HomeLinks:
			out[name] = m.LinkNames()
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
	Links      map[string]bool // user half: home links that point at their target

	// Conflicts checks need these, when the half installs packages: every
	// installed package (Packages), and the packages to install (Incoming).
	// An own build not built yet may be missing from Incoming in the root
	// process; it is only installed in a later root stage, after the build.
	Packages, Incoming map[string]system.Package

	// Marking the packages taken over needs these, in the system half: the
	// user half's state, for the AUR packages it takes over, and the
	// packages installed explicitly. Each is nil when it can't matter.
	User     *state.State
	Explicit map[string]bool
}

// System plans the root half: pacman packages, system files, system units
// and groups.
func System(in Input, h Half) ([]Step, error) {
	var steps []Step
	steps = append(steps, removals(in, h.State, Leave, SystemTeardown, state.Groups, h.Groups)...)
	steps = append(steps, removals(in, h.State, Disable, SystemTeardown, state.SystemUnits, h.Units)...)
	layers, err := layerSteps(in, h, SystemTeardown, SystemSetup, nil)
	if err != nil {
		return nil, err
	}
	steps = append(steps, layers.off...)
	steps = append(steps, packageRemovals(in, h, Remove, Packages, state.Pacman)...)
	steps = append(steps, packageRemovals(in, h, RemoveLocal, Packages, state.Local)...)
	steps = append(steps, additions(in, h.State, Install, Packages, state.Pacman, h.Installed)...)
	if names := slices.DeleteFunc(Adopted(in, h), func(n string) bool { return h.Explicit[n] }); len(names) > 0 {
		steps = append(steps, Step{Stage: Packages, Op: MarkAdopted, Names: names})
	}
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
	// The links removed leave their paths free for the layers, which come
	// on after them.
	unlinks := removals(in, h.State, Unlink, UserTeardown, state.HomeLinks, h.Links)
	var unlinked []string
	for _, s := range unlinks {
		if s.Op == Unlink {
			for _, n := range s.Names {
				if p, _, ok := module.SplitLink(n); ok {
					unlinked = append(unlinked, filepath.Join(h.Layer.Target, p))
				}
			}
		}
	}
	layers, err := layerSteps(in, h, UserTeardown, UserSetup, unlinked)
	if err != nil {
		return nil, err
	}
	steps = append(steps, layers.off...)
	// After the layers, so a directory that held only deployed files is empty.
	steps = append(steps, unlinks...)
	steps = append(steps, removals(in, h.State, Rmdir, UserTeardown, state.HomeDirs, h.Dirs)...)
	steps = append(steps, packageRemovals(in, h, Remove, AURRemove, state.AUR)...)
	steps = append(steps, additions(in, h.State, Install, AURInstall, state.AUR, h.Installed)...)
	steps = append(steps, additions(in, h.State, Mkdir, UserSetup, state.HomeDirs, h.Dirs)...)
	steps = append(steps, additions(in, h.State, Link, UserSetup, state.HomeLinks, h.Links)...)
	steps = append(steps, layers.on...)
	steps = append(steps, additions(in, h.State, Enable, UserSetup, state.UserUnits, h.Units)...)
	return steps, nil
}

// Merge orders the steps of both halves by stage, keeping the order within
// a stage. Local then adds the builds of the own builds it installs.
func Merge(system, user []Step) []Step {
	steps := append(slices.Clone(system), user...)
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
// So is what needs a conflicting package ("; needed by"): an own build
// installed by an earlier batch of the same run may need it too, and the
// names removed are hashed either way.
func Hash(steps []Step) string {
	hashed := make([]Step, len(steps))
	for i, s := range steps {
		var changes []string
		for _, c := range s.Changes {
			if strings.HasPrefix(c, "! ") {
				continue
			}
			if s.Stage == LocalPackages && strings.HasPrefix(c, "x ") {
				c, _, _ = strings.Cut(c, neededBy)
			}
			changes = append(changes, c)
		}
		s.Changes = changes
		hashed[i] = s
	}
	data, _ := json.Marshal(hashed) // plain strings, ints and bools can't fail to encode
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// removals plans what reconciling kind removes, one step per owner so units
// are stopped according to their own module's `now`. When nothing is
// removed but the record still changes (resources move to another module,
// or are released while already gone or protected), a hand-over step
// records that; a removal step records it along with the removal.
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
					line += neededBy + strings.Join(by, " ")
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

const neededBy = "; needed by "

// Local replaces the own build steps of merged steps with batches: the
// builds of a batch, then their installs with the removal of what they
// conflict with. A build waits for an earlier batch to install what its
// BuildDepends need from the pending own builds, so makepkg finds it
// installed; an install shares the batch of what its Depends need, or
// comes later, so pacman finds it installed or in the same transaction.
// Unrelated builds share a batch.
//
// Batches are planned on h as the earlier stages and batches leave it.
// h.Incoming must hold every own build to install, with BuildDepends for
// the ones that will be built (from SrcInfo); one already built from its
// recipe is only installed. It is an error if the builds need each other
// in a cycle.
func Local(in Input, h Half, steps []Step) ([]Step, error) {
	var local []Step
	build, after := len(steps), len(steps)
	for i, s := range steps {
		if s.Op == InstallLocal {
			local = append(local, s)
		}
		if s.Stage >= Build && build == len(steps) {
			build = i
		}
		if s.Stage > LocalPackages && after == len(steps) {
			after = i
		}
	}
	if len(local) == 0 {
		return steps, nil
	}

	packages := map[string]system.Package{}
	maps.Copy(packages, h.Packages)
	h.Packages = packages
	apply := func(steps []Step) {
		for _, s := range steps {
			switch s.Op {
			case Remove, RemoveLocal, RemoveConflicting:
				for _, n := range s.Names {
					delete(h.Packages, n)
				}
			case Install, InstallLocal:
				for _, n := range s.Names {
					h.Packages[n] = h.Incoming[n]
				}
			}
		}
	}
	apply(steps[:build])
	var ordered []Step
	for pending := local; len(pending) > 0; {
		batch, err := nextBatch(pending, h.Incoming, h.Packages)
		if err != nil {
			return nil, err
		}
		pending = slices.DeleteFunc(pending, func(s Step) bool {
			return slices.ContainsFunc(batch, func(b Step) bool { return b.Names[0] == s.Names[0] })
		})
		for _, s := range batch {
			ordered = append(ordered, Step{Stage: Build, Op: BuildLocal, Module: s.Module, Names: s.Names, Recipe: s.Recipe})
		}
		installs, err := Conflicts(in, h, batch)
		if err != nil {
			return nil, err
		}
		ordered = append(ordered, installs...)
		apply(installs)
	}
	kept := slices.DeleteFunc(slices.Clone(steps[:after]), func(s Step) bool { return s.Stage == LocalPackages })
	return slices.Concat(kept, ordered, steps[after:]), nil
}

// need is a dependency on pending own builds. Any one of of meets it. An
// install need that installed packages meet has rm, one list per installed
// satisfier of the pending builds that would remove it (never empty): it is
// also met while one satisfier has none of its removers in the batch.
type need struct {
	build bool // makepkg needs it installed before the build
	of    []string
	rm    [][]string
}

func (nd need) met(batch map[string]bool) bool {
	in := func(p string) bool { return batch[p] }
	return slices.ContainsFunc(nd.of, in) ||
		slices.ContainsFunc(nd.rm, func(removers []string) bool { return !slices.ContainsFunc(removers, in) })
}

// nextBatch returns the pending own build installs that can go in the next
// batch, in their order, with the packages installed by then.
//
// It works in pending order, so the result doesn't depend on map order.
// First, as before removals counted: the builds with no build need, less
// those whose needs on pending builds point outside the batch, until
// stable; a need an installed package meets counts as met. Then, one at a
// time, a build whose installed satisfier another member would remove, with
// the first step re-run after each. Last, a dropped build without build
// needs comes back when every member's needs stay met with it, and all of
// it repeats until nothing changes.
func nextBatch(pending []Step, incoming, installed map[string]system.Package) ([]Step, error) {
	needs := localNeeds(pending, incoming, installed)
	names := make([]string, len(pending))
	for i, s := range pending {
		names[i] = s.Names[0]
	}
	builds := func(n string) bool { return slices.ContainsFunc(needs[n], func(nd need) bool { return nd.build }) }
	batch := map[string]bool{}
	for _, n := range names {
		if !builds(n) {
			batch[n] = true
		}
	}
	unmet := func(n string, ignoreRemovals bool) bool {
		return slices.ContainsFunc(needs[n], func(nd need) bool {
			return !(ignoreRemovals && nd.rm != nil) && !nd.met(batch)
		})
	}
	settle := func() {
		for changed := true; changed; {
			changed = false
			for _, n := range names {
				if batch[n] && unmet(n, true) {
					delete(batch, n)
					changed = true
				}
			}
		}
	}
	for {
		settle()
		if i := slices.IndexFunc(names, func(n string) bool { return batch[n] && unmet(n, false) }); i >= 0 {
			delete(batch, names[i])
			continue
		}
		back := false
		for _, n := range names {
			if batch[n] || builds(n) {
				continue
			}
			batch[n] = true
			if slices.ContainsFunc(names, func(m string) bool { return batch[m] && unmet(m, false) }) {
				delete(batch, n)
				continue
			}
			back = true
		}
		if !back {
			break
		}
	}
	if len(batch) == 0 {
		return nil, fmt.Errorf("own builds need each other in a cycle: %s", strings.Join(cycle(pending, needs), " → "))
	}
	return slices.DeleteFunc(slices.Clone(pending), func(s Step) bool { return !batch[s.Names[0]] }), nil
}

// localNeeds lists what each pending own build needs from the other ones.
// A name the build provides itself is no install need, unless the
// dependency has a version, which arcus doesn't compare; to build, it needs
// the name installed like any other. A dependency on the name of a
// pending own build needs that one, even while an older version is
// installed: its recipe changed. Otherwise an installed satisfier (see
// satisfiers) meets a build need, and an install need too unless every
// installed satisfier has a pending remover: then it is met while one of
// them stays (see need). Without one, it needs one of the pending builds
// that provide the name, and with none of those either it is pacman's to
// resolve, as when everything was one transaction.
//
// It doesn't cover everything a batch boundary can break: a removal in a
// later batch can break an own build installed in an earlier one, a build
// can miss a package an earlier batch removed, two consumers that block
// each other's satisfier, or two builds that need each other, can be
// refused although another order works, and a versioned dependency waits
// for a pending provider even when the installed one would do.
func localNeeds(steps []Step, incoming, installed map[string]system.Package) map[string][]need {
	pending := map[string]bool{}
	for _, s := range steps {
		pending[s.Names[0]] = true
	}
	needs := map[string][]need{}
	for _, s := range steps {
		n := s.Names[0]
		self := incoming[n]
		add := func(deps []string, build bool) {
			for _, dep := range deps {
				name, versioned := dep, false
				if i := strings.IndexAny(dep, "<>="); i >= 0 {
					name, versioned = dep[:i], true
				}
				if name == n || !build && !versioned && provides(self, name) {
					continue
				}
				if pending[name] {
					needs[n] = append(needs[n], need{build: build, of: []string{name}})
					continue
				}
				of := slices.DeleteFunc(providers(incoming, name), func(p string) bool { return !pending[p] || p == n })
				// The build runs before the consumer's own install removes
				// anything, so only an install need loses those satisfiers.
				sat := satisfiers(installed, name, "", system.Package{})
				if !build {
					sat = satisfiers(installed, name, n, self)
				}
				switch {
				case versioned || len(sat) == 0:
					if len(of) > 0 {
						needs[n] = append(needs[n], need{build: build, of: of})
					}
				case !build:
					var rm [][]string
					for _, q := range sat {
						r := removers(steps, incoming, n, q, installed[q])
						if len(r) == 0 {
							rm = nil
							break
						}
						rm = append(rm, r)
					}
					if rm != nil {
						needs[n] = append(needs[n], need{of: of, rm: rm})
					}
				}
			}
		}
		add(self.BuildDepends, true)
		add(self.Depends, false)
	}
	return needs
}

// satisfiers returns the installed packages that are or provide name, less
// those that consumer, whose package is c, removes by its own install: a
// conflict either way, or the same name. Sorted.
func satisfiers(installed map[string]system.Package, name, consumer string, c system.Package) []string {
	var out []string
	for _, q := range slices.Sorted(maps.Keys(installed)) {
		p := installed[q]
		if q != name && !provides(p, name) {
			continue
		}
		if q == consumer || conflicts(c, q, p) || conflicts(p, consumer, c) {
			continue
		}
		out = append(out, q)
	}
	return out
}

// removers returns the pending builds other than consumer whose install
// removes the installed package q: a conflict either way, or the same name.
// Sorted.
func removers(steps []Step, incoming map[string]system.Package, consumer, q string, p system.Package) []string {
	var out []string
	for _, s := range steps {
		r := s.Names[0]
		if r != consumer && (r == q || conflicts(incoming[r], q, p) || conflicts(p, r, incoming[r])) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// provides reports whether p provides name, whatever the version.
func provides(p system.Package, name string) bool {
	return slices.ContainsFunc(p.Provides, func(prov string) bool {
		n, _, _ := strings.Cut(prov, "=")
		return n == name
	})
}

// providers returns the packages of pkgs that provide name, sorted.
func providers(pkgs map[string]system.Package, name string) []string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(pkgs)) {
		if provides(pkgs[n], name) {
			out = append(out, n)
		}
	}
	return out
}

// cycle returns one cycle among pending own builds none of which can go in
// a batch: each has a need that only other pending ones can meet or would
// break, so following the first one from any of them comes back to one
// already visited.
func cycle(pending []Step, needs map[string][]need) []string {
	var path []string
	for n := pending[0].Names[0]; ; {
		if i := slices.Index(path, n); i >= 0 {
			return append(path[i:], n)
		}
		path = append(path, n)
		if nd := needs[n][0]; len(nd.of) > 0 {
			n = nd.of[0]
		} else {
			n = nd.rm[0][0]
		}
	}
}

// CheckBatch refuses a root batch of own builds when one of them has no
// package to read, or when the batch would remove, before installing them,
// the only package that one of them depends on. The plan
// comes from .SRCINFO, which can miss a dependency the built package has
// (see system.SrcInfo); the hash then matches, and pacman -Rdd would succeed
// before pacman -U fails. It compares names only, so a soname dependency
// (libfoo.so=1-64) counts, and refuses only when nothing of any version
// provides the name after the batch. A dependency that no installed package
// meets before the batch, or only ones the build itself removes, is
// pacman's to resolve, as in localNeeds. It checks only the batch's own
// installs: an installed package that a same-name upgrade stops providing
// for is let through, as Conflicts lets "needed by" removals through.
//
// The refusal heals on the next run, which plans from the built package,
// unless the build is rebuilt (then the recipe has to list the dependency)
// or the dependency has a version and no own build provides it (then the
// next run plans the same batch, so a provider has to come first). Root sees
// only its batch, so the message names both ways.
// With nothing pending providing it, the next run delays the build behind
// the removal, where pacman decides.
func CheckBatch(h Half, steps []Step, rebuild []string) error {
	for _, s := range steps {
		if s.Op != InstallLocal {
			continue
		}
		for _, n := range s.Names {
			if _, ok := h.Incoming[n]; !ok {
				return fmt.Errorf("own build %s has no package to install; nothing was done, run the command again", n)
			}
		}
	}
	removed := map[string]bool{}
	var targets []string
	for _, s := range steps {
		switch s.Op {
		case RemoveConflicting:
			for _, n := range s.Names {
				removed[n] = true
			}
		case InstallLocal:
			targets = append(targets, s.Names...)
		}
	}
	after := map[string]system.Package{}
	for q, p := range h.Packages {
		if !removed[q] {
			after[q] = p
		}
	}
	for _, t := range targets {
		after[t] = h.Incoming[t] // a same-name upgrade replaces the old package
	}
	for _, t := range targets {
		p := h.Incoming[t]
		for _, dep := range p.Depends {
			name := dep
			if i := strings.IndexAny(dep, "<>="); i >= 0 {
				name = dep[:i]
			}
			if len(satisfiers(h.Packages, name, t, p)) == 0 {
				continue
			}
			if _, ok := after[name]; ok || len(providers(after, name)) > 0 {
				continue
			}
			switch {
			case slices.Contains(rebuild, t):
				return fmt.Errorf("%s needs %s, which this batch would remove, and its recipe doesn't list it: add it to depends in the PKGBUILD; this batch was not installed", t, dep)
			case name != dep:
				// A versioned need waits only for a pending provider, so with
				// none the next run plans the same batch.
				return fmt.Errorf("%s needs %s, which this batch would remove and nothing in it provides; this batch was not installed: run the command again if an own build provides %s, otherwise install a provider of it first", t, dep, name)
			}
			return fmt.Errorf("%s needs %s, which this batch would remove; this batch was not installed, run the command again", t, dep)
		}
	}
	return nil
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

// handOvers lists how reconciling kind changes its record without removing
// anything: a resource that moves to another module as "<name>: <old> →
// <new>", one that is dropped as "<name>: <old>, <why>, forgotten".
func handOvers(in Input, st *state.State, kind string, present map[string]bool) []string {
	after := st.Clone()
	in.Reconcile(after, kind, present)
	var moves []string
	for _, owner := range slices.Sorted(maps.Keys(st.Owned[kind])) {
		for _, n := range st.Owned[kind][owner] {
			if slices.Contains(after.Owned[kind][owner], n) {
				continue
			}
			to := ""
			for _, other := range slices.Sorted(maps.Keys(after.Owned[kind])) {
				if slices.Contains(after.Owned[kind][other], n) {
					to = other
					break
				}
			}
			switch {
			case to != "":
				moves = append(moves, fmt.Sprintf("%s: %s → %s", n, owner, to))
			case present != nil && !present[n]:
				moves = append(moves, fmt.Sprintf("%s: %s, already gone, forgotten", n, owner))
			default:
				moves = append(moves, fmt.Sprintf("%s: %s, protected and kept, forgotten", n, owner))
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

// Adopted returns the packages that the plan of the system half takes over,
// and those of the user half if h.User is set, sorted. Own builds are never
// taken over: they are installed again, as explicitly installed.
func Adopted(in Input, h Half) []string {
	steps := additions(in, h.State, Install, Packages, state.Pacman, h.Installed)
	if h.User != nil {
		steps = append(steps, additions(in, h.User, Install, AURInstall, state.AUR, h.Installed)...)
	}
	var names []string
	for _, s := range steps {
		if s.Op == Adopt {
			names = append(names, s.Names...)
		}
	}
	slices.Sort(names)
	return names
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
func layerSteps(in Input, h Half, teardown, setup Stage, unlinked []string) (layerPlan, error) {
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
	// the links removed, what the layers going off deploy and what changed
	// modules dropped.
	freed := map[string]bool{}
	for _, host := range unlinked {
		freed[host] = true
	}
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
				where := "kept as " + filepath.Base(e.Backup) // beside the host: the store is blocked
				if tag := h.Layer.BackupTag(e.Backup); tag != "" {
					where = "kept in arcus backups: " + tag
				}
				replaced = append(replaced, fmt.Sprintf("! %s (%s)", e.Host, where))
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
