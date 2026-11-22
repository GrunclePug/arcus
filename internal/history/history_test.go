package history

import (
	"crypto/sha256"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func needGit(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("git not on PATH")
	}
	if os.Geteuid() == 0 {
		t.Skip("git is refused as root")
	}
}

// isolate keeps the developer's git configuration out of the test's own
// git calls.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	needGit(t)
	isolate(t)
	return &Repo{Dir: filepath.Join(t.TempDir(), "history"), UID: os.Getuid(), Git: Git{Euid: os.Geteuid()}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// git runs plain git for the test's own checks.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// tree returns what the latest entry holds: path to content.
func tree(t *testing.T, r *Repo) map[string]string {
	t.Helper()
	got := map[string]string{}
	list := git(t, r.Dir, "ls-tree", "-r", "--name-only", Branch)
	for path := range strings.SplitSeq(list, "\n") {
		if path != "" {
			got[path] = git(t, r.Dir, "cat-file", "blob", Branch+":"+path)
		}
	}
	return got
}

func count(t *testing.T, r *Repo) int {
	t.Helper()
	n, err := strconv.Atoi(git(t, r.Dir, "rev-list", "--count", Branch))
	must(t, err)
	return n
}

func TestCommitHoldsExactlyTheFiles(t *testing.T) {
	r := newRepo(t)
	if st, err := r.Status(); err != nil || st != Absent {
		t.Fatalf("status = %v, %v; want absent", st, err)
	}
	files := map[string][]byte{"host.toml": []byte("modules = [\"a\"]"), "ledger/a.json": []byte("{}"), "system/state.json": []byte("{\"x\":1}")}
	made, err := r.Commit(files, "first", false)
	if err != nil || !made {
		t.Fatalf("commit = %v, %v", made, err)
	}
	want := map[string]string{"host.toml": "modules = [\"a\"]", "ledger/a.json": "{}", "system/state.json": "{\"x\":1}"}
	if got := tree(t, r); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
	info, err := os.Stat(r.Dir)
	must(t, err)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", info.Mode().Perm())
	}

	// Unchanged: nothing, unless asked.
	if made, err := r.Commit(files, "same", false); err != nil || made {
		t.Errorf("unchanged commit = %v, %v; want none", made, err)
	}
	if made, err := r.Commit(files, "same\n\nArcus: dev\n", true); err != nil || !made {
		t.Errorf("empty commit = %v, %v", made, err)
	}
	if got := git(t, r.Dir, "log", "-1", "--format=%s|%(trailers:key=Arcus,valueonly,separator=)|%an", Branch); got != "same|dev|arcus" {
		t.Errorf("latest = %q", got)
	}

	// A removed ledger disappears; an empty set commits.
	delete(files, "ledger/a.json")
	_, err = r.Commit(files, "removed", false)
	must(t, err)
	if _, ok := tree(t, r)["ledger/a.json"]; ok {
		t.Error("removed ledger still recorded")
	}
	_, err = r.Commit(nil, "empty", false)
	must(t, err)
	if got := tree(t, r); len(got) != 0 {
		t.Errorf("empty set recorded %v", got)
	}
	if n := count(t, r); n != 4 {
		t.Errorf("%d entries, want 4", n)
	}
	entries, err := r.Log(10)
	must(t, err)
	if len(entries) != 4 || entries[0].Subject != "empty" || entries[3].Subject != "first" {
		t.Errorf("log = %+v", entries)
	}
	if left, _ := filepath.Glob(filepath.Join(r.Dir, "arcus-index-*")); len(left) > 0 {
		t.Errorf("index left behind: %v", left)
	}
}

func TestEmptySetOnAnUnbornRepositoryIsNothing(t *testing.T) {
	r := newRepo(t)
	if made, err := r.Commit(nil, "history starts", false); err != nil || made {
		t.Fatalf("commit = %v, %v; want none", made, err)
	}
	if _, err := os.Lstat(r.Dir); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("repository created for nothing: %v", err)
	}
	if made, err := r.Commit(nil, "on x", true); err != nil || !made {
		t.Fatalf("command's commit = %v, %v", made, err)
	}
	if n := count(t, r); n != 1 {
		t.Errorf("%d entries, want 1", n)
	}
}

