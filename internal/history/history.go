// Package history records arcus's state in a bare git repository: each
// commit is built from the bytes arcus read, so git never looks at a work
// tree, an index or the user's configuration. It also reads, without
// changing anything, which commit of the config directory's own repository
// the modules came from.
package history

import (
	"bytes"
	"errors"
	"fmt"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Branch is the one branch the history keeps.
const Branch = "refs/heads/arcus"

// Status is what a history directory holds.
type Status int

const (
	Absent  Status = iota // no repository
	Unborn                // no commit yet, or an init that was cut short
	Healthy               // the branch points at a commit with a readable tree
)

// BrokenError is a repository arcus doesn't write to or read from.
type BrokenError struct {
	Dir, Reason string
}

func (e *BrokenError) Error() string {
	return fmt.Sprintf("the history in %s is broken: %s", e.Dir, e.Reason)
}

// Repo is the history repository.
type Repo struct {
	Dir string
	UID int // the owner it must have: the user's real uid
	Git Git
}

// identity and isolation for every call: no GIT_* variable from outside,
// no global or system configuration, a fixed author.
func (r *Repo) env(extra ...string) []string {
	return append([]string{
		"GIT_DIR=" + r.Dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=arcus", "GIT_AUTHOR_EMAIL=arcus@localhost",
		"GIT_COMMITTER_NAME=arcus", "GIT_COMMITTER_EMAIL=arcus@localhost",
	}, extra...)
}

// git runs git on the repository. Hooks never run (update-ref would run
// reference-transaction) and objects and refs are synced to disk.
func (r *Repo) git(stdin []byte, env []string, args ...string) ([]byte, error) {
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsync=committed,reference"}, args...)
	return r.Git.run(r.Dir, r.env(env...), stdin, args...)
}

// out runs git and returns its output without the final newline.
func (r *Repo) out(args ...string) (string, error) {
	out, err := r.git(nil, nil, args...)
	return strings.TrimSuffix(string(out), "\n"), err
}

// Check refuses a history path that is anything but a directory the user
// owns: a symlink could point the history somewhere else. With fix, a mode
// other than 0700 is set back, since a symlinked host file may be private;
// without, wrongMode reports it.
func (r *Repo) Check(fix bool) (wrongMode bool, err error) {
	info, err := os.Lstat(r.Dir)
	if errors.Is(err, iofs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory; arcus keeps no history there", r.Dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != r.UID {
		return false, fmt.Errorf("%s belongs to another user; arcus keeps no history there", r.Dir)
	}
	if info.Mode().Perm() == 0o700 {
		return false, nil
	}
	if !fix {
		return true, nil
	}
	return false, os.Chmod(r.Dir, 0o700)
}

// Status reports what the repository holds. A broken one is an error.
func (r *Repo) Status() (Status, error) {
	if _, err := os.Lstat(r.Dir); errors.Is(err, iofs.ErrNotExist) {
		return Absent, nil
	}
	if _, err := r.git(nil, nil, "rev-parse", "--git-dir"); err != nil {
		if exitCode(err) < 0 {
			return 0, err
		}
		// An init cut short has nothing to lose: init never writes the
		// branch. With the branch there, the repository is damaged.
		for _, ref := range []string{filepath.Join("refs", "heads", "arcus"), "packed-refs"} {
			if _, err := os.Lstat(filepath.Join(r.Dir, ref)); !errors.Is(err, iofs.ErrNotExist) {
				return 0, &BrokenError{r.Dir, "git doesn't recognise it as a repository"}
			}
		}
		return Unborn, nil
	}
	head, err := r.out("symbolic-ref", "-q", "--no-recurse", "HEAD")
	if exitCode(err) < 0 && err != nil {
		return 0, err
	}
	if err != nil || head != Branch {
		return 0, &BrokenError{r.Dir, "HEAD doesn't point at " + Branch}
	}
	switch _, err := r.git(nil, nil, "show-ref", "--exists", Branch); exitCode(err) {
	case 2:
		return Unborn, nil
	case -1:
		if err != nil {
			return 0, err
		}
	default:
		return 0, &BrokenError{r.Dir, Branch + " can't be read"}
	}
	if _, err := r.out("rev-parse", "-q", "--verify", Branch+"^{commit}"); err != nil {
		return 0, &BrokenError{r.Dir, Branch + " doesn't point at a commit"}
	}
	tree, err := r.out("rev-parse", "-q", "--verify", Branch+"^{tree}")
	if err == nil {
		_, err = r.git(nil, nil, "cat-file", "-e", tree)
	}
	if err != nil {
		return 0, &BrokenError{r.Dir, "the latest entry's tree is missing"}
	}
	return Healthy, nil
}

// init creates the repository, or finishes one an init cut short (git init
// keeps what is already there).
func (r *Repo) init() error {
	if err := os.Mkdir(r.Dir, 0o700); err != nil && !errors.Is(err, iofs.ErrExist) {
		return err
	}
	if err := os.Chmod(r.Dir, 0o700); err != nil {
		return err
	}
	_, err := r.git(nil, []string{"GIT_DEFAULT_REF_FORMAT=files"}, "init", "-q", "--bare", "-b", "arcus", "--template=")
	return err
}

// Commit records files (path to bytes) as the next entry, with message.
// Unless empty is set, it records nothing when files are what the latest
// entry holds; on a repository with no entry yet, an empty set is nothing.
// It reports whether it made a commit.
func (r *Repo) Commit(files map[string][]byte, message string, empty bool) (bool, error) {
	made, err := r.commit(files, message, empty)
	if err != nil {
		if locks := r.StaleLocks(); len(locks) > 0 {
			err = fmt.Errorf("%w (%s, left by a killed arcus: remove it when no arcus is running, or run 'arcus doctor -fix')", err, strings.Join(locks, ", "))
		}
	}
	return made, err
}

func (r *Repo) commit(files map[string][]byte, message string, empty bool) (bool, error) {
	st, err := r.Status()
	if err != nil {
		return false, err
	}
	if st != Healthy {
		if len(files) == 0 && !empty {
			return false, nil
		}
		if err := r.init(); err != nil {
			return false, fmt.Errorf("creating the history: %w", err)
		}
	}

	// A private index per commit, so a stale one never blocks; git
	// rejects an empty file as an index, so it is a path in a new directory.
	tmp, err := os.MkdirTemp(r.Dir, "arcus-index-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	var info bytes.Buffer
	for _, path := range slices.Sorted(maps.Keys(files)) {
		oid, err := r.git(files[path], nil, "hash-object", "-w", "--stdin")
		if err != nil {
			return false, err
		}
		fmt.Fprintf(&info, "100644 blob %s\t%s\x00", bytes.TrimSpace(oid), path)
	}
	if info.Len() > 0 {
		if _, err := r.git(info.Bytes(), index, "update-index", "-z", "--index-info"); err != nil {
			return false, err
		}
	}
	tree, err := r.git(nil, index, "write-tree")
	if err != nil {
		return false, err
	}
	treeID := strings.TrimSpace(string(tree))

	args := []string{"commit-tree", "--no-gpg-sign", treeID}
	old := ""
	if st == Healthy {
		if old, err = r.out("rev-parse", "-q", "--verify", Branch+"^{commit}"); err != nil {
			return false, err
		}
		head, err := r.out("rev-parse", "-q", "--verify", Branch+"^{tree}")
		if err != nil {
			return false, err
		}
		if head == treeID && !empty {
			return false, nil
		}
		args = append(args, "-p", old)
	}
	commit, err := r.git([]byte(message), nil, args...)
	if err != nil {
		return false, err
	}
	if _, err := r.git(nil, nil, "update-ref", "--no-deref", Branch, strings.TrimSpace(string(commit)), old); err != nil {
		return false, err
	}
	return true, nil
}

// GC packs loose objects when git thinks it's time, in the foreground, so no
// git process outlives the command.
func (r *Repo) GC() error {
	_, err := r.git(nil, nil, "-c", "gc.autoDetach=false", "gc", "--auto", "--quiet")
	return err
}

// Entry is one recorded command.
type Entry struct {
	Hash    string
	Time    time.Time
	Subject string
	Dot     string // the Dot trailer: "<hash>" or "<hash> dirty", or empty
}

// Log returns the latest n entries, newest first.
func (r *Repo) Log(n int) ([]Entry, error) {
	return r.log("-n", strconv.Itoa(n), Branch)
}

// Entry returns the entry of a full commit hash.
func (r *Repo) Entry(commit string) (Entry, error) {
	entries, err := r.log("-n", "1", commit)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) != 1 {
		return Entry{}, fmt.Errorf("no entry %s", commit)
	}
	return entries[0], nil
}

func (r *Repo) log(args ...string) ([]Entry, error) {
	format := "--format=%H%x1f%ct%x1f%s%x1f%(trailers:key=Dot,valueonly,separator=%x20)%x1e"
	out, err := r.git(nil, nil, append([]string{"log", format}, append(args, "--")...)...)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for rec := range strings.SplitSeq(string(out), "\x1e") {
		f := strings.Split(strings.TrimLeft(rec, "\n"), "\x1f")
		if len(f) != 4 {
			continue
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("reading the history: %w", err)
		}
		entries = append(entries, Entry{Hash: f[0], Time: time.Unix(sec, 0), Subject: f[2], Dot: strings.TrimSpace(f[3])})
	}
	return entries, nil
}

// Resolve turns a revision into the full hash of a commit.
func (r *Repo) Resolve(rev string) (string, error) {
	bad := fmt.Errorf("%q is not an entry of the history ('arcus history' lists them)", rev)
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", bad
	}
	full, err := r.out("rev-parse", "-q", "--verify", "--end-of-options", rev+"^{commit}")
	if exitCode(err) >= 0 {
		return "", bad
	}
	return full, err
}

