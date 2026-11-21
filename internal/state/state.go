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
// groups; the user half AUR packages, user units, home directories, home
// links and AppImages.
const (
	Pacman      = "pacman"
	Local       = "local" // own builds from recipes
	AUR         = "aur"
	SystemUnits = "system-units"
	UserUnits   = "user-units"
	Groups      = "groups"
	HomeDirs    = "home-dirs"  // directories in $HOME, removed only when empty
	HomeLinks   = "home-links" // symlinks in $HOME, removed only while they still point at their target
	// AppImages in $HOME, by path. Each also has a record in State.AppImages.
	HomeAppImages = "home-appimages"
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

	// AppImages records, per AppImage path below $HOME that is owned as
	// HomeAppImages, the file arcus installed or took over there.
	AppImages map[string]AppImage `json:"appimages,omitempty"`
}

// AppImage is the record of an AppImage file.
type AppImage struct {
	// SHA256 is the file's content when arcus installed or took it over.
	SHA256 string `json:"sha256"`
	// SelfUpdate is the module's self_update then: the app may replace the
	// file, so off removes it even if it changed.
	SelfUpdate bool `json:"self_update,omitempty"`
	// Stamp is the file as it was when SHA256 was taken: while it still
	// matches, the file still has SHA256 and needn't be read again.
	Stamp Stamp `json:"stamp"`
}

// Stamp identifies a version of a file the way git's index does: any write,
// rename over it, chmod or touch changes one of these.
type Stamp struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // nanoseconds
	CTime int64  `json:"ctime"` // nanoseconds
	Inode uint64 `json:"inode"`
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
	c := &State{Version: s.Version, Modules: slices.Clone(s.Modules), Built: maps.Clone(s.Built), AppImages: maps.Clone(s.AppImages)}
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
	s.dropUnownedAppImages(kind)
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
	s.dropUnownedAppImages(kind)
	slices.Sort(pruned)
	return pruned
}

// Owner returns the module that owns name as kind, or "".
func (s *State) Owner(kind, name string) string {
	for owner, names := range s.Owned[kind] {
		if slices.Contains(names, name) {
			return owner
		}
	}
	return ""
}

// AppImage returns the record of the AppImage at path, or nil if no module
// owns it or it has no record (one an older arcus dropped on saving).
func (s *State) AppImage(path string) *AppImage {
	r, ok := s.AppImages[path]
	if !ok || s.Owner(HomeAppImages, path) == "" {
		return nil
	}
	return &r
}

// SetAppImage records that module owns the AppImage at path, as r.
func (s *State) SetAppImage(module, path string, r AppImage) {
	s.Own(HomeAppImages, module, []string{path})
	if s.AppImages == nil {
		s.AppImages = map[string]AppImage{}
	}
	s.AppImages[path] = r
}

// dropUnownedAppImages forgets the records of AppImages that reconciling or
// pruning kind left without an owner.
func (s *State) dropUnownedAppImages(kind string) {
	if kind != HomeAppImages {
		return
	}
	maps.DeleteFunc(s.AppImages, func(path string, _ AppImage) bool { return s.Owner(HomeAppImages, path) == "" })
	if len(s.AppImages) == 0 {
		s.AppImages = nil
	}
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
