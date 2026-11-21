// Package layer deploys the files of modules onto a target tree as
// reversible layers, journaled in a write-ahead ledger, with a separate home
// and system half.
package layer

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// Stacker manages the layers of one half: the home/ trees of the modules
// deployed into $HOME as symlinks, or their system/ trees deployed under the
// root prefix as copies.
//
// Model: for a given host path P, the active modules that deploy P form a
// chain in stack order. The topmost deployment lives at P itself; every
// lower one lives at the Backup path recorded by the entry directly above
// it; the bottom entry's Backup holds the host's original (if any).
// Removing any link of the chain splices it out, so layers can be torn down
// in any order without stranding or deleting the original.
type Stacker struct {
	// Subdir is the module subdirectory this half deploys ("home" or "system").
	Subdir string
	// Target is the directory Subdir maps onto: $HOME, or the root prefix
	// ("/" outside tests).
	Target string
	// Copy deploys copies verified by SHA-256 instead of symlinks.
	Copy bool
	// Allowed limits deployments to these directories below Target. Empty
	// means anywhere below Target.
	Allowed []string
	// StateDir is the half's state directory. Ledgers live in its ledger/
	// subdirectory, and no module may deploy anything into it.
	StateDir string
	// Dirs maps every known module name to its directory.
	Dirs map[string]string

	// Warn receives non-fatal diagnostics. Defaults to slog.
	Warn func(format string, args ...any)

	// holding is set between Hold and Release. A hold an interrupted
	// restack left on disk is released by doctor, never by a turn-off.
	holding bool

	// crashHook, when set, runs between the individual changes of an
	// operation. Tests panic from it to simulate a crash at that point.
	crashHook func()
	// saveHook, when set, runs before a ledger is saved. Tests fail a save
	// through it.
	saveHook func(module string) error
}

// NewHome returns the stacker for the user half: home/ trees are symlinked
// into home.
func NewHome(home, stateDir string, dirs map[string]string) *Stacker {
	return &Stacker{Subdir: "home", Target: home, StateDir: stateDir, Dirs: dirs, Warn: warn}
}

// NewSystem returns the stacker for the system half: system/ trees are
// copied below root, only into /etc and /usr/local. Copies are owned by the
// running process, which is root outside tests.
func NewSystem(root, stateDir string, dirs map[string]string) *Stacker {
	return &Stacker{
		Subdir: "system", Target: root, Copy: true, Allowed: []string{"etc", "usr/local"},
		StateDir: stateDir, Dirs: dirs, Warn: warn,
	}
}

