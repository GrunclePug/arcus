package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/profile"
	"github.com/grunclepug/arcus/internal/system"
)

// cmdMachine writes the machine module from the running system: what it
// needs to boot and swap the way it is set up. Like plan and apply, it shows
// the changes first.
func (a *app) cmdMachine(args []string) error {
	var o opts
	fs := a.flags("machine", &o, true, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: arcus machine [-y] [-n]")
	}
	if err := a.load(&o); err != nil {
		return err
	}
	facts, err := a.machineFacts()
	if err != nil {
		return err
	}
	dir := filepath.Join(a.paths.Machine, machine.Name)
	changes, err := fileChanges(dir, facts.Files())
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintf(a.stdout, "The machine module in %s is up to date.\n", dir)
		return nil
	}
	fmt.Fprintf(a.stdout, "root  write the machine module in %s:\n", dir)
	for _, c := range changes {
		fmt.Fprintf(a.stdout, "        %s\n", c)
	}
	if o.dry {
		return nil
	}
	if !o.yes && !a.confirm("Proceed?") {
		return errors.New("cancelled, nothing was changed")
	}
	if err := a.sudo([]string{"-machine", "-hash", facts.Hash(), "-home", a.paths.Home, "-config", a.paths.Config}); err != nil {
		return fmt.Errorf("writing the machine module: %w", err)
	}
	fmt.Fprintln(a.stdout, a.machineHint(&o))
	return nil
}

// machineHint says what comes after writing the machine module: whether the
// host file already selects it, through a profile or its own modules.
func (a *app) machineHint(o *opts) string {
	const off = "Turn it on with 'arcus on machine', or list it in a profile."
	path, err := a.hostPath(o)
	if err != nil {
		return off
	}
	f, err := host.Load(path)
	if err != nil {
		return off
	}
	_, via, err := a.profiles.Expand(f.Profiles, f.Modules)
	from, ok := via[machine.Name]
	switch {
	case err != nil || !ok:
		return off
	case from == profile.Host:
		return "The host file turns it on; 'arcus apply' brings it in."
	}
	return fmt.Sprintf("Profile %s turns it on; 'arcus apply' brings it in.", from)
}

func (a *app) machineFacts() (machine.Facts, error) {
	installed, err := system.Installed()
	if err != nil {
		return machine.Facts{}, err
	}
	return machine.Detect(a.root, installed), nil
}

// writeMachine is exec-system -machine: the root side recomputes the module
// and writes it only if it is what the user confirmed.
func (a *app) writeMachine(hash string) error {
	facts, err := a.machineFacts()
	if err != nil {
		return err
	}
	if facts.Hash() != hash {
		return errors.New("the machine changed since its module was shown; nothing was done, run the command again")
	}
	dir := filepath.Join(a.paths.Machine, machine.Name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := facts.Write(dir); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Wrote %s.\n", dir)
	return nil
}

// fileChanges compares the files in dir with want, keyed by relative path:
// "+ path" (new), "- path" (gone) and "~ path" with its line diff.
func fileChanges(dir string, want map[string]string) ([]string, error) {
	have := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		have[rel] = string(data)
		return nil
	})
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return nil, err
	}
	var changes []string
	for _, path := range slices.Sorted(maps.Keys(want)) {
		old, ok := have[path]
		switch {
		case !ok:
			changes = append(changes, "+ "+path)
			for line := range strings.Lines(want[path]) {
				changes = append(changes, "  + "+strings.TrimRight(line, "\n"))
			}
		case old != want[path]:
			changes = append(changes, "~ "+path)
			for _, line := range lineDiff(old, want[path]) {
				changes = append(changes, "  "+line)
			}
		}
	}
	for _, path := range slices.Sorted(maps.Keys(have)) {
		if _, ok := want[path]; !ok {
			changes = append(changes, "- "+path)
		}
	}
	return changes, nil
}
