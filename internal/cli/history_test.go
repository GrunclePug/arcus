package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grunclepug/arcus/internal/layer"
	"github.com/grunclepug/arcus/internal/state"
)

// histEnv is an env with the state history on.
func histEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if os.Geteuid() == 0 {
		t.Skip("git is refused as root")
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	e := newEnv(t)
	e.history, e.version = true, "dev"
	return e
}

func (e *env) historyDir() string {
	return filepath.Join(e.home, ".local", "state", "arcus", "history")
}

// git runs git on the history repository for the test's own checks.
func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+e.historyDir(), "GIT_CONFIG_NOSYSTEM=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		e.t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// subjects returns the history's subjects, newest first.
func (e *env) subjects() []string {
	e.t.Helper()
	if _, err := os.Stat(e.historyDir()); err != nil {
		return nil
	}
	if exec.Command("git", "--git-dir", e.historyDir(), "rev-parse", "-q", "--verify", "refs/heads/arcus").Run() != nil {
		return nil
	}
	out := e.git("log", "--format=%s", "refs/heads/arcus")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

func (e *env) wantSubjects(want ...string) {
	e.t.Helper()
	if got := e.subjects(); !slices.Equal(got, want) {
		e.t.Errorf("history = %q, want %q", got, want)
	}
}

// trailer returns a trailer of the latest entry.
func (e *env) trailer(key string) string {
	e.t.Helper()
	return e.git("log", "-1", "--format=%(trailers:key="+key+",valueonly,separator=)", "refs/heads/arcus")
}

func (e *env) files(rev string) []string {
	e.t.Helper()
	return strings.Split(e.git("ls-tree", "-r", "--name-only", rev), "\n")
}

func TestHistoryRecordsCommands(t *testing.T) {
	e := histEnv(t)
	e.wlBase()
	e.module("tool", "[packages]\npacman = [\"tool\"]\n", map[string]string{"home/.toolrc": "t"})
	must(t, os.MkdirAll(filepath.Join(e.root, "etc/arcus/modules/machine"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.root, "etc/arcus/modules/machine/module.toml"), []byte("description = \"m\"\n"), 0o644))

	// State from before the history: its first entry says so.
	e.history = false
	must(t, e.run("on", "-y", "base"))
	e.history = true
	must(t, e.run("on", "-y", "tool"))
	e.wantSubjects("on -y tool", "history starts")
	if got := e.trailer("Arcus"); got != "dev" {
		t.Errorf("Arcus trailer = %q", got)
	}
	if got := e.git("log", "-1", "--format=%(trailers)", "refs/heads/arcus~1"); got != "" {
		t.Errorf("history starts has trailers: %q", got)
	}
	contains(t, e.files("refs/heads/arcus"), "host.toml", "state.json", "ledger/tool.json", "system/state.json", "machine/module.toml")
	if info, err := os.Stat(e.historyDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("history mode: %v %v", info, err)
	}

	// Dry runs and plans record nothing; nor does a declined apply.
	must(t, e.run("plan"))
	must(t, e.run("on", "-n", "wl-base"))
	must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"base\", \"tool\", \"wl-base\"]\n"), 0o644))
	if err := e.app("n\n").run([]string{"apply"}); err == nil {
		t.Fatal("declined apply succeeded")
	}
	e.wantSubjects("on -y tool", "history starts")

	// A hand edit is recorded on its own before the command.
	must(t, e.run("apply", "-y"))
	e.wantSubjects("apply -y", "unrecorded changes", "on -y tool", "history starts")
	if got := e.git("log", "-1", "--format=%(trailers)", "refs/heads/arcus~1"); got != "" {
		t.Errorf("unrecorded changes has trailers: %q", got)
	}

	// A failing command is recorded as failed.
	e.module("broken", "[packages]\npacman = [\"nope\"]\n", nil)
	e.sys.Fail("pacman")
	if err := e.run("on", "-y", "broken"); err == nil {
		t.Fatal("on with a failing pacman succeeded")
	}
	if got := e.subjects()[0]; got != "on -y broken (failed)" {
		t.Errorf("latest = %q", got)
	}
}