// Show returns a file as an entry recorded it, and whether it did.
func (r *Repo) Show(commit, path string) ([]byte, bool, error) {
	listed, err := r.git(nil, nil, "ls-tree", "-z", "--name-only", commit, "--", path)
	if err != nil || len(listed) == 0 {
		return nil, false, err
	}
	data, err := r.git(nil, nil, "cat-file", "blob", commit+":"+path)
	return data, err == nil, err
}

// locks a killed arcus can leave; each makes the next commit, or init, fail.
var locks = []string{"HEAD.lock", filepath.Join("refs", "heads", "arcus.lock"), "config.lock"}

// StaleLocks returns the lock files left in the repository. Only call it
// under the user lock: then no git of arcus's is running.
func (r *Repo) StaleLocks() []string {
	var found []string
	for _, l := range locks {
		path := filepath.Join(r.Dir, l)
		if _, err := os.Lstat(path); err == nil {
			found = append(found, path)
		}
	}
	return found
}

// RemoveStaleLocks removes them, under the user lock.
func (r *Repo) RemoveStaleLocks() ([]string, error) {
	var removed []string
	for _, path := range r.StaleLocks() {
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, path)
	}
	return removed, nil
}

// SweepIndexes removes the private indexes a killed arcus left, under the
// user lock.
func (r *Repo) SweepIndexes() {
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "arcus-index-") {
			os.RemoveAll(filepath.Join(r.Dir, e.Name()))
		}
	}
}

// Dot reports the commit of the git repository dir is in and whether dir
// differs from it (tracked changes, staged ones and untracked files), the
// way the user's own git status sees it. ok is false when dir isn't tracked
// in a repository with a commit, or git fails. It changes nothing in the
// repository: it never takes the index lock to refresh it.
func (g Git) Dot(dir string) (hash string, dirty, ok bool) {
	run := func(args ...string) ([]byte, error) {
		return g.run(dir, nil, nil, append([]string{"--no-optional-locks"}, args...)...)
	}
	out, err := run("rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return "", false, false
	}
	hash = strings.TrimSpace(string(out))
	// Tracked: in the index, or, when a staged deletion emptied the index
	// there, in HEAD.
	listed, err := run("ls-files", "-z", "--", ".")
	if err == nil && len(listed) == 0 {
		listed, err = run("ls-tree", "-r", "-z", "--name-only", "HEAD", "--", ".")
	}
	if err != nil || len(listed) == 0 {
		return "", false, false
	}
	status, err := run("status", "--porcelain", "-unormal", "--", ".")
	if err != nil {
		return "", false, false
	}
	return hash, len(status) > 0, true
}
