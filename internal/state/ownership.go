package state

import "slices"

// Added returns the names from wanted that are present in after but were
// absent in before, i.e. the ones this run actually added. It applies to
// every kind: packages installed, units enabled, groups joined.
func Added(wanted []string, before, after map[string]bool) []string {
	added := []string{}
	for _, n := range wanted {
		if !before[n] && after[n] {
			added = append(added, n)
		}
	}
	return added
}

// Removal is the outcome of Removals.
type Removal struct {
	// Remove is what may be uninstalled, disabled or left.
	Remove []string
	// Transfer moves ownership of still-needed resources to another module
	// that is on, so they are removed when the last module using them goes.
	// Protected and absent resources are in neither list.
	Transfer map[string][]string
}

// Removals decides what turning a module off may remove.
//
//   - owned: resources arcus added for this module.
//   - order: the other modules that are on, in stack order. It fixes which
//     module inherits a shared resource.
//   - others: what each of those modules declares, keyed by name.
//   - protected: never removed.
//   - present: the current set, or nil if unknown. Resources that are already
//     gone are skipped, since pacman aborts the whole transaction on an
//     unknown target.
func Removals(owned, order []string, others map[string][]string, protected []string, present map[string]bool) Removal {
	r := Removal{Transfer: map[string][]string{}}
outer:
	for _, n := range owned {
		if slices.Contains(protected, n) || (present != nil && !present[n]) {
			continue
		}
		for _, name := range order {
			if slices.Contains(others[name], n) {
				r.Transfer[name] = append(r.Transfer[name], n)
				continue outer
			}
		}
		r.Remove = append(r.Remove, n)
	}
	return r
}
