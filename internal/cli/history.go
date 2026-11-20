package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/grunclepug/arcus/internal/config"
	"github.com/grunclepug/arcus/internal/history"
	"github.com/grunclepug/arcus/internal/host"
	"github.com/grunclepug/arcus/internal/machine"
	"github.com/grunclepug/arcus/internal/state"
)

// recorder is one command's entry in the state history.
type recorder struct {
	begun, armed, warned bool
}

// lockUser takes the user half's lock. The history's git children inherit
// an exclusive one, so it stays held while one of them runs.
func (a *app) lockUser(exclusive bool) (*state.Lock, error) {
	lock, err := a.userHalf().lock(exclusive)
	if err == nil && exclusive {
		a.userLock = lock
	}
	return lock, err
}

// repo returns the history repository.
func (a *app) repo() *history.Repo {
	g := history.Git{Euid: a.euid}
	if a.userLock != nil && a.userLock.File() != nil {
		g.Files = []*os.File{a.userLock.File()}
	}
	return &history.Repo{Dir: filepath.Join(a.paths.UserState, "history"), UID: a.uid, Git: g}
}

// warnHistory says, once per command, why it isn't recorded.
func (a *app) warnHistory(err error) {
	if a.rec.warned {
		return
	}
	a.rec.warned = true
	fmt.Fprintf(a.stderr, "warning: state history: %v; this command isn't recorded\n", err)
}

// begin marks a command's point of no return; call it under the exclusive
// user lock. What changed since the latest entry (a hand edit, a run
// without git, a killed arcus, another user's run) becomes an entry of its
// own first; only when that worked is the command's own entry armed, which
// record makes when the command returns. So a change is never put in the
// wrong entry: when the first step fails, the next command's records it.
// It returns an error when the command was cancelled meanwhile, so nothing
// changes after a Ctrl-C there.
func (a *app) begin() error {
	a.beginHistory()
	if err := a.ctx.Err(); err != nil {
		return errors.New("cancelled, nothing was changed")
	}
	return nil
}

func (a *app) beginHistory() {
	if !a.history || a.rec.begun {
		return
	}
	a.rec.begun = true
	repo := a.repo()
	if _, err := repo.Check(true); err != nil {
		a.warnHistory(err)
		return
	}
	repo.SweepIndexes()
	files, err := a.snapshot()
	if err != nil {
		a.warnHistory(err)
		return
	}
	status, err := repo.Status()
	if err != nil {
		a.warnHistory(err)
		return
	}
	label := "unrecorded changes"
	if status != history.Healthy {
		label = "history starts"
	}
	if _, err := repo.Commit(files, label, false); err != nil {
		a.warnHistory(err)
		return
	}
	a.rec.armed = true
}

// record makes the command's entry, if begin armed it: the command line,
// " (failed)" when it failed, and the config directory's commit and arcus
// version as trailers. It is made even when nothing recorded changed.
func (a *app) record(failed bool) {
	if !a.rec.armed {
		return
	}
	a.rec.armed = false
	files, err := a.snapshot()
	if err != nil {
		a.warnHistory(err)
		return
	}
	msg := a.subject
	if failed {
		msg += " (failed)"
	}
	msg += "\n\n"
	// Probed now, not at begin: a rename by 'arcus mv' changes dot.
	if hash, dirty, ok := (history.Git{Euid: a.euid}).Dot(a.paths.Config); ok {
		msg += "Dot: " + hash
		if dirty {
			msg += " dirty"
		}
		msg += "\n"
	}
	msg += "Arcus: " + a.version + "\n"
	repo := a.repo()
	if _, err := repo.Check(true); err != nil {
		a.warnHistory(err)
		return
	}
	if _, err := repo.Commit(files, msg, true); err != nil {
		a.warnHistory(err)
		return
	}
	if err := repo.GC(); err != nil {
		fmt.Fprintf(a.stderr, "warning: state history: packing it: %v\n", err)
	}
}

// subjectOf flattens a command line to one line, so no argument can add
// lines (or trailers) to an entry.
func subjectOf(args []string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.Join(args, " "))
}