func TestUnbornAndKilledInitAreInitialised(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"unborn", func(t *testing.T, dir string) {
			r := &Repo{Dir: dir, UID: os.Getuid(), Git: Git{Euid: os.Geteuid()}}
			must(t, r.init())
		}},
		{"HEAD only", func(t *testing.T, dir string) {
			must(t, os.Mkdir(dir, 0o700))
			must(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/arcus\n"), 0o644))
		}},
		{"empty dir", func(t *testing.T, dir string) { must(t, os.Mkdir(dir, 0o700)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			tc.setup(t, r.Dir)
			if st, err := r.Status(); err != nil || st != Unborn {
				t.Fatalf("status = %v, %v; want unborn", st, err)
			}
			if made, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "history starts", false); err != nil || !made {
				t.Fatalf("commit = %v, %v", made, err)
			}
			if st, err := r.Status(); err != nil || st != Healthy {
				t.Errorf("status = %v, %v; want healthy", st, err)
			}
		})
	}
}

func TestBrokenRepositories(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, r *Repo)
	}{
		{"missing tree", func(t *testing.T, r *Repo) {
			tr := git(t, r.Dir, "rev-parse", Branch+"^{tree}")
			must(t, os.Remove(filepath.Join(r.Dir, "objects", tr[:2], tr[2:])))
		}},
		{"corrupt branch", func(t *testing.T, r *Repo) {
			must(t, os.WriteFile(filepath.Join(r.Dir, "refs", "heads", "arcus"), []byte("garbage\n"), 0o644))
		}},
		{"malformed HEAD", func(t *testing.T, r *Repo) {
			must(t, os.WriteFile(filepath.Join(r.Dir, "HEAD"), []byte("garbage\n"), 0o644))
		}},
		{"malformed HEAD, unreadable refs", func(t *testing.T, r *Repo) {
			must(t, os.WriteFile(filepath.Join(r.Dir, "HEAD"), []byte("garbage\n"), 0o644))
			heads := filepath.Join(r.Dir, "refs", "heads")
			must(t, os.Chmod(heads, 0))
			t.Cleanup(func() { os.Chmod(heads, 0o755) })
		}},
		{"detached HEAD", func(t *testing.T, r *Repo) {
			c := git(t, r.Dir, "rev-parse", Branch)
			must(t, os.WriteFile(filepath.Join(r.Dir, "HEAD"), []byte(c+"\n"), 0o644))
		}},
		{"HEAD on another branch", func(t *testing.T, r *Repo) {
			git(t, r.Dir, "update-ref", "refs/heads/other", Branch)
			git(t, r.Dir, "symbolic-ref", "HEAD", "refs/heads/other")
		}},
		{"HEAD through an alias", func(t *testing.T, r *Repo) {
			git(t, r.Dir, "symbolic-ref", "refs/heads/alias", Branch)
			git(t, r.Dir, "symbolic-ref", "HEAD", "refs/heads/alias")
		}},
		{"branch at a tree", func(t *testing.T, r *Repo) {
			tr := git(t, r.Dir, "rev-parse", Branch+"^{tree}")
			must(t, os.WriteFile(filepath.Join(r.Dir, "refs", "heads", "arcus"), []byte(tr+"\n"), 0o644))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			_, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "first", false)
			must(t, err)
			tc.break_(t, r)
			refs := refFiles(t, r)
			_, err = r.Status()
			if _, ok := errors.AsType[*BrokenError](err); !ok {
				t.Fatalf("status error = %v; want broken", err)
			}
			if _, err := r.Commit(map[string][]byte{"host.toml": []byte("y")}, "second", false); err == nil {
				t.Error("committed to a broken repository")
			}
			if got := refFiles(t, r); got != refs {
				t.Errorf("refs changed:\n%s\nwant\n%s", got, refs)
			}
		})
	}
}

// refFiles describes HEAD and every ref file.
func refFiles(t *testing.T, r *Repo) string {
	t.Helper()
	var b strings.Builder
	for _, top := range []string{"HEAD", "refs", "packed-refs"} {
		filepath.WalkDir(filepath.Join(r.Dir, top), func(p string, d iofs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				data, _ := os.ReadFile(p)
				fmt.Fprintf(&b, "%s: %s", p, data)
			}
			return nil
		})
	}
	return b.String()
}

