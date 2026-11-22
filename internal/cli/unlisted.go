package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/system"
)

// unlistedHeader opens a module that 'arcus new -unlisted' writes.
const unlistedHeader = `# Written by 'arcus new -unlisted' from what this machine had that no
# module that was on listed. Turning this module off removes everything
# it lists, so split it into real modules. A package in aur that isn't on
# the AUR (built by hand) needs an own build in pkg/ or deleting from the
# list; a unit you wrote yourself needs its file in this module's system/
# or home/, or deleting from the list. Both work here, but not on
# another machine.
`

// unlisted is what this machine has that no module that's on lists: what
// 'arcus new -unlisted' writes into a module.
type unlisted struct {
	pacman, aur  []string
	system, user []string // units
	groups       []string
	leftOut      []string // names a manifest can't hold
	machineOn    bool
}

// unlisted collects what no module that's on lists: explicitly installed
// packages, units enabled outside their vendor preset and the user's
// groups, as doctor reports them. Without a host file nothing is on.
func (a *app) unlisted() (*unlisted, error) {
	in, err := a.selectionOrNone()
	if err != nil {
		return nil, err
	}
	explicit, err := system.Explicit()
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	foreign, err := system.Foreign()
	if err != nil {
		return nil, fmt.Errorf("querying packages: %w", err)
	}
	u := &unlisted{machineOn: slices.Contains(in.Desired, machine.Name)}
	listed := in.Listed()
	var pacman, aur []string
	for _, n := range slices.Sorted(maps.Keys(explicit)) {
		switch {
		case listed[n]:
		case foreign[n]:
			aur = append(aur, n)
		default:
			pacman = append(pacman, n)
		}
	}
	u.pacman = u.valid("packages.pacman", pacman)
	u.aur = u.valid("packages.aur", aur)

	for _, user := range []bool{false, true} {
		units, err := a.unlistedUnits(in, user)
		if err != nil {
			return nil, fmt.Errorf("querying units: %w", err)
		}
		field, list := "units.system", &u.system
		if user {
			field, list = "units.user", &u.user
		}
		*list = collapseAlso(a.unitDirs(user), u.valid(field, units))
	}

	groups, err := a.unlistedGroups(in)
	if err != nil {
		return nil, fmt.Errorf("querying groups: %w", err)
	}
	u.groups = u.valid("groups", groups)
	return u, nil
}

// valid returns the names field accepts and keeps the rest as left out.
func (u *unlisted) valid(field string, names []string) []string {
	ok, bad := module.ValidNames(field, names)
	u.leftOut = append(u.leftOut, bad...)
	return ok
}

func (u *unlisted) empty() bool {
	return len(u.pacman)+len(u.aur)+len(u.system)+len(u.user)+len(u.groups) == 0
}

// manifest renders the module.toml, in the order of moduleTemplate. Names
// are TOML literal strings: the ones ValidNames accepts hold no quote or
// newline, and unit names keep their backslash escapes as they are.
func (u *unlisted) manifest() string {
	list := func(names []string) string {
		if len(names) == 0 {
			return "[]"
		}
		var b strings.Builder
		b.WriteString("[\n")
		for _, n := range names {
			fmt.Fprintf(&b, "  '%s',\n", n)
		}
		b.WriteString("]")
		return b.String()
	}
	return unlistedHeader + `description = "Taken over by arcus new -unlisted"
category  = ""

requires  = []
conflicts = []
groups    = ` + list(u.groups) + `
dirs      = []

[packages]
pacman = ` + list(u.pacman) + `
aur    = ` + list(u.aur) + `
local  = []

[units]
system = ` + list(u.system) + `
user   = ` + list(u.user) + `
now    = false
`
}

// collapseAlso drops each unit that another one in units enables with it
// through Also=, so a module lists NetworkManager.service and not the
// units it brings along. Of units that enable each other, the first in
// sort order stays. The result doesn't depend on the order of units.
func collapseAlso(dirs, units []string) []string {
	reach := map[string]map[string]bool{}
	for _, u := range units {
		r := map[string]bool{}
		for _, x := range system.WithAlso(dirs, []string{u})[1:] {
			r[x], r[system.Template(x)] = true, true
		}
		reach[u] = r
	}
	return slices.DeleteFunc(slices.Clone(units), func(u string) bool {
		for _, v := range units {
			if v != u && reach[v][u] && (!reach[u][v] || v < u) {
				return true
			}
		}
		return false
	})
}

// selectionOrNone is the host file's selection, or nothing on when there
// is no host file yet.
func (a *app) selectionOrNone() (plan.Input, error) {
	path, err := a.hostPath()
	if err != nil {
		return plan.Input{}, err
	}
	f, err := host.Load(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return a.input(nil)
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
