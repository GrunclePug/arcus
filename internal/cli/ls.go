package cli

import (
	"cmp"
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/profile"
)

// cmdLs lists every profile and every module, modules grouped by category
// (module.CompareCategories). For a module it shows whether it is on (or
// missing: files deployed, directory gone), its rank in the stack, what
// selected it (a profile, the host file, or another module's requires), which
// halves have its files deployed, and what it requires.
func (a *app) cmdLs(args []string) error {
	var o opts
	fs := a.flags("ls", &o, false, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := a.load(&o); err != nil {
		return err
	}

	f := &host.File{}
	path, err := a.hostPath(&o)
	if err != nil {
		return err
	}
	switch loaded, err := host.Load(path); {
	case errors.Is(err, iofs.ErrNotExist):
		fmt.Fprintf(a.stderr, "no host file at %s; nothing is on\n", path)
	case err != nil:
		return err
	default:
		f = loaded
	}

	selected, via, err := a.profiles.Expand(f.Profiles, f.Modules)
	if err != nil {
		fmt.Fprintf(a.stderr, "host file: %v\n", err)
	}
	desired, err := a.modules.Resolve(selected)
	if err != nil {
		fmt.Fprintf(a.stderr, "host file: %v\n", err)
		desired = selected
	}

	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	if len(a.profiles) > 0 {
		fmt.Fprintln(w, "PROFILE\tSTATUS\tMODULES")
		for _, name := range slices.Sorted(maps.Keys(a.profiles)) {
			status := "off"
			if slices.Contains(f.Profiles, name) {
				status = "on"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", name, status, strings.Join(a.profiles[name].Modules, ","))
		}
		fmt.Fprintln(w)
	}

	layers := map[string][]string{}
	for _, h := range []*half{a.userHalf(), a.systemHalf()} {
		st, err := h.loadState()
		if err != nil {
			return err
		}
		for _, name := range st.Modules {
			layers[name] = append(layers[name], h.layer.Subdir)
		}
	}

	names := slices.Collect(maps.Keys(a.modules))
	for name := range layers {
		if a.modules[name] == nil {
			names = append(names, name)
		}
	}
	category := func(name string) string {
		if m := a.modules[name]; m != nil {
			return m.Category
		}
		return ""
	}
	slices.SortFunc(names, func(x, y string) int {
		return cmp.Or(module.CompareCategories(category(x), category(y)), cmp.Compare(x, y))
	})

	fmt.Fprintln(w, "CATEGORY\tMODULE\tSTATUS\tRANK\tVIA\tFILES\tREQUIRES")
	for _, name := range names {
		status, rank, from, requires := "off", "-", "-", "-"
		m := a.modules[name]
		switch {
		case m == nil:
			status = "missing"
		case len(m.Requires) > 0:
			requires = strings.Join(m.Requires, ",")
		}
		if i := slices.Index(desired, name); i >= 0 {
			status, rank = "on", strconv.Itoa(i+1)
			switch p, ok := via[name]; {
			case !ok:
				from = "requires"
			case p == profile.Host:
				from = "host"
			default:
				from = p
			}
		}
		files := "-"
		if len(layers[name]) > 0 {
			files = strings.Join(layers[name], ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cmp.Or(category(name), "-"), name, status, rank, from, files, requires)
	}
	return w.Flush()
}