// snapshot reads what the history records: the host file, both halves'
// state and ledgers, and the machine module's manifest. The system half
// and the machine module are read under the system half's shared lock, so
// they are one consistent set.
func (a *app) snapshot() (map[string][]byte, error) {
	files := map[string][]byte{}
	if err := readInto(files, "host.toml", a.paths.HostFile()); err != nil {
		return nil, err
	}
	if err := a.readHalf(files, a.userHalf(), ""); err != nil {
		return nil, err
	}
	sh := a.systemHalf()
	lockFile := filepath.Join(sh.dir, ".lock")
	for {
		_, err := os.Stat(lockFile)
		existed := err == nil
		system := map[string][]byte{}
		if err := func() error {
			lock, err := sh.lock(false)
			if err != nil {
				return err
			}
			defer lock.Release()
			if err := a.readHalf(system, sh, "system/"); err != nil {
				return err
			}
			return readInto(system, "machine/module.toml", filepath.Join(a.paths.Machine, machine.Name, "module.toml"))
		}(); err != nil {
			return nil, err
		}
		// Without a lock file the shared lock is a no-op: if root created
		// one meanwhile, it may have been writing, so read again under it.
		if _, err := os.Stat(lockFile); existed || err != nil {
			for k, v := range system {
				files[k] = v
			}
			return files, nil
		}
	}
}

// readHalf reads a half's state and the ledgers it lists.
func (a *app) readHalf(files map[string][]byte, h *half, prefix string) error {
	if err := readInto(files, prefix+"state.json", h.statePath()); err != nil {
		return err
	}
	names, err := h.layer.ListLedgers()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := readInto(files, prefix+"ledger/"+name+".json", filepath.Join(h.dir, "ledger", name+".json")); err != nil {
			return err
		}
	}
	return nil
}

// readInto reads path into files under name; a missing file stays absent.
func readInto(files map[string][]byte, name, path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	files[name] = data
	return nil
}

// historyRepo opens the history for reading: it needs only the state
// directory, so it works with a broken module configuration.
func (a *app) historyRepo(cmd string) (*history.Repo, bool, error) {
	if !a.history {
		return nil, false, fmt.Errorf("arcus %s needs git: %w", cmd, history.ErrNoGit)
	}
	if a.paths.UserState == "" {
		home := a.getenv("HOME")
		if !filepath.IsAbs(home) {
			return nil, false, errors.New("HOME must be set to an absolute path")
		}
		a.paths = config.UserPaths(home, a.root, a.getenv)
	}
	repo := a.repo()
	if _, err := repo.Check(false); err != nil {
		return nil, false, err
	}
	status, err := repo.Status()
	if err != nil {
		return nil, false, err
	}
	return repo, status == history.Healthy, nil
}

// cmdHistory lists the latest entries of the state history.
func (a *app) cmdHistory(args []string) error {
	var o opts
	fs := a.flags("history", &o, false, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	count := 20
	if fs.NArg() == 1 {
		n, err := strconv.Atoi(fs.Arg(0))
		if err != nil || n < 1 {
			return errors.New("usage: arcus history [count]")
		}
		count = n
	} else if fs.NArg() > 1 {
		return errors.New("usage: arcus history [count]")
	}
	repo, ok, err := a.historyRepo("history")
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(a.stdout, "No history yet; it starts with the next command that changes the machine.")
		return nil
	}
	entries, err := repo.Log(count)
	if err != nil {
		return err
	}
	for _, e := range entries {
		line := fmt.Sprintf("%s %s %s", e.Hash[:min(7, len(e.Hash))], e.Time.Format("2006-01-02 15:04"), e.Subject)
		if hash, dirty := dotOf(e.Dot); hash != "" {
			line += "  dot " + hash[:min(7, len(hash))]
			if dirty {
				line += "*"
			}
		}
		fmt.Fprintln(a.stdout, line)
	}
	return nil
}

// dotOf splits a Dot trailer.
func dotOf(trailer string) (hash string, dirty bool) {
	hash, rest, _ := strings.Cut(trailer, " ")
	return hash, rest == "dirty"
}