func TestFirstCommandOnNoStateIsTheFirstEntry(t *testing.T) {
	e := histEnv(t)
	e.module("tool", "", map[string]string{"home/.toolrc": "t"})
	must(t, e.run("on", "-y", "tool"))
	e.wantSubjects("on -y tool")
}

func TestFailedCommandWithACancelledContextIsRecorded(t *testing.T) {
	e := histEnv(t)
	e.module("tool", "[packages]\npacman = [\"tool\"]\n", nil)
	must(t, e.run("on", "-y", "tool"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.ctx = ctx
	if err := e.run("off", "-y", "tool"); err == nil {
		t.Skip("off ran despite the cancelled context")
	}
	if got := e.subjects()[0]; got != "off -y tool (failed)" {
		t.Errorf("latest = %q", got)
	}
}

func TestHostChangeWithAnEmptyPlanIsRecorded(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "requires = [\"a\"]\n", nil)
	must(t, e.run("on", "-y", "b"))
	// a is on already, through b: only the host file changes.
	must(t, e.run("on", "-y", "a"))
	e.wantSubjects("on -y a", "on -y b")
	must(t, e.run("apply", "-y")) // nothing to do: nothing recorded
	e.wantSubjects("on -y a", "on -y b")
}

// stdinThatLocks takes the system half's lock when the confirmation reads
// its answer, so the lock is busy from the point of no return on.
type stdinThatLocks struct {
	path string
	lock *state.Lock
	r    *strings.Reader
}

func (s *stdinThatLocks) Read(p []byte) (int, error) {
	if s.lock == nil {
		lock, err := state.AcquireLock(s.path, true)
		if err != nil {
			return 0, err
		}
		s.lock = lock
	}
	return s.r.Read(p)
}

func TestBusySystemLockSkipsTheEntryAndTheNextRecordsIt(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	e.module("s", "[packages]\npacman = [\"s\"]\n", nil)
	must(t, e.run("on", "-y", "s")) // the system lock file exists
	in := &stdinThatLocks{path: filepath.Join(e.root, "var/lib/arcus/.lock"), r: strings.NewReader("y\n")}
	a := e.app("")
	a.stdin = in
	e.out.Reset()
	must(t, a.run([]string{"on", "a"}))
	in.lock.Release()
	if !strings.Contains(e.out.String(), "this command isn't recorded") {
		t.Errorf("no warning:\n%s", e.out.String())
	}
	e.wantSubjects("on -y s")
	must(t, e.run("on", "-y", "b"))
	e.wantSubjects("on -y b", "unrecorded changes", "on -y s")
}

func TestRollback(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "[packages]\npacman = [\"bpkg\"]\n", map[string]string{"home/.b": "b"})
	must(t, os.WriteFile(e.hostPath(), []byte("# mine\nmodules = [\"a\"]\n"), 0o644))
	must(t, e.run("apply", "-y"))
	first := e.git("rev-parse", "refs/heads/arcus")
	must(t, e.run("on", "-y", "b"))

	// Declined: nothing changes.
	before, err := os.ReadFile(e.hostPath())
	must(t, err)
	if err := e.app("n\n").run([]string{"rollback", first[:8]}); err == nil {
		t.Fatal("declined rollback succeeded")
	}
	if now, _ := os.ReadFile(e.hostPath()); !bytes.Equal(now, before) {
		t.Error("declined rollback changed the host file")
	}
	e.wantSubjects("on -y b", "apply -y", "history starts")

	// -n shows the change and records nothing.
	must(t, e.run("rollback", "-n", first[:8]))
	if !strings.Contains(e.out.String(), "- module b") {
		t.Errorf("rollback -n:\n%s", e.out.String())
	}

	must(t, e.run("rollback", "-y", first[:8]))
	if got := e.hostModules(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("host modules = %v", got)
	}
	if now, _ := os.ReadFile(e.hostPath()); !strings.HasPrefix(string(now), "# mine\n") {
		t.Errorf("rollback lost the host file's comments: %q", now)
	}
	if e.read("home/u/.b") != "<missing>" || slices.Contains(e.sys.Packages(), "bpkg") {
		t.Error("rollback didn't apply")
	}
	e.wantSubjects("rollback "+first, "on -y b", "apply -y", "history starts")

	// Refused, with nothing changed.
	for _, rev := range []string{"nope", "-x", "HEAD:host.toml"} {
		if err := e.run("rollback", "-y", rev); err == nil {
			t.Errorf("rollback %s succeeded", rev)
		}
	}
	e.wantSubjects("rollback "+first, "on -y b", "apply -y", "history starts")
}