// digest describes every file under dir.
func digest(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	must(t, filepath.WalkDir(dir, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s %x\n", p, sha256.Sum256(data))
		}
		return nil
	}))
	return b.String()
}

func TestIsolatedFromTheEnvironmentAndConfiguration(t *testing.T) {
	r := newRepo(t)
	// A second repository the environment points at.
	other := t.TempDir()
	git(t, other, "init", "-q")
	must(t, os.WriteFile(filepath.Join(other, "f"), []byte("f"), 0o644))
	git(t, other, "add", "f")
	git(t, other, "commit", "-q", "-m", "x")
	before := digest(t, other)

	// Hooks, from a global configuration and from the repository.
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	must(t, os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte(hook), 0o755))
	global := filepath.Join(os.Getenv("HOME"), ".gitconfig")
	must(t, os.WriteFile(global, []byte("[user]\n\tname = Wrong\n\temail = wrong@wrong\n[core]\n\thooksPath = "+hooks+"\n\tfsync = nonsense\n[commit]\n\tgpgSign = true\n[init]\n\tdefaultObjectFormat = sha256\n"), 0o644))

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_AUTHOR_NAME", "Wrong")
	_, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "first", false)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(r.Dir, "hooks"), 0o755))
	must(t, os.WriteFile(filepath.Join(r.Dir, "hooks", "reference-transaction"), []byte(hook), 0o755))
	_, err = r.Commit(map[string][]byte{"host.toml": []byte("y")}, "second", false)
	must(t, err)

	for _, k := range []string{"GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_AUTHOR_NAME"} {
		os.Unsetenv(k)
	}
	if got := digest(t, other); got != before {
		t.Error("the repository the environment named changed")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a hook ran")
	}
	if got := git(t, r.Dir, "rev-parse", Branch); len(got) != 40 {
		t.Errorf("the global configuration chose the object format: %s", got)
	}
	if got := git(t, r.Dir, "log", "--format=%an %cn", Branch); got != "arcus arcus\narcus arcus" {
		t.Errorf("identities = %q", got)
	}
}

func TestStaleLocks(t *testing.T) {
	for _, lock := range locks[:2] {
		t.Run(lock, func(t *testing.T) {
			r := newRepo(t)
			_, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "first", false)
			must(t, err)
			path := filepath.Join(r.Dir, lock)
			must(t, os.WriteFile(path, nil, 0o644))
			_, err = r.Commit(map[string][]byte{"host.toml": []byte("y")}, "second", false)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "doctor -fix") {
				t.Fatalf("commit error = %v; want one naming %s", err, path)
			}
			if got := r.StaleLocks(); !slices.Equal(got, []string{path}) {
				t.Errorf("stale locks = %v", got)
			}
			removed, err := r.RemoveStaleLocks()
			if err != nil || !slices.Equal(removed, []string{path}) {
				t.Errorf("removed %v, %v", removed, err)
			}
			if _, err := r.Commit(map[string][]byte{"host.toml": []byte("y")}, "second", false); err != nil {
				t.Errorf("commit after removal: %v", err)
			}
		})
	}
}

func TestLeftoverIndexesDontBlockAndAreSwept(t *testing.T) {
	r := newRepo(t)
	_, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "first", false)
	must(t, err)
	left := filepath.Join(r.Dir, "arcus-index-123")
	must(t, os.MkdirAll(left, 0o700))
	must(t, os.WriteFile(filepath.Join(left, "index.lock"), nil, 0o644))
	if _, err := r.Commit(map[string][]byte{"host.toml": []byte("y")}, "second", false); err != nil {
		t.Fatalf("a leftover index blocked: %v", err)
	}
	r.SweepIndexes()
	if _, err := os.Stat(left); !errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("leftover index not swept: %v", err)
	}
}

