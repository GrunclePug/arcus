// Package state holds the JSON state and the lock of one half (user or
// system) and the ownership rules shared by every resource kind.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"os"
	"slices"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// Resource kinds whose ownership is recorded. Each half records only its own
// kinds: the system half pacman packages, own builds, system units and
// groups; the user half AUR packages, user units, home directories and
// home links.
const (
	Pacman      = "pacman"
	Local       = "local" // own builds from recipes
	AUR         = "aur"
	SystemUnits = "system-units"
	UserUnits   = "user-units"
	Groups      = "groups"
	HomeDirs    = "home-dirs"  // directories in $HOME, removed only when empty
	HomeLinks   = "home-links" // symlinks in $HOME, removed only while they still point at their target
)

// Version is the state file format. Load refuses any other, so an older
// arcus never misreads a newer file.
const Version = 1

// State is one half's record of what arcus has done.
type State struct {
	Version int `json:"version"`

	// Modules are the modules with a layer in this half, in stack order
	// (later wins). A module without files for this half has no layer here.
	Modules []string `json:"modules"`

	// Owned records, per kind and module, the resources arcus itself added
	// for that module (they were absent before). Only these are ever removed,
	// so anything that was already present is left alone.
	Owned map[string]map[string][]string `json:"owned,omitempty"`

	// Built records, per own build installed by arcus, the hash of the recipe
	// it was built from, so apply rebuilds only when the recipe changes.
	Built map[string]string `json:"built,omitempty"`
}

// Load reads the state file. A missing file is an empty state.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return &State{Version: Version, Modules: []string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}

	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing state %s: %w", path, err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("state %s has format %d; this arcus reads format %d", path, s.Version, Version)
	}
	if s.Modules == nil {
		s.Modules = []string{}
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("state %s is inconsistent: %w (run 'arcus doctor')", path, err)
	}
	return &s, nil
}

// validate rejects state that would make stack arithmetic ambiguous.
func (s *State) validate() error {
	seen := make(map[string]bool, len(s.Modules))
	for _, name := range s.Modules {
		if seen[name] {
			return fmt.Errorf("module %q appears twice in the stack", name)
		}
		seen[name] = true
	}
	return nil
}

// Save writes the state atomically.
func (s *State) Save(path string) error {
	s.Version = Version
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}
	if err := atomicfile.Write(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}

// Own records that arcus added names for module. Names already recorded for
// the module are not repeated.
func (s *State) Own(kind, module string, names []string) {
	if len(names) == 0 {
		return
	}
	if s.Owned == nil {
		s.Owned = map[string]map[string][]string{}
	}
	if s.Owned[kind] == nil {
		s.Owned[kind] = map[string][]string{}
	}
	owned := s.Owned[kind][module]
	for _, n := range names {
		if !slices.Contains(owned, n) {
			owned = append(owned, n)
		}
	}
	s.Owned[kind][module] = owned
}

// Release drops module's ownership record for kind after r was carried out:
// transferred resources become owned by the modules that still declare them,
// and removed or kept ones are forgotten.
func (s *State) Release(kind, module string, r Removal) {
	delete(s.Owned[kind], module)
	for to, names := range r.Transfer {
		s.Own(kind, to, names)
	}
	if len(s.Owned[kind]) == 0 {
		delete(s.Owned, kind)
	}
}

// Clone returns a deep copy, for planning against a state without changing it.
func (s *State) Clone() *State {
	c := &State{Version: s.Version, Modules: slices.Clone(s.Modules), Built: maps.Clone(s.Built)}
	for kind, byModule := range s.Owned {
		for module, names := range byModule {
			c.Own(kind, module, names)
		}
	}
	return c
}

// Reconcile applies the ownership rules to every record of kind at once and
// returns what may be removed. A resource stays with its owner while the
// owner is in order and still declares it. Otherwise it moves to the first
// module in order that declares it, or is removed unless it is protected or
// already gone. order is the desired modules in stack order and declares
// what each of them lists. The record is updated as if the removal
// succeeded, so call it on a Clone until it has.
func (s *State) Reconcile(kind string, order []string, declares map[string][]string, protected []string, present map[string]bool) []string {
	var remove []string
	owners := slices.Sorted(maps.Keys(s.Owned[kind]))
	for _, owner := range owners {
		wanted := slices.Contains(order, owner)
		var keep, released []string
		for _, n := range s.Owned[kind][owner] {
			if wanted && slices.Contains(declares[owner], n) {
				keep = append(keep, n)
			} else {
				released = append(released, n)
			}
		}
		if len(released) == 0 {
			continue
		}

		others := slices.DeleteFunc(slices.Clone(order), func(p string) bool { return p == owner })
		r := Removals(released, others, declares, protected, present)
		s.Release(kind, owner, r)
		s.Own(kind, owner, keep)
		for _, n := range r.Remove {
			if !slices.Contains(remove, n) {
				remove = append(remove, n)
			}
		}
	}
	return remove
}

// Prune forgets owned resources of kind that are no longer present (removed
// outside arcus) and returns them.
func (s *State) Prune(kind string, present map[string]bool) []string {
	var pruned []string
	for owner, names := range s.Owned[kind] {
		kept := slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !present[n] })
		for _, n := range names {
			if !present[n] {
				pruned = append(pruned, n)
			}
		}
		if len(kept) == 0 {
			delete(s.Owned[kind], owner)
		} else {
			s.Owned[kind][owner] = kept
		}
	}
	if len(s.Owned[kind]) == 0 {
		delete(s.Owned, kind)
	}
	slices.Sort(pruned)
	return pruned
}

// Rename moves every record of module oldName to newName.
func (s *State) Rename(oldName, newName string) {
	for i, p := range s.Modules {
		if p == oldName {
			s.Modules[i] = newName
		}
	}
	for _, byModule := range s.Owned {
		if names, ok := byModule[oldName]; ok {
			delete(byModule, oldName)
			byModule[newName] = names
		}
	}
}

// Has reports whether the state records anything for module.
func (s *State) Has(module string) bool {
	if slices.Contains(s.Modules, module) {
		return true
	}
	for _, byModule := range s.Owned {
		if _, ok := byModule[module]; ok {
			return true
		}
	}
	return false
}