func warn(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

func (s *Stacker) crashPoint() {
	if s.crashHook != nil {
		s.crashHook()
	}
}

// tree returns the directory of module that this half deploys, or "" if the
// module is unknown.
func (s *Stacker) tree(module string) string {
	dir, ok := s.Dirs[module]
	if !ok {
		return ""
	}
	return filepath.Join(dir, s.Subdir)
}

// HasTree reports whether module has anything for this half to deploy.
func (s *Stacker) HasTree(module string) bool {
	tree := s.tree(module)
	if tree == "" {
		return false
	}
	info, err := os.Stat(tree)
	return err == nil && info.IsDir()
}

// hostPathFor maps a file inside a module tree onto the target.
func (s *Stacker) hostPathFor(tree, src string) (string, error) {
	rel, err := filepath.Rel(tree, src)
	if err != nil {
		return "", fmt.Errorf("relating %q to %q: %w", src, tree, err)
	}
	return filepath.Join(s.Target, rel), nil
}

// ownData reports whether host lies inside arcus's own data.
func (s *Stacker) ownData(host string) bool {
	if isWithin(host, s.StateDir) {
		return true
	}
	for _, dir := range s.Dirs {
		if isWithin(host, dir) {
			return true
		}
	}
	return false
}

func (s *Stacker) allowed(host string) bool {
	if len(s.Allowed) == 0 {
		return true
	}
	for _, a := range s.Allowed {
		root := filepath.Join(s.Target, a)
		if host != root && isWithin(host, root) {
			return true
		}
	}
	return false
}

// ErrForeign is returned when a host path no longer holds what arcus deployed.
var ErrForeign = errors.New("foreign modification")

// Activate layers module on top of activeStack (which must not contain it).
// On any error, including ctx cancellation, every mutation made by this call
// is rolled back and the ledger is removed.
func (s *Stacker) Activate(ctx context.Context, module string, activeStack []string) (err error) {
	if l, err := s.LoadLedger(module); err != nil {
		return err
	} else if l != nil {
		return fmt.Errorf("module %q already has a ledger (status %q); run 'arcus doctor -fix'", module, l.Status)
	}

	below, err := s.loadStack(activeStack)
	if err != nil {
		return err
	}
	if err := s.yieldHeldDirs(module); err != nil {
		return err
	}

	// Phase 1: plan and pre-flight every path before touching anything.
	plan, err := s.planActivation(module, below, true, nil)
	if err != nil {
		return err
	}

	// Phase 2: apply, journaling each step before performing it.
	l := &Ledger{Module: module, Status: StatusActivating}
	if err := s.saveLedger(l); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		if rbErr := s.Rollback(module); rbErr != nil {
			err = fmt.Errorf("%w; rollback incomplete: %v (run 'arcus doctor -fix')", err, rbErr)
		}
	}()

	for _, e := range plan {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("activation of %q interrupted: %w", module, err)
		}

		// Re-check right before mutating: the host may have changed since planning.
		if exists(e.Host) != (e.Backup != "") {
			return fmt.Errorf("host path %q changed during activation", e.Host)
		}

		created := missingDirs(filepath.Dir(e.Host))
		l.CreatedDirs = append(l.CreatedDirs, created...)
		l.Entries = append(l.Entries, e)
		if err := s.saveLedger(l); err != nil {
			return err
		}
		s.crashPoint()

		if len(created) > 0 {
			if err := os.MkdirAll(filepath.Dir(e.Host), 0o755); err != nil {
				return fmt.Errorf("creating parent directories for %q: %w", e.Host, err)
			}
		}
		if e.Backup != "" {
			if err := renameDurable(e.Host, e.Backup); err != nil {
				return fmt.Errorf("backing up %q to %q: %w", e.Host, e.Backup, err)
			}
			s.crashPoint()
		}
		if err := deploy(e); err != nil {
			return fmt.Errorf("deploying %q: %w", e.Host, err)
		}
		s.crashPoint()
	}

	l.Status = StatusActive
	return s.saveLedger(l)
}

func deploy(e Entry) error {
	if e.Kind == KindLink {
		return symlinkReplace(e.Src, e.Host)
	}
	data, err := readRegular(e.Src)
	if err != nil {
		return err
	}
	if hashBytes(data) != e.SHA256 {
		return fmt.Errorf("%s changed during activation", e.Src)
	}
	mode, err := parseMode(e.Mode)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(e.Host, data, mode); err != nil {
		return err
	}
	return os.Chown(e.Host, e.UID, e.GID)
}

// Preview returns what activating module on top of activeStack would do,
// running the same pre-flight checks except the write-permission one, so the
// user can preview the root half. Entries with a Backup replace an existing
// file. Paths in freed count as empty: steps that run first remove them.
func (s *Stacker) Preview(module string, activeStack []string, freed map[string]bool) ([]Entry, error) {
	below, err := s.loadStack(activeStack)
	if err != nil {
		return nil, err
	}
	return s.planActivation(module, below, false, freed)
}

