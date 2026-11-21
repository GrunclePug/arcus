package module

import (
	"fmt"
	"maps"
	"slices"
)

// Resolve expands names with everything they require, directly or
// transitively, and orders the result so every module comes after its
// requirements. Apart from that the order of names is kept, since it is the
// stacking order. It fails on unknown modules, cycles and conflicts, and on
// AppImages that clash.
func (s Set) Resolve(names []string) ([]string, error) {
	order, err := s.Order(names)
	if err != nil {
		return nil, err
	}
	if err := s.appImageClashes(order); err != nil {
		return nil, err
	}
	return order, nil
}

// Order is Resolve without the AppImage checks, for a selection that is
// being taken apart: AppImages that came to clash in two modules that are
// on mustn't keep either from being turned off.
func (s Set) Order(names []string) ([]string, error) {
	order := []string{}
	visited := map[string]bool{}
	visiting := map[string]bool{}

	var visit func(name, from string) error
	visit = func(name, from string) error {
		if visiting[name] {
			return fmt.Errorf("circular requirement at module %q", name)
		}
		if visited[name] {
			return nil
		}
		m, ok := s[name]
		if !ok {
			if from != "" {
				return fmt.Errorf("module %q requires %q, which does not exist", from, name)
			}
			return fmt.Errorf("module %q does not exist", name)
		}

		visiting[name] = true
		for _, req := range m.Requires {
			if err := visit(req, name); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for _, name := range names {
		if err := visit(name, ""); err != nil {
			return nil, err
		}
	}

	for i, a := range order {
		for _, b := range order[i+1:] {
			if slices.Contains(s[a].Conflicts, b) || slices.Contains(s[b].Conflicts, a) {
				return nil, fmt.Errorf("modules %q and %q conflict", a, b)
			}
		}
	}
	return order, nil
}

// appImageClashes rejects two modules declaring an AppImage of the same name
// or at the same path (or one inside the other), and an AppImage where a
// module's home files, links or dirs go.
func (s Set) appImageClashes(order []string) error {
	names, paths := map[string]string{}, map[string]string{}
	for _, m := range order {
		for _, name := range slices.Sorted(maps.Keys(s[m].AppImages)) {
			p := s[m].AppImages[name].Path
			if other, ok := names[name]; ok {
				return fmt.Errorf("modules %q and %q both declare AppImage %q", other, m, name)
			}
			for _, q := range slices.Sorted(maps.Keys(paths)) {
				if paths[q] != m && nested(p, q) {
					if p == q {
						return fmt.Errorf("modules %q and %q both put an AppImage at ~/%s", paths[q], m, p)
					}
					return fmt.Errorf("modules %q and %q put AppImages at ~/%s and ~/%s, one inside the other", paths[q], m, q, p)
				}
			}
			names[name], paths[p] = m, m
			for _, other := range order {
				if what := s[other].claims(p); what != "" {
					return fmt.Errorf("module %q puts an AppImage at ~/%s, but module %q %s", m, p, other, what)
				}
			}
		}
	}
	return nil
}

// Dependents returns every module in stack that requires target, directly
// or transitively, in stack order. Teardown must remove them (in reverse)
// before target. Chains through modules that are not in the stack are still
// followed; modules missing from s are skipped.
func (s Set) Dependents(target string, stack []string) []string {
	var dependsOn func(name string, seen map[string]bool) bool
	dependsOn = func(name string, seen map[string]bool) bool {
		if seen[name] || s[name] == nil {
			return false
		}
		seen[name] = true
		for _, req := range s[name].Requires {
			if req == target || dependsOn(req, seen) {
				return true
			}
		}
		return false
	}

	var out []string
	for _, name := range stack {
		if name != target && dependsOn(name, map[string]bool{}) {
			out = append(out, name)
		}
	}
	return out
}