func TestRollbackReorderAndRecreate(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"a\", \"b\"]\n"), 0o644))
	must(t, e.run("apply", "-y"))
	ab := e.git("rev-parse", "refs/heads/arcus")
	must(t, os.WriteFile(e.hostPath(), []byte("modules = [\"b\", \"a\"]\n"), 0o644))
	must(t, e.run("apply", "-y"))

	// A reorder alone is confirmed, and a declined one changes nothing.
	if err := e.app("n\n").run([]string{"rollback", ab}); err == nil {
		t.Fatal("declined reorder succeeded")
	}
	if got := e.hostModules(); !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("host modules = %v", got)
	}
	must(t, e.run("rollback", "-y", ab))
	if got := e.hostModules(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("host modules = %v", got)
	}

	// An empty selection brings a missing host file back.
	must(t, os.WriteFile(e.hostPath(), []byte("modules = []\n"), 0o644))
	must(t, e.run("apply", "-y"))
	empty := e.git("rev-parse", "refs/heads/arcus")
	must(t, os.Remove(e.hostPath()))
	if err := e.app("n\n").run([]string{"rollback", empty}); err == nil {
		t.Fatal("declined recreate succeeded")
	}
	must(t, e.run("rollback", "-y", empty))
	if !strings.Contains(e.out.String(), "will be recreated") {
		t.Errorf("rollback output:\n%s", e.out.String())
	}
	if _, err := os.Stat(e.hostPath()); err != nil {
		t.Errorf("host file not recreated: %v", err)
	}
}

func TestRollbackRefusals(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("gone", "", nil)
	must(t, e.run("on", "-y", "a"))
	must(t, e.run("on", "-y", "gone"))
	withGone := e.git("rev-parse", "refs/heads/arcus")
	must(t, e.run("off", "-y", "gone"))
	must(t, os.RemoveAll(filepath.Join(e.home, ".config/arcus/modules/gone")))
	before, _ := os.ReadFile(e.hostPath())
	if err := e.run("rollback", "-y", withGone); err == nil {
		t.Error("rollback to a missing module succeeded")
	}
	if now, _ := os.ReadFile(e.hostPath()); !bytes.Equal(now, before) {
		t.Error("refused rollback changed the host file")
	}

	// An entry without a host file.
	a := e.app("")
	must(t, a.load(&opts{}))
	_, err := a.repo().Commit(map[string][]byte{"state.json": []byte("{}")}, "no host file", false)
	must(t, err)
	if err := e.run("rollback", "-y", e.git("rev-parse", "refs/heads/arcus")); err == nil || !strings.Contains(err.Error(), "no host file") {
		t.Errorf("rollback to an entry without a host file: %v", err)
	}
}