// Frees adds to freed the paths that taking module's entries for hosts off
// leaves empty (all of its entries when hosts is nil): those that didn't
// replace a file, since a replaced file comes back.
func (s *Stacker) Frees(module string, hosts []string, freed map[string]bool) error {
	l, err := s.LoadLedger(module)
	if err != nil || l == nil {
		return err
	}
	for _, e := range l.Entries {
		if e.Backup == "" && (hosts == nil || slices.Contains(hosts, e.Host)) {
			freed[e.Host] = true
		}
	}
	return nil
}

// symlinkedParent returns the first existing directory between Target and
// host that is a symlink, or "". Deploying through one would write wherever
// it points: into a dotfiles repository, or, for the system half, anywhere.
func (s *Stacker) symlinkedParent(host string) string {
	var dirs []string
	for dir := filepath.Dir(host); dir != s.Target && isWithin(dir, s.Target); dir = filepath.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
	}
	for _, dir := range dirs {
		info, err := os.Lstat(dir)
		if err != nil {
			return ""
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return dir
		}
	}
	return ""
}

// symlinkProblem refuses teardown of an entry whose host path lies below a
// directory that has become a symlink since activation, which refuses such a
// path too. Everything teardown would look at or change there (the
// deployment, its backup, a lower layer's file) comes through the link, so
// nothing about the entry can be trusted, not even that nothing is left to do.
func (s *Stacker) symlinkProblem(host string) error {
	dir := s.symlinkedParent(host)
	if dir == "" {
		return nil
	}
	target, _ := os.Readlink(dir)
	return fmt.Errorf("%s: parent %s is a symlink (to %s); make it a real directory again, with everything that was in it back in place if you moved it", host, dir, target)
}