// cmdRollback puts the host file's profiles and modules back as an entry of
// the history recorded them and then does what apply does: the same plan,
// confirmation and flags. Declined or failed, the host file stays as it is.
// The modules themselves are not touched: it says when they have changed
// since.
func (a *app) cmdRollback(args []string) error {
	var o opts
	fs := a.flags("rollback", &o, true, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: arcus rollback [-f] [-y] [-n] <entry>")
	}
	rev := fs.Arg(0)
	if !a.history {
		return fmt.Errorf("arcus rollback needs git: %w", history.ErrNoGit)
	}
	// The edit runs under the user lock, before anything changes.
	return a.change(&o, true, func(f *host.File) error {
		repo, ok, err := a.historyRepo("rollback")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("no history yet")
		}
		full, err := repo.Resolve(rev)
		if err != nil {
			return err
		}
		short := full[:7]
		missing := false
		if _, err := os.Lstat(f.Path); errors.Is(err, iofs.ErrNotExist) {
			missing = true
		} else if _, err := os.Stat(f.Path); errors.Is(err, iofs.ErrNotExist) {
			// Saving would replace the link with a regular file.
			return fmt.Errorf("%s is a symlink to a missing file; restore its target first", f.Path)
		}
		data, ok, err := repo.Show(full, "host.toml")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("entry %s has no host file", short)
		}
		old, err := host.Parse(f.Path, data)
		if err != nil {
			return fmt.Errorf("the host file of entry %s: %w", short, err)
		}
		a.printHostChanges(f, old, short, missing)
		if e, err := repo.Entry(full); err == nil {
			a.printDotNote(e.Dot)
		}
		f.Profiles, f.Modules = old.Profiles, old.Modules
		a.subject = "rollback " + full
		a.confirmHost, a.recreateHost = true, missing
		return nil
	})
}

// printHostChanges shows what putting the host file's lists back as of an
// entry changes: entries it would drop ("-") and bring back ("+").
func (a *app) printHostChanges(now, old *host.File, rev string, missing bool) {
	var lines []string
	for _, l := range []struct {
		kind     string
		now, old []string
	}{{"profile", now.Profiles, old.Profiles}, {"module", now.Modules, old.Modules}} {
		before := len(lines)
		for _, name := range l.now {
			if !slices.Contains(l.old, name) {
				lines = append(lines, fmt.Sprintf("- %s %s", l.kind, name))
			}
		}
		for _, name := range l.old {
			if !slices.Contains(l.now, name) {
				lines = append(lines, fmt.Sprintf("+ %s %s", l.kind, name))
			}
		}
		if len(lines) == before && !slices.Equal(l.now, l.old) {
			lines = append(lines, fmt.Sprintf("~ %ss are listed in another order", l.kind))
		}
	}
	switch {
	case missing:
		fmt.Fprintf(a.stdout, "The host file %s is missing; it will be recreated as of %s.\n", now.Path, rev)
	case len(lines) == 0:
		fmt.Fprintf(a.stdout, "The host file already lists what it did at %s.\n", rev)
		return
	default:
		fmt.Fprintf(a.stdout, "Host file %s, as of %s:\n", now.Path, rev)
	}
	for _, line := range lines {
		fmt.Fprintf(a.stdout, "        %s\n", line)
	}
}

// printDotNote says whether the config directory's repository has moved on
// since the entry: rollback restores the host file, not the modules.
func (a *app) printDotNote(trailer string) {
	then, thenDirty := dotOf(trailer)
	now, nowDirty, ok := (history.Git{Euid: a.euid}).Dot(a.paths.Config)
	switch {
	case then == "" && !ok:
	case then == "" || !ok || thenDirty || nowDirty:
		fmt.Fprintln(a.stdout, "Can't compare the modules with what they were then (no commit recorded, or uncommitted changes).")
	case then != now:
		fmt.Fprintf(a.stdout, "The modules were at %s then and are at %s now; rollback doesn't change them.\n", then[:7], now[:7])
	}
}