func TestRollbackThroughASymlinkedHostFile(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	private := filepath.Join(e.home, "private", "host.toml")
	must(t, os.MkdirAll(filepath.Dir(private), 0o700))
	must(t, os.WriteFile(private, []byte("modules = [\"a\"]\n"), 0o600))
	must(t, os.Symlink(private, e.hostPath()))
	must(t, e.run("apply", "-y"))
	first := e.git("rev-parse", "refs/heads/arcus")
	if got := e.git("cat-file", "blob", first+":host.toml"); got != "modules = [\"a\"]" {
		t.Errorf("recorded host file = %q", got)
	}
	must(t, e.run("on", "-y", "b"))
	must(t, e.run("rollback", "-y", first))
	if info, err := os.Lstat(e.hostPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the host file is no longer a link: %v", err)
	}
	if got := e.hostModules(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("host modules = %v", got)
	}

	// A dangling link is refused and survives.
	must(t, os.Remove(private))
	if err := e.run("rollback", "-y", first); err == nil || !strings.Contains(err.Error(), "restore its target first") {
		t.Errorf("rollback through a dangling link: %v", err)
	}
	if target, err := os.Readlink(e.hostPath()); err != nil || target != private {
		t.Errorf("link = %q, %v", target, err)
	}
}

func TestHistoryCommand(t *testing.T) {
	e := histEnv(t)
	must(t, e.run("history"))
	if !strings.Contains(e.out.String(), "No history yet") {
		t.Errorf("history before any entry:\n%s", e.out.String())
	}
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	must(t, e.run("on", "-y", "a"))
	must(t, e.run("on", "-y", "b"))
	must(t, e.run("history"))
	lines := strings.Split(strings.TrimSpace(e.out.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " on -y b") || !strings.HasSuffix(lines[1], " on -y a") {
		t.Errorf("history:\n%s", e.out.String())
	}
	must(t, e.run("history", "1"))
	if got := strings.Count(e.out.String(), "\n"); got != 1 {
		t.Errorf("history 1:\n%s", e.out.String())
	}
	if err := e.run("history", "0"); err == nil {
		t.Error("history 0 accepted")
	}

	// Broken: reported as broken, not as empty.
	must(t, os.WriteFile(filepath.Join(e.historyDir(), "HEAD"), []byte(e.git("rev-parse", "refs/heads/arcus")+"\n"), 0o644))
	if err := e.run("history"); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("history of a broken repository: %v", err)
	}
}

func TestHistoryModeAndStaleLocks(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	e.module("c", "", map[string]string{"home/.c": "c"})
	must(t, e.run("on", "-y", "a"))

	// A wrong mode: doctor and history leave it, a recording command fixes it.
	must(t, os.Chmod(e.historyDir(), 0o755))
	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), "mode 0700") {
		t.Errorf("doctor with a wrong mode (%v):\n%s", err, e.out.String())
	}
	must(t, e.run("history"))
	if info, _ := os.Stat(e.historyDir()); info.Mode().Perm() != 0o755 {
		t.Error("doctor or history changed the mode")
	}
	must(t, e.run("on", "-y", "b"))
	if info, _ := os.Stat(e.historyDir()); info.Mode().Perm() != 0o700 {
		t.Error("a recording command left the wrong mode")
	}

	// A stale lock: the command warns and isn't recorded; doctor -fix
	// removes it, without an entry of its own.
	lock := filepath.Join(e.historyDir(), "HEAD.lock")
	must(t, os.WriteFile(lock, nil, 0o644))
	must(t, e.run("on", "-y", "c"))
	if !strings.Contains(e.out.String(), lock) {
		t.Errorf("no warning naming the lock:\n%s", e.out.String())
	}
	e.wantSubjects("on -y b", "on -y a")
	if err := e.run("doctor"); err == nil || !strings.Contains(e.out.String(), "stale lock") {
		t.Errorf("doctor with a stale lock (%v):\n%s", err, e.out.String())
	}
	must(t, e.run("doctor", "-fix"))
	if _, err := os.Stat(lock); err == nil {
		t.Error("doctor -fix left the stale lock")
	}
	e.wantSubjects("on -y b", "on -y a")
	must(t, e.run("off", "-y", "c"))
	e.wantSubjects("off -y c", "unrecorded changes", "on -y b", "on -y a")
}