func (s *Stacker) planActivation(module string, below []*Ledger, checkAccess bool, freed map[string]bool) ([]Entry, error) {
	tree := s.tree(module)
	if tree == "" {
		return nil, fmt.Errorf("module %q does not exist", module)
	}
	if _, err := os.Stat(tree); errors.Is(err, iofs.ErrNotExist) {
		return nil, nil // nothing to deploy in this half
	}

	var plan []Entry
	var problems []string
	err := walkTree(tree, func(src string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		host, err := s.hostPathFor(tree, src)
		if err != nil {
			return err
		}

		if s.ownData(host) {
			problems = append(problems, fmt.Sprintf("%s: refusing to layer over arcus's own data", host))
			return nil
		}
		if !s.allowed(host) {
			problems = append(problems, fmt.Sprintf("%s: outside the allowed directories (%s)", host, strings.Join(s.Allowed, ", ")))
			return nil
		}
		if dir := s.symlinkedParent(host); dir != "" {
			target, _ := os.Readlink(dir)
			problems = append(problems, fmt.Sprintf("%s: parent %s is a symlink (to %s); remove it first", host, dir, target))
			return nil
		}
		if checkAccess {
			if err := VerifyWritePrivilege(host); err != nil {
				problems = append(problems, err.Error())
				return nil
			}
		}

		e := Entry{Host: host, Src: src, Kind: KindLink}
		if s.Copy {
			if !d.Type().IsRegular() {
				problems = append(problems, fmt.Sprintf("%s: not a regular file (system files are copied, never linked)", src))
				return nil
			}
			data, err := readRegular(src)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			e.Kind, e.SHA256, e.Mode = KindCopy, hashBytes(data), copyMode(info.Mode())
			e.UID, e.GID = os.Geteuid(), os.Getegid()
		}

		if info, err := os.Lstat(host); err == nil && !freed[host] {
			if info.IsDir() {
				problems = append(problems, fmt.Sprintf("%s: module provides a file but the host has a directory", host))
				return nil
			}
			e.Backup = uniquePath(host + "." + ownerTag(host, below))
		}
		plan = append(plan, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s of %q: %w", s.Subdir, module, err)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("pre-flight failed for %q, nothing was changed:\n  %s", module, strings.Join(problems, "\n  "))
	}
	return plan, nil
}

// ownerTag names a backup after the layer whose file it holds: the topmost
// active module already deploying host, or "orig" for the host's own file.
func ownerTag(host string, below []*Ledger) string {
	for i := len(below) - 1; i >= 0; i-- {
		if below[i] != nil && below[i].find(host) >= 0 {
			return below[i].Module
		}
	}
	return "orig"
}

// step is one planned teardown action for an entry.
type step struct {
	idx      int     // index into the ledger's entries
	next     *Ledger // lowest module above that deploys the same host path, or nil
	nextIdx  int
	loc      string // where this entry's deployment currently lives ("" = nowhere)
	remove   bool   // loc holds our deployment and must be removed
	foreign  bool   // loc holds something we did not put there
	cache    bool   // loc holds a home-half bytecode cache Python rewrote; teardown removes it
	finished bool   // a previous interrupted run already completed this step
}

// planStep classifies one entry. It is written so that re-running teardown
// after a crash at any point converges instead of misreporting state.
func (s *Stacker) planStep(l *Ledger, idx int, above []*Ledger) step {
	e := l.Entries[idx]
	st := step{idx: idx}
	for _, a := range above {
		if a == nil {
			continue
		}
		if j := a.find(e.Host); j >= 0 {
			st.next, st.nextIdx = a, j
			break
		}
	}

	if st.next == nil {
		st.loc = e.Host
	} else {
		st.loc = st.next.Entries[st.nextIdx].Backup
		if e.Backup != "" && st.loc == e.Backup {
			st.finished = true // already spliced out
			return st
		}
	}

	switch {
	case st.loc == "" || !exists(st.loc):
		// Nothing deployed here any more.
	case owns(e, st.loc):
		// A copy whose recorded backup never got created is
		// indistinguishable from an untouched original with identical
		// content; leave it rather than risk deleting the original.
		if st.next == nil && e.Kind == KindCopy && e.Backup != "" && !exists(e.Backup) {
			return st
		}
		st.remove = true
	case st.next == nil && e.Backup != "" && !exists(e.Backup):
		st.finished = true // original already restored by an interrupted run
	case st.next == nil && cacheEntry(e) && isWithin(st.loc, s.Target) && s.symlinkedParent(st.loc) == "" && isPyc(st.loc):
		// Ledgers written before 0.8.2 hold cache links, and Python replaces a
		// cache by renaming a new file over it. This comes after finished, so
		// an original a re-run already put back is kept.
		st.cache = true
	default:
		st.foreign = true
	}
	return st
}

// Deactivate removes module's layer. activeStack is the full current stack
// (containing module). In force mode, files that were modified after
// activation are moved aside to <path>.arcus-foreign[.N] instead of blocking;
// arcus never deletes data it did not create, except a home-half bytecode
// cache Python rewrote over one arcus deployed.
func (s *Stacker) Deactivate(ctx context.Context, module string, activeStack []string, force bool) error {
	pos := slices.Index(activeStack, module)
	if pos < 0 {
		return fmt.Errorf("module %q is not in the active stack", module)
	}

	stack, err := s.loadStack(activeStack)
	if err != nil {
		return err
	}
	l, above := stack[pos], stack[pos+1:]
	if l == nil {
		return fmt.Errorf("module %q is on but has no ledger; run 'arcus doctor'", module)
	}

	// Pre-flight: refuse before mutating anything if a path lies below a
	// symlinked parent, which -f doesn't override, or if unforced and foreign.
	var linked, foreign []string
	for i, e := range l.Entries {
		if err := s.symlinkProblem(e.Host); err != nil {
			linked = append(linked, err.Error())
			continue
		}
		if st := s.planStep(l, i, above); st.foreign {
			foreign = append(foreign, st.loc)
		}
	}
	if len(linked) > 0 {
		return fmt.Errorf("cannot turn off %q, nothing was changed in its %s files:\n  %s", module, s.Subdir, strings.Join(linked, "\n  "))
	}
	if len(foreign) > 0 && !force {
		return fmt.Errorf("%w: these paths changed since %q was turned on, nothing was changed (use -f to move them aside):\n  %s",
			ErrForeign, module, strings.Join(foreign, "\n  "))
	}

	l.Status = StatusDeactivating
	if err := s.saveLedger(l); err != nil {
		return err
	}

	for i := len(l.Entries) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("deactivation of %q interrupted, re-run it or 'arcus doctor -fix' to finish: %w", module, err)
		}
		s.crashPoint()
		if err := s.teardownEntry(l, i, above); err != nil {
			return err
		}
		s.crashPoint()
		l.Entries = l.Entries[:i]
		if err := s.saveLedger(l); err != nil {
			return err
		}
	}

	if err := s.dropCreatedDirs(l, stack); err != nil {
		return err
	}
	s.crashPoint()
	return s.removeLedger(module)
}

