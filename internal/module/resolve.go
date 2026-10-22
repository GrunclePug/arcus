package module

import (
	"fmt"
	"slices"
)

// Resolve expands names with everything they require, directly or
// transitively, and orders the result so every module comes after its
// requirements. Apart from that the order of names is kept, since it is the
// stacking order. It fails on unknown modules, cycles and conflicts.
func (s Set) Resolve(names []string) ([]string, error) {
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