func TestKilledInitIsRepairedByDoctor(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	must(t, os.MkdirAll(e.historyDir(), 0o700))
	must(t, os.WriteFile(filepath.Join(e.historyDir(), "config.lock"), nil, 0o644))
	must(t, e.run("on", "-y", "a"))
	if !strings.Contains(e.out.String(), "isn't recorded") {
		t.Errorf("no warning:\n%s", e.out.String())
	}
	e.wantSubjects()
	_ = e.run("doctor", "-fix")
	must(t, e.run("on", "-y", "b"))
	e.wantSubjects("on -y b", "history starts")
}

func TestDoctorFixIsRecorded(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	must(t, e.run("on", "-y", "a"))

	// Nothing to repair: nothing recorded.
	must(t, e.run("doctor", "-fix"))
	e.wantSubjects("on -y a")

	// An interrupted turn-on of the home layer, with a stale lock too.
	e.module("orphan", "", map[string]string{"home/.orphan": "x"})
	dirs := map[string]string{"orphan": filepath.Join(e.home, ".config", "arcus", "modules", "orphan")}
	must(t, layer.NewHome(e.home, filepath.Join(e.home, ".local", "state", "arcus"), dirs).Activate(context.Background(), "orphan", nil))
	must(t, os.WriteFile(filepath.Join(e.historyDir(), "HEAD.lock"), nil, 0o644))
	if err := e.run("doctor", "-fix"); err != nil {
		t.Fatalf("doctor -fix: %v\n%s", err, e.out.String())
	}
	e.wantSubjects("doctor -fix", "unrecorded changes", "on -y a")
	if strings.Contains(e.subjects()[0], "failed") {
		t.Error("a successful repair recorded as failed")
	}
}

func TestDoctorFixCountsSystemRepairs(t *testing.T) {
	e := histEnv(t)
	e.module("p", "[packages]\npacman = [\"ppkg\", \"qpkg\"]\n", map[string]string{"system/etc/p.conf": "AAAA"})
	must(t, e.run("on", "-y", "p"))

	// Unfixable alone: no sudo, nothing recorded.
	must(t, os.WriteFile(filepath.Join(e.root, "etc/p.conf"), []byte("BBBB"), 0o644))
	e.sudos = 0
	if err := e.run("doctor", "-fix"); err == nil {
		t.Error("doctor -fix with an edited file succeeded")
	}
	if e.sudos != 0 {
		t.Errorf("sudo ran %d times for nothing to repair", e.sudos)
	}
	e.wantSubjects("on -y p")

	// One fixable (a package removed by hand) and one unfixable; the
	// package to reinstall is a finding of the host file.
	e.sys.SetPackages(slices.DeleteFunc(e.sys.Packages(), func(p string) bool { return p == "ppkg" })...)
	err := e.run("doctor", "-fix")
	out := e.out.String()
	if err == nil || !strings.Contains(out, "3 problem(s), 1 fixed.") || !strings.Contains(err.Error(), "2 problem(s)") {
		t.Errorf("doctor -fix = %v:\n%s", err, out)
	}
	e.wantSubjects("doctor -fix", "on -y p")

	// A repair that fails next to one that works: the one counted, the
	// run recorded as failed.
	e.sys.SetPackages(slices.DeleteFunc(e.sys.Packages(), func(p string) bool { return p == "qpkg" })...)
	ledger := filepath.Join(e.root, "var/lib/arcus/ledger/p.json")
	data, err := os.ReadFile(ledger)
	must(t, err)
	must(t, os.WriteFile(ledger, bytes.Replace(data, []byte(`"status": "active"`), []byte(`"status": "deactivating"`), 1), 0o644))
	err = e.run("doctor", "-fix")
	out = e.out.String()
	if err == nil || !strings.Contains(out, "1 fixed.") {
		t.Errorf("doctor -fix = %v:\n%s", err, out)
	}
	if got := e.subjects()[0]; got != "doctor -fix (failed)" {
		t.Errorf("latest = %q", got)
	}
}