func (s *Stacker) teardownEntry(l *Ledger, idx int, above []*Ledger) error {
	e := l.Entries[idx]
	if err := s.symlinkProblem(e.Host); err != nil {
		return err
	}
	st := s.planStep(l, idx, above)
	if st.finished {
		return nil
	}

	switch {
	case st.remove || st.cache:
		if err := removeDurable(st.loc); err != nil {
			return fmt.Errorf("removing %q: %w", st.loc, err)
		}
		s.crashPoint()
	case st.foreign:
		aside := uniquePath(st.loc + ".arcus-foreign")
		if err := renameDurable(st.loc, aside); err != nil {
			return fmt.Errorf("moving modified %q aside: %w", st.loc, err)
		}
		s.Warn("%s was modified after activation; preserved as %s", st.loc, aside)
		s.crashPoint()
	}

	if st.next != nil {
		// Splice: the layer above now displaces whatever we displaced.
		st.next.Entries[st.nextIdx].Backup = e.Backup
		return s.saveLedger(st.next)
	}

	if e.Backup != "" && exists(e.Backup) {
		if exists(e.Host) {
			return fmt.Errorf("cannot restore %q: %q is occupied", e.Backup, e.Host)
		}
		if err := renameDurable(e.Backup, e.Host); err != nil {
			return fmt.Errorf("restoring %q to %q: %w", e.Backup, e.Host, err)
		}
	}
	return nil
}

// HoldLedger is the ledger that keeps the directories of the layers a
// restack turns off until the layers are on again. Module names start with a
// letter or digit, so no module can have it. Doctor releases a hold that an
// interrupted restack left.
const HoldLedger = "_restack"

// Hold makes the deactivations that follow keep the directories their layers
// created, for Release to sort out once the layers are on again. A restack
// would otherwise replace every directory its layers created with a new one,
// and programs watching the old one (a shell's plugin folder, an app's
// preset folder) lose track of it or crash. A hold an interrupted restack
// left must be released by doctor first.
func (s *Stacker) Hold() error {
	if err := s.CheckHold(); err != nil {
		return err
	}
	if err := s.saveLedger(&Ledger{Module: HoldLedger, Status: StatusActive}); err != nil {
		return err
	}
	s.holding = true
	return nil
}

// HoldPath is the hold ledger's file, for a message that tells the user how
// to give a hold up by hand.
func (s *Stacker) HoldPath() string { return s.ledgerPath(HoldLedger) }

// CheckHold reports a hold an interrupted restack left. Turning layers on or
// off before doctor releases it would lose track of the held directories.
func (s *Stacker) CheckHold() error {
	l, err := s.LoadLedger(HoldLedger)
	if err != nil {
		return fmt.Errorf("%w; run 'arcus doctor -fix'", err)
	}
	if l != nil {
		return fmt.Errorf("a restack of the %s files was interrupted; run 'arcus doctor -fix' first", s.Subdir)
	}
	return nil
}