func TestCheckRefusesAndFixesTheMode(t *testing.T) {
	r := newRepo(t)
	_, err := r.Commit(map[string][]byte{"host.toml": []byte("x")}, "first", false)
	must(t, err)
	must(t, os.Chmod(r.Dir, 0o755))
	if wrong, err := r.Check(false); err != nil || !wrong {
		t.Errorf("check without fix = %v, %v; want a wrong mode", wrong, err)
	}
	if info, _ := os.Stat(r.Dir); info.Mode().Perm() != 0o755 {
		t.Error("check without fix changed the mode")
	}
	if wrong, err := r.Check(true); err != nil || wrong {
		t.Errorf("check with fix = %v, %v", wrong, err)
	}
	if info, _ := os.Stat(r.Dir); info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v after fix", info.Mode().Perm())
	}

	other := *r
	other.UID = os.Getuid() + 1
	if _, err := other.Check(true); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Errorf("another owner: %v", err)
	}

	link := *r
	link.Dir = filepath.Join(t.TempDir(), "history")
	must(t, os.Symlink(r.Dir, link.Dir))
	if _, err := link.Check(true); err == nil {
		t.Error("a symlinked history was accepted")
	}
	file := *r
	file.Dir = filepath.Join(t.TempDir(), "history")
	must(t, os.WriteFile(file.Dir, nil, 0o600))
	if _, err := file.Check(true); err == nil {
		t.Error("a file was accepted as the history")
	}
}

func TestResolveAndShow(t *testing.T) {
	r := newRepo(t)
	_, err := r.Commit(map[string][]byte{"host.toml": []byte("one")}, "first", false)
	must(t, err)
	first := git(t, r.Dir, "rev-parse", Branch)
	_, err = r.Commit(map[string][]byte{"state.json": []byte("{}")}, "second", false)
	must(t, err)

	for _, bad := range []string{"", "-n", "--all", "HEAD:host.toml", "nope", first + "^{tree}"} {
		if got, err := r.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) = %s", bad, got)
		}
	}
	full, err := r.Resolve(first[:7])
	if err != nil || full != first {
		t.Fatalf("Resolve(short) = %s, %v", full, err)
	}
	data, ok, err := r.Show(full, "host.toml")
	if err != nil || !ok || string(data) != "one" {
		t.Errorf("Show(first) = %q, %v, %v", data, ok, err)
	}
	head, err := r.Resolve("HEAD")
	must(t, err)
	if _, ok, err := r.Show(head, "host.toml"); ok || err != nil {
		t.Errorf("Show of a file the entry lacks = %v, %v", ok, err)
	}
}

func TestRefusesRoot(t *testing.T) {
	needGit(t)
	if _, err := (Git{Euid: 0}).run(t.TempDir(), nil, nil, "version"); err == nil {
		t.Error("git ran as root")
	}
	if _, _, ok := (Git{Euid: 0}).Dot(t.TempDir()); ok {
		t.Error("dot probed as root")
	}
}

func TestAvailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if Available() {
		t.Error("git found on an empty PATH")
	}
}

// fakeGit puts a git script first on PATH.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	must(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// gone waits for a process to be gone or a zombie.
func gone(pid int) bool {
	for range 100 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			if _, rest, ok := strings.Cut(string(stat), ") "); ok && strings.HasPrefix(rest, "Z") {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	must(t, err)
	return pid
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("git is refused as root")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	fakeGit(t, "sleep 30 &\necho $! > "+pidFile+"\nsleep 30\n")
	start := time.Now()
	_, err := (Git{Euid: os.Geteuid(), Timeout: 300 * time.Millisecond, WaitDelay: 100 * time.Millisecond}).run(t.TempDir(), nil, nil, "gc")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v; want a timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	if !gone(readPid(t, pidFile)) {
		t.Error("git's child survived the timeout")
	}
}

func TestADescendantHoldingTheOutputIsKilled(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("git is refused as root")
	}
	for _, status := range []int{0, 7} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			fakeGit(t, fmt.Sprintf("sleep 30 &\necho $! > %s\nexit %d\n", pidFile, status))
			_, err := (Git{Euid: os.Geteuid(), WaitDelay: 100 * time.Millisecond}).run(t.TempDir(), nil, nil, "gc")
			if (err == nil) != (status == 0) {
				t.Errorf("err = %v for exit status %d", err, status)
			}
			if !gone(readPid(t, pidFile)) {
				t.Error("a descendant survived the call")
			}
		})
	}
}

func TestChildrenGetNoTerminalAndTheLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("git is refused as root")
	}
	fakeGit(t, "readlink /proc/self/fd/0\nreadlink /proc/self/fd/3\n")
	lock, err := os.Create(filepath.Join(t.TempDir(), ".lock"))
	must(t, err)
	defer lock.Close()
	out, err := (Git{Euid: os.Geteuid(), Files: []*os.File{lock}}).run(t.TempDir(), nil, nil, "x")
	must(t, err)
	if got, want := string(out), "/dev/null\n"+lock.Name()+"\n"; got != want {
		t.Errorf("fds = %q, want %q", got, want)
	}
}

// dotRepo makes a dot-like repository with config/arcus tracked in it.
func dotRepo(t *testing.T) (repo, config string) {
	t.Helper()
	needGit(t)
	isolate(t)
	repo = t.TempDir()
	config = filepath.Join(repo, "config", "arcus")
	must(t, os.MkdirAll(filepath.Join(config, "modules", "a"), 0o755))
	must(t, os.WriteFile(filepath.Join(config, "modules", "a", "module.toml"), []byte("x"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "README"), []byte("r"), 0o644))
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "x")
	return repo, config
}

func TestDot(t *testing.T) {
	g := Git{Euid: os.Geteuid()}
	t.Run("clean", func(t *testing.T) {
		repo, config := dotRepo(t)
		hash, dirty, ok := g.Dot(config)
		if !ok || dirty || hash != git(t, repo, "rev-parse", "HEAD") {
			t.Errorf("Dot = %s, %v, %v", hash, dirty, ok)
		}
	})
	t.Run("a change outside the config directory", func(t *testing.T) {
		repo, config := dotRepo(t)
		must(t, os.WriteFile(filepath.Join(repo, "README"), []byte("changed"), 0o644))
		must(t, os.WriteFile(filepath.Join(repo, "new"), []byte("n"), 0o644))
		if _, dirty, ok := g.Dot(config); !ok || dirty {
			t.Errorf("Dot = %v, %v; want clean", dirty, ok)
		}
	})
	t.Run("untracked module, untracked files hidden", func(t *testing.T) {
		repo, config := dotRepo(t)
		git(t, repo, "config", "status.showUntrackedFiles", "no")
		must(t, os.MkdirAll(filepath.Join(config, "modules", "b"), 0o755))
		must(t, os.WriteFile(filepath.Join(config, "modules", "b", "module.toml"), []byte("y"), 0o644))
		if _, dirty, ok := g.Dot(config); !ok || !dirty {
			t.Errorf("Dot = %v, %v; want dirty", dirty, ok)
		}
	})
	t.Run("stat-dirty file leaves the index alone", func(t *testing.T) {
		repo, config := dotRepo(t)
		index := filepath.Join(repo, ".git", "index")
		before, err := os.ReadFile(index)
		must(t, err)
		future := time.Now().Add(time.Hour)
		must(t, os.Chtimes(filepath.Join(config, "modules", "a", "module.toml"), future, future))
		if _, dirty, ok := g.Dot(config); !ok || dirty {
			t.Errorf("Dot = %v, %v; want clean", dirty, ok)
		}
		after, err := os.ReadFile(index)
		must(t, err)
		if string(after) != string(before) {
			t.Error("the dot index was rewritten")
		}
	})
	t.Run("last tracked file staged for deletion", func(t *testing.T) {
		repo, config := dotRepo(t)
		git(t, repo, "rm", "-q", "--cached", "config/arcus/modules/a/module.toml")
		if _, dirty, ok := g.Dot(config); !ok || !dirty {
			t.Errorf("Dot = %v, %v; want tracked and dirty", dirty, ok)
		}
	})
	t.Run("not in git", func(t *testing.T) {
		isolate(t)
		if _, _, ok := g.Dot(t.TempDir()); ok {
			t.Error("a directory outside git has a commit")
		}
	})
	t.Run("ignored in a parent repository", func(t *testing.T) {
		repo, _ := dotRepo(t)
		ignored := filepath.Join(repo, "ignored", "arcus")
		must(t, os.MkdirAll(ignored, 0o755))
		must(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/ignored/\n"), 0o644))
		if _, _, ok := g.Dot(ignored); ok {
			t.Error("an ignored directory has a commit")
		}
	})
	t.Run("unborn", func(t *testing.T) {
		isolate(t)
		dir := t.TempDir()
		git(t, dir, "init", "-q")
		if _, _, ok := g.Dot(dir); ok {
			t.Error("an unborn repository has a commit")
		}
	})
}