func TestMvAndMachineAreRecorded(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("idle", "", nil)
	must(t, e.run("on", "-y", "a"))

	// Nothing selects or records idle: like new and rm, not recorded.
	must(t, e.run("mv", "idle", "idler"))
	e.wantSubjects("on -y a")
	if _, err := os.Stat(filepath.Join(e.home, ".local/state/arcus/state.json")); err != nil {
		t.Fatal(err)
	}
	must(t, e.run("mv", "a", "aa"))
	e.wantSubjects("mv a aa", "on -y a")
	if got := e.hostModules(); !slices.Equal(got, []string{"aa"}) {
		t.Errorf("host modules = %v", got)
	}
}

func TestUnrecordedMvCreatesNoState(t *testing.T) {
	e := histEnv(t)
	e.module("idle", "", nil)
	must(t, e.run("mv", "idle", "idler"))
	if _, err := os.Stat(filepath.Join(e.home, ".local/state/arcus/state.json")); err == nil {
		t.Error("mv of an unrecorded module created state.json")
	}
	e.wantSubjects()
}

// dotEnv links ~/.config/arcus into a git checkout, as dot does.
func dotEnv(t *testing.T) (e *env, checkout string) {
	t.Helper()
	e = histEnv(t)
	checkout = filepath.Join(e.root, "dot")
	config := filepath.Join(checkout, "config", "arcus")
	must(t, os.MkdirAll(config, 0o755))
	must(t, os.MkdirAll(filepath.Join(e.home, ".config"), 0o755))
	must(t, os.Symlink(config, filepath.Join(e.home, ".config", "arcus")))
	e.module("a", "", map[string]string{"home/.a": "a"})
	e.module("b", "", map[string]string{"home/.b": "b"})
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return e, checkout
}

func dotHead(t *testing.T, checkout string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = checkout
	out, err := cmd.Output()
	must(t, err)
	return strings.TrimSpace(string(out))
}

func TestDotTrailer(t *testing.T) {
	e, checkout := dotEnv(t)
	must(t, e.run("on", "-y", "a"))
	head := dotHead(t, checkout)
	if got := e.trailer("Dot"); got != head {
		t.Errorf("Dot = %q, want %s", got, head)
	}
	// arcus mv renames a module inside dot: the entry says it's dirty.
	must(t, e.run("mv", "a", "aa"))
	if got := e.trailer("Dot"); got != head+" dirty" {
		t.Errorf("Dot after mv = %q", got)
	}
	must(t, e.run("history"))
	if !strings.Contains(e.out.String(), "dot "+head[:7]+"*") {
		t.Errorf("history:\n%s", e.out.String())
	}
}

func TestRollbackNotesTheModulesMoved(t *testing.T) {
	e, checkout := dotEnv(t)
	must(t, e.run("on", "-y", "a"))
	first := e.git("rev-parse", "refs/heads/arcus")
	then := dotHead(t, checkout)
	must(t, os.WriteFile(filepath.Join(checkout, "config/arcus/modules/b/home/.b"), []byte("b2"), 0o644))
	cmd := exec.Command("git", "commit", "-q", "-am", "y")
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	must(t, cmd.Run())
	must(t, e.run("on", "-y", "b"))
	must(t, e.run("rollback", "-y", first))
	if want := "The modules were at " + then[:7]; !strings.Contains(e.out.String(), want) {
		t.Errorf("rollback output lacks %q:\n%s", want, e.out.String())
	}
}

func TestReadOnlyCommandsRunNoGit(t *testing.T) {
	e := histEnv(t)
	e.module("a", "", map[string]string{"home/.a": "a"})
	must(t, e.run("on", "-y", "a"))
	real, err := exec.LookPath("git")
	must(t, err)
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	must(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$@\" >> "+log+"\nexec "+real+" \"$@\"\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, args := range [][]string{{"ls"}, {"plan"}, {"apply", "-n"}, {"apply", "-y"}} {
		must(t, e.run(args...))
	}
	if data, err := os.ReadFile(log); err == nil {
		t.Errorf("git ran:\n%s", data)
	}
}