// Release ends a hold. Each held directory that still holds files passes to
// a ledger with files or directories in it, whether that layer is on or was
// interrupted, and the empty ones are removed, as the deactivations would
// have done. If a directory can be neither handed over nor removed, the hold
// stays for the next 'arcus doctor -fix'.
func (s *Stacker) Release() error {
	s.holding = false
	hold, err := s.LoadLedger(HoldLedger)
	if err != nil || hold == nil {
		return err
	}
	names, err := s.ListLedgers()
	if err != nil {
		return err
	}
	others, err := s.loadStack(slices.DeleteFunc(names, func(n string) bool { return n == HoldLedger }))
	if err != nil {
		return err
	}
	if err := s.removeCreatedDirs(hold, others); err != nil {
		return err
	}
	s.crashPoint()
	return s.removeLedger(HoldLedger)
}

// dropCreatedDirs gives up the directories a layer being turned off created:
// to the hold during a restack, otherwise by removing them.
func (s *Stacker) dropCreatedDirs(l *Ledger, stack []*Ledger) error {
	if !s.holding {
		if err := s.removeCreatedDirs(l, stack); err != nil {
			s.Warn("%v", err)
		}
		return nil
	}
	hold, err := s.LoadLedger(HoldLedger)
	if err != nil {
		return err
	}
	if hold == nil {
		return fmt.Errorf("the %s ledger is gone during a restack", HoldLedger)
	}
	for _, dir := range l.CreatedDirs {
		if !slices.Contains(hold.CreatedDirs, dir) {
			hold.CreatedDirs = append(hold.CreatedDirs, dir)
		}
	}
	return s.saveLedger(hold)
}

// yieldHeldDirs removes the empty held directories where the module now
// deploys a file, so a module that turned a directory into a file restacks
// as it would without the hold. It runs before Activate's pre-flight, which
// is safe only because it removes nothing but empty directories (rmdir) and
// never below a symlinked parent.
func (s *Stacker) yieldHeldDirs(module string) error {
	if !s.holding {
		return nil
	}
	hold, err := s.LoadLedger(HoldLedger)
	if err != nil || hold == nil || len(hold.CreatedDirs) == 0 {
		return err
	}
	tree := s.tree(module)
	if tree == "" || !exists(tree) {
		return nil
	}
	var yielded []string
	err = walkTree(tree, func(src string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		host, err := s.hostPathFor(tree, src)
		if err != nil {
			return err
		}
		dirs := slices.DeleteFunc(slices.Clone(hold.CreatedDirs), func(dir string) bool { return !isWithin(dir, host) })
		slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
		for _, dir := range dirs {
			if s.symlinkedParent(dir) == "" && syscall.Rmdir(dir) == nil {
				yielded = append(yielded, dir)
			}
		}
		return nil
	})
	if err != nil || len(yielded) == 0 {
		return err
	}
	hold.CreatedDirs = slices.DeleteFunc(hold.CreatedDirs, func(dir string) bool { return slices.Contains(yielded, dir) })
	return s.saveLedger(hold)
}

// removeCreatedDirs deletes directories this layer created, deepest first,
// if they are empty. It removes only directories (rmdir), so a file or a
// symlink that took a directory's place stays, and nothing below a symlinked
// parent. A directory still holding other layers' files or directories is
// handed to each of their ledgers, so it is cleaned up when the last of them
// goes; handing it to one alone would lose it if that one were rolled back.
// The error lists the hand-overs that failed to save and the empty
// directories no ledger took that could not be removed; a non-empty
// directory no ledger took (other programs' files) is left, as before.
func (s *Stacker) removeCreatedDirs(l *Ledger, stack []*Ledger) error {
	dirs := slices.Clone(l.CreatedDirs)
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })

	var errs []error
	for _, dir := range dirs {
		if s.symlinkedParent(dir) != "" {
			continue
		}
		err := syscall.Rmdir(dir)
		if err == nil || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		taken := false
		for _, other := range stack {
			if other == nil || other == l {
				continue
			}
			if slices.Contains(other.CreatedDirs, dir) {
				taken = true
				continue
			}
			if !slices.ContainsFunc(other.Entries, func(e Entry) bool { return isWithin(e.Host, dir) }) &&
				!slices.ContainsFunc(other.CreatedDirs, func(d string) bool { return isWithin(d, dir) }) {
				continue
			}
			other.CreatedDirs = append(other.CreatedDirs, dir)
			if serr := s.saveLedger(other); serr != nil {
				errs = append(errs, fmt.Errorf("could not transfer ownership of %s: %w", dir, serr))
				continue
			}
			taken = true
		}
		if taken || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			continue
		}
		if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) > 0 {
			s.Warn("leaving %s: %v", dir, err)
			continue
		}
		errs = append(errs, fmt.Errorf("removing %s: %w", dir, err))
	}
	return errors.Join(errs...)
}

// Rollback undoes a module's layer as if it were the top of the stack. It is
// used for failed or interrupted activations and orphaned ledgers. It never
// touches foreign files; those are reported and left in place.
func (s *Stacker) Rollback(module string) error {
	l, err := s.LoadLedger(module)
	if err != nil || l == nil {
		return err
	}

	var problems []string
	for i := len(l.Entries) - 1; i >= 0; i-- {
		// Before planStep, whose view through the link can't be trusted.
		if err := s.symlinkProblem(l.Entries[i].Host); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		st := s.planStep(l, i, nil)
		if st.foreign {
			problems = append(problems, fmt.Sprintf("%s was modified; left in place", st.loc))
			continue
		}
		if err := s.teardownEntry(l, i, nil); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		l.Entries = append(l.Entries[:i], l.Entries[i+1:]...)
		if err := s.saveLedger(l); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	if err := s.removeCreatedDirs(l, nil); err != nil {
		s.Warn("%v", err)
	}
	return s.removeLedger(module)
}

// Rename moves a layer's ledger and live symlinks from oldName to newName
// after the module directory was moved from oldDir to newDir. activeStack
// uses the old name.
func (s *Stacker) Rename(oldName, newName, oldDir, newDir string, activeStack []string) error {
	stack, err := s.loadStack(activeStack)
	if err != nil {
		return err
	}
	pos := slices.Index(activeStack, oldName)
	if pos < 0 || stack[pos] == nil {
		return nil
	}
	l, above := stack[pos], stack[pos+1:]

	oldRoot, newRoot := filepath.Join(oldDir, s.Subdir), filepath.Join(newDir, s.Subdir)
	for i := range l.Entries {
		e := &l.Entries[i]
		newSrc := newRoot + strings.TrimPrefix(e.Src, oldRoot)
		st := s.planStep(l, i, above)
		if e.Kind == KindLink && st.loc != "" && st.remove {
			if err := symlinkReplace(newSrc, st.loc); err != nil {
				return fmt.Errorf("relinking %q: %w", st.loc, err)
			}
		}
		e.Src = newSrc
	}

	// Backups named after the old module are renamed so on-disk tags stay truthful.
	for _, other := range stack {
		if other == nil || other == l {
			continue
		}
		dirty := false
		for i := range other.Entries {
			e := &other.Entries[i]
			// Only ".<old>" or ".<old>.<N>".
			if tag, ok := strings.CutPrefix(e.Backup, e.Host+"."); !ok || backupTag(tag) != oldName {
				continue
			}
			fresh := uniquePath(e.Host + "." + newName)
			if exists(e.Backup) {
				if err := renameDurable(e.Backup, fresh); err != nil {
					return fmt.Errorf("renaming backup %q: %w", e.Backup, err)
				}
			}
			e.Backup, dirty = fresh, true
		}
		if dirty {
			if err := s.saveLedger(other); err != nil {
				return err
			}
		}
	}

	l.Module = newName
	if err := s.saveLedger(l); err != nil {
		return err
	}
	return s.removeLedger(oldName)
}
