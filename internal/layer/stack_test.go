package layer

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// sandbox is a throwaway target tree, module directories and state
// directory, all inside t.TempDir(). Test paths are relative to <target>/etc
// so the same test runs on both halves (the system half only allows /etc and
// /usr/local).
type sandbox struct {
	t       *testing.T
	root    string
	host    string // <target>/etc
	s       *Stacker
	stack   []string
	warns   []string
	profDir string
}

type half struct {
	name string
	new  func(target, stateDir string, dirs map[string]string) *Stacker
}

var halves = []half{{"home", NewHome}, {"system", NewSystem}}

func newSandbox(t *testing.T, h half) *sandbox {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	sb := &sandbox{t: t, root: root, host: filepath.Join(target, "etc"), profDir: filepath.Join(root, "modules")}
	sb.s = h.new(target, filepath.Join(root, "state"), map[string]string{})
	sb.s.Warn = func(format string, args ...any) { sb.warns = append(sb.warns, fmt.Sprintf(format, args...)) }
	must(t, os.MkdirAll(sb.host, 0o755))
	return sb
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// hostFile writes a file into the target tree.
func (sb *sandbox) hostFile(rel, content string) string {
	p := filepath.Join(sb.host, rel)
	must(sb.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(sb.t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// module registers a module directory and returns it.
func (sb *sandbox) module(name string) string {
	dir := filepath.Join(sb.profDir, name)
	sb.s.Dirs[name] = dir
	return dir
}

// moduleFile adds a file to the half's tree of a module, targeting etc/<rel>.
func (sb *sandbox) moduleFile(module, rel, content string) string {
	p := filepath.Join(sb.module(module), sb.s.Subdir, "etc", rel)
	must(sb.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(sb.t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func (sb *sandbox) activate(module string) error {
	sb.module(module)
	err := sb.s.Activate(context.Background(), module, sb.stack)
	if err == nil {
		sb.stack = append(sb.stack, module)
	}
	return err
}

func (sb *sandbox) deactivate(module string, force bool) error {
	err := sb.s.Deactivate(context.Background(), module, sb.stack, force)
	if err == nil {
		sb.stack = slices.DeleteFunc(sb.stack, func(s string) bool { return s == module })
	}
	return err
}

func (sb *sandbox) read(rel string) string {
	data, err := os.ReadFile(filepath.Join(sb.host, rel))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(data)
}

// snapshot captures every node under the target tree: type, mode, and
// content or link target.
func (sb *sandbox) snapshot() map[string]string {
	sb.t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(sb.host, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(sb.host, p)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// Relative to the sandbox, so snapshots of two sandboxes compare.
			target, _ := os.Readlink(p)
			snap[rel] = "link:" + strings.Replace(target, sb.root, "$ROOT", 1)
		case info.IsDir():
			snap[rel] = fmt.Sprintf("dir:%v", info.Mode().Perm())
		default:
			data, _ := os.ReadFile(p)
			snap[rel] = fmt.Sprintf("file:%v:%s", info.Mode().Perm(), data)
		}
		return nil
	})
	must(sb.t, err)
	return snap
}

func assertSame(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q, got %q", k, v, got[k])
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected leftover %s (%s)", k, v)
		}
	}
}

func (sb *sandbox) assertNoLedgers() {
	sb.t.Helper()
	names, err := sb.s.ListLedgers()
	must(sb.t, err)
	if len(names) != 0 {
		sb.t.Errorf("leftover ledgers: %v", names)
	}
}

func eachHalf(t *testing.T, fn func(t *testing.T, sb *sandbox)) {
	for _, h := range halves {
		t.Run(h.name, func(t *testing.T) { fn(t, newSandbox(t, h)) })
	}
}

func TestSingleLayerRoundTrip(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("foot/foot.ini", "host original")
		sb.moduleFile("A", "foot/foot.ini", "from A")
		sb.moduleFile("A", "new/deep/cfg", "new file")
		before := sb.snapshot()

		must(t, sb.activate("A"))
		if got := sb.read("foot/foot.ini"); got != "from A" {
			t.Fatalf("foot.ini = %q", got)
		}
		if got := sb.read("foot/foot.ini.orig"); got != "host original" {
			t.Fatalf("backup = %q", got)
		}

		must(t, sb.deactivate("A", false))
		assertSame(t, before, sb.snapshot()) // also proves created dirs were removed
		sb.assertNoLedgers()
	})
}

// Every teardown order of three layers sharing files, including removing one
// from the middle, restores the host exactly.
func TestStackedLayersAnyTeardownOrder(t *testing.T) {
	layers := []string{"a", "b", "c"}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}

	for _, h := range halves {
		for _, perm := range perms {
			order := []string{layers[perm[0]], layers[perm[1]], layers[perm[2]]}
			t.Run(h.name+"/"+strings.Join(order, ""), func(t *testing.T) {
				sb := newSandbox(t, h)
				sb.hostFile("shared.ini", "ORIGINAL")
				sb.hostFile("only-host", "untouched")
				sb.moduleFile("a", "shared.ini", "a")
				sb.moduleFile("b", "shared.ini", "b")
				sb.moduleFile("c", "shared.ini", "c")
				sb.moduleFile("a", "ab.conf", "a")
				sb.moduleFile("b", "ab.conf", "b")
				sb.moduleFile("c", "c-only/x", "c")
				before := sb.snapshot()

				for _, l := range layers {
					must(t, sb.activate(l))
				}

				for i, l := range order {
					must(t, sb.deactivate(l, false))
					// The highest remaining layer must be visible after each removal.
					if want := topmost(sb.stack, "ORIGINAL"); sb.read("shared.ini") != want {
						t.Fatalf("after removing %v: shared.ini = %q, want %q", order[:i+1], sb.read("shared.ini"), want)
					}
				}

				assertSame(t, before, sb.snapshot())
				sb.assertNoLedgers()
			})
		}
	}
}

func topmost(stack []string, fallback string) string {
	if len(stack) == 0 {
		return fallback
	}
	return stack[len(stack)-1]
}

// After any sequence of layers going on and off, every file shows the topmost
// layer that provides it, and the original comes back byte for byte once all
// are off.
func TestRandomOnOffSequences(t *testing.T) {
	provides := map[string][]string{
		"p": {"shared", "pq", "p-only/x"},
		"q": {"shared", "pq", "qr/y"},
		"r": {"shared", "qr/y", "new"},
		"s": {"shared", "p-only/x"},
	}
	names := []string{"p", "q", "r", "s"}

	for _, h := range halves {
		for seed := range uint64(5) {
			t.Run(fmt.Sprintf("%s/seed=%d", h.name, seed), func(t *testing.T) {
				sb := newSandbox(t, h)
				sb.hostFile("shared", "ORIGINAL")
				sb.hostFile("pq", "ORIGINAL")
				for p, files := range provides {
					for _, f := range files {
						sb.moduleFile(p, f, p)
					}
				}
				before := sb.snapshot()
				rng := rand.New(rand.NewPCG(seed, 0))

				check := func(step int) {
					t.Helper()
					for _, f := range []string{"shared", "pq", "qr/y", "p-only/x", "new"} {
						want := "<missing>"
						if f == "shared" || f == "pq" {
							want = "ORIGINAL"
						}
						for i := len(sb.stack) - 1; i >= 0; i-- {
							if slices.Contains(provides[sb.stack[i]], f) {
								want = sb.stack[i]
								break
							}
						}
						got := sb.read(f)
						if strings.HasPrefix(got, "<") {
							got = "<missing>"
						}
						if got != want {
							t.Fatalf("step %d, stack %v: %s = %q, want %q", step, sb.stack, f, got, want)
						}
					}
				}

				for step := range 200 {
					p := names[rng.IntN(len(names))]
					if slices.Contains(sb.stack, p) {
						must(t, sb.deactivate(p, false))
					} else {
						must(t, sb.activate(p))
					}
					check(step)
				}
				for len(sb.stack) > 0 {
					must(t, sb.deactivate(sb.stack[rng.IntN(len(sb.stack))], false))
				}
				assertSame(t, before, sb.snapshot())
				sb.assertNoLedgers()
			})
		}
	}
}

// Removing an unrelated lower layer first must not make the upper layer look
// for ".orig" and delete the host file.
func TestUnrelatedLowerLayerLeavesFirst(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("foot.ini", "ORIGINAL")
		sb.moduleFile("x", "other", "x")
		sb.moduleFile("b", "foot.ini", "b")
		before := sb.snapshot()

		must(t, sb.activate("x"))
		must(t, sb.activate("b"))
		must(t, sb.deactivate("x", false))
		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
	})
}

// A pre-existing foo.orig (e.g. from patch(1)) is never mistaken for arcus's
// backup.
func TestPreexistingOrigFileIsNotConfused(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("c.conf", "REAL")
		sb.hostFile("c.conf.orig", "PATCH LEFTOVER")
		sb.moduleFile("c", "c.conf", "arcus")
		before := sb.snapshot()

		must(t, sb.activate("c"))
		must(t, sb.deactivate("c", false))
		assertSame(t, before, sb.snapshot())
	})
}

// A file edited after deployment blocks teardown; -f moves it to
// <path>.arcus-foreign and never deletes it. For the system half this is also
// "a changed hash blocks teardown" (same-size edit).
func TestForeignModificationIsProtected(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("g.cfg", "ORIGINAL")
		sb.moduleFile("g", "g.cfg", "AAAA")
		must(t, sb.activate("g"))

		hostPath := filepath.Join(sb.host, "g.cfg")
		must(t, os.Remove(hostPath))
		must(t, os.WriteFile(hostPath, []byte("BBBB"), 0o644)) // same size, user's own content

		err := sb.deactivate("g", false)
		if !errors.Is(err, ErrForeign) {
			t.Fatalf("want ErrForeign, got %v", err)
		}
		if sb.read("g.cfg") != "BBBB" || sb.read("g.cfg.orig") != "ORIGINAL" {
			t.Fatal("refused teardown must not change anything")
		}

		must(t, sb.deactivate("g", true))
		if sb.read("g.cfg") != "ORIGINAL" {
			t.Errorf("original not restored: %q", sb.read("g.cfg"))
		}
		if sb.read("g.cfg.arcus-foreign") != "BBBB" {
			t.Errorf("user edit not preserved: %q", sb.read("g.cfg.arcus-foreign"))
		}
		if len(sb.warns) == 0 {
			t.Error("moving a file aside must warn")
		}
	})
}

func TestFailedActivationRollsBackExactly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks are bypassed as root")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("a.conf", "ORIGINAL")
		sb.moduleFile("p", "a.conf", "p")
		sb.moduleFile("p", "new/dir/file", "p")
		sb.moduleFile("p", "ro/blocked", "p")
		ro := filepath.Join(sb.host, "ro")
		must(t, os.MkdirAll(ro, 0o555))
		t.Cleanup(func() { os.Chmod(ro, 0o755) })
		before := sb.snapshot()

		err := sb.activate("p")
		if err == nil || !strings.Contains(err.Error(), "pre-flight") {
			t.Fatalf("want pre-flight failure, got %v", err)
		}
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
	})
}

// Ctrl-C or SIGTERM (a cancelled context) during activation rolls back
// exactly, whether it arrives before the first change or after some files
// were already deployed.
func TestCancelledActivationRollsBack(t *testing.T) {
	for _, h := range halves {
		for _, when := range []string{"before", "midway"} {
			t.Run(h.name+"/"+when, func(t *testing.T) {
				sb := newSandbox(t, h)
				sb.hostFile("a.conf", "ORIGINAL")
				sb.moduleFile("p", "a.conf", "p")
				sb.moduleFile("p", "b/c.conf", "p")
				before := sb.snapshot()

				ctx, cancel := context.WithCancel(context.Background())
				if when == "before" {
					cancel()
				} else {
					var n int
					sb.s.crashHook = func() {
						if n++; n == 3 { // after the first file is deployed
							cancel()
						}
					}
				}
				err := sb.s.Activate(ctx, "p", nil)
				cancel()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("want context.Canceled, got %v", err)
				}
				assertSame(t, before, sb.snapshot())
				sb.assertNoLedgers()
			})
		}
	}
}

type crash struct{}

// runUntilCrash runs op with a crash injected at the n-th crash point and
// reports whether it crashed.
func runUntilCrash(sb *sandbox, n int, op func() error) (crashed bool, err error) {
	count := 0
	sb.s.crashHook = func() {
		if count++; count == n {
			panic(crash{})
		}
	}
	defer func() {
		sb.s.crashHook = nil
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	return false, op()
}

// assertLedgerCovers checks the crash guarantee: every node that is new or
// changed compared with before is described by a ledger (a deployment, a
// backup or a created directory), and no file of the pristine host is lost.
// Nodes arcus removed need no record: they were its own deployments.
func (sb *sandbox) assertLedgerCovers(before, pristine map[string]string) {
	sb.t.Helper()
	known := map[string]bool{}
	names, err := sb.s.ListLedgers()
	must(sb.t, err)
	for _, name := range names {
		l, err := sb.s.LoadLedger(name)
		must(sb.t, err)
		for _, e := range l.Entries {
			known[e.Host], known[e.Backup] = true, true
		}
		for _, d := range l.CreatedDirs {
			known[d] = true
		}
	}

	now := sb.snapshot()
	present := map[string]bool{}
	for rel, v := range now {
		present[v] = true
		if v != before[rel] && !known[filepath.Join(sb.host, rel)] {
			sb.t.Errorf("%s changed (%q -> %q) but no ledger describes it", rel, before[rel], v)
		}
	}
	for rel, v := range pristine {
		if strings.HasPrefix(v, "file:") && !present[v] {
			sb.t.Errorf("host file %s (%q) was lost", rel, v)
		}
	}
}

// A crash at any point leaves a ledger describing a superset of the disk, and
// the doctor -fix repairs (rollback of an interrupted activation, re-running
// an interrupted deactivation) converge exactly.
func TestCrashAtAnyPointConverges(t *testing.T) {
	setup := func(sb *sandbox) {
		sb.hostFile("shared", "ORIGINAL")
		sb.hostFile("keep", "host only")
		sb.moduleFile("a", "shared", "a")
		sb.moduleFile("a", "a-dir/deep/x", "a")
		sb.moduleFile("b", "shared", "b")
		sb.moduleFile("b", "a-dir/deep/y", "b")
		sb.moduleFile("b", "b-only", "b")
	}

	for _, h := range halves {
		t.Run(h.name+"/activation", func(t *testing.T) {
			for n := 1; ; n++ {
				sb := newSandbox(t, h)
				setup(sb)
				original := sb.snapshot()
				must(t, sb.activate("a"))
				withA := sb.snapshot()

				crashed, err := runUntilCrash(sb, n, func() error { return sb.s.Activate(context.Background(), "b", sb.stack) })
				must(t, err)
				if !crashed {
					if n == 1 {
						t.Fatal("no crash points were hit")
					}
					t.Logf("crashed at each of %d points", n-1)
					break
				}
				sb.assertLedgerCovers(withA, original)

				must(t, sb.s.Rollback("b"))
				assertSame(t, withA, sb.snapshot())
				must(t, sb.deactivate("a", false))
				assertSame(t, original, sb.snapshot())
				sb.assertNoLedgers()
			}
		})

		t.Run(h.name+"/deactivation", func(t *testing.T) {
			// Reference: the target after a clean removal of a from under b.
			ref := newSandbox(t, h)
			setup(ref)
			must(t, ref.activate("a"))
			must(t, ref.activate("b"))
			must(t, ref.deactivate("a", false))
			onlyB := ref.snapshot()

			for n := 1; ; n++ {
				sb := newSandbox(t, h)
				setup(sb)
				original := sb.snapshot()
				must(t, sb.activate("a"))
				must(t, sb.activate("b"))
				both := sb.snapshot()

				crashed, err := runUntilCrash(sb, n, func() error { return sb.deactivate("a", false) })
				must(t, err)
				if !crashed {
					assertSame(t, onlyB, sb.snapshot())
					t.Logf("crashed at each of %d points", n-1)
					break
				}
				sb.assertLedgerCovers(both, original)

				must(t, sb.deactivate("a", false))
				assertSame(t, onlyB, sb.snapshot())
				must(t, sb.deactivate("b", false))
				assertSame(t, original, sb.snapshot())
				sb.assertNoLedgers()
			}
		})
	}
}

func TestDirectoryCollisionRejected(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("conf/inner", "x")
		sb.moduleFile("p", "conf", "file where host has a dir")
		before := sb.snapshot()
		if err := sb.activate("p"); err == nil {
			t.Fatal("expected collision error")
		}
		assertSame(t, before, sb.snapshot())
	})
}

// Simulates crashes in the middle of a teardown by replaying ledger states:
// re-running Deactivate from a partially applied state must converge.
func TestDeactivationIsResumable(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("shared", "ORIGINAL")
		sb.moduleFile("a", "shared", "a")
		sb.moduleFile("b", "shared", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))

		// Crash in the middle of splicing a out: a's file at shared.a was
		// removed but b's ledger was not yet updated and a's entry is still
		// recorded.
		b, _ := sb.s.LoadLedger("b")
		must(t, os.Remove(b.Entries[0].Backup))

		must(t, sb.deactivate("a", false))
		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
	})

	eachHalf(t, func(t *testing.T, sb *sandbox) {
		// Crash after restoring the original but before the ledger was trimmed.
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		must(t, sb.activate("p"))
		l, _ := sb.s.LoadLedger("p")
		must(t, os.Remove(l.Entries[0].Host))
		must(t, os.Rename(l.Entries[0].Backup, l.Entries[0].Host))
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
	})
}

func TestDeletedDeploymentStillRestoresBackup(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		must(t, sb.activate("p"))
		must(t, os.Remove(filepath.Join(sb.host, "f"))) // user deleted our deployment
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
	})
}

func TestSharedCreatedDirectoryIsHandedOver(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("a", "fresh/a", "a")
		sb.moduleFile("b", "fresh/b", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		must(t, sb.deactivate("a", false)) // fresh/ still holds b
		if _, err := os.Stat(filepath.Join(sb.host, "fresh")); err != nil {
			t.Fatal("dir removed while b still uses it")
		}
		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
	})
}

func TestUnregisteredModuleIsRejected(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		if err := sb.s.Activate(context.Background(), "ghost", nil); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("got %v", err)
		}
		sb.assertNoLedgers()
	})
}

func TestOwnDataIsRefused(t *testing.T) {
	sb := newSandbox(t, halves[0])
	// A home tree that would deploy into the state directory and into a module.
	sb.s.Target = sb.root
	sb.s.StateDir = filepath.Join(sb.root, "state")
	dir := sb.module("p")
	for _, rel := range []string{"state/ledger/x.json", "modules/p/home/loop"} {
		p := filepath.Join(dir, "home", rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, nil, 0o644))
	}
	err := sb.s.Activate(context.Background(), "p", nil)
	if err == nil || strings.Count(err.Error(), "own data") != 2 {
		t.Fatalf("want both paths refused, got %v", err)
	}
}

func TestRenameRelinksAndRetagsBackups(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("old", "f", "old")
		sb.moduleFile("top", "f", "top")
		// A module whose name starts with the old one must keep its backup.
		sb.moduleFile("oldx", "g", "oldx")
		sb.moduleFile("top", "g", "top")
		before := sb.snapshot()
		must(t, sb.activate("old"))
		must(t, sb.activate("oldx"))
		must(t, sb.activate("top"))
		if sb.read("f.old") != "old" || sb.read("g.oldx") != "oldx" {
			t.Fatalf("expected backups tagged with owner, got %q, %q", sb.read("f.old"), sb.read("g.oldx"))
		}

		oldDir, newDir := sb.s.Dirs["old"], filepath.Join(sb.profDir, "new")
		must(t, os.Rename(oldDir, newDir))
		delete(sb.s.Dirs, "old")
		sb.s.Dirs["new"] = newDir
		must(t, sb.s.Rename("old", "new", oldDir, newDir, sb.stack))
		sb.stack = []string{"new", "oldx", "top"}

		if sb.read("f.new") != "old" {
			t.Errorf("backup not retagged / relinked: %q", sb.read("f.new"))
		}
		if sb.read("g.oldx") != "oldx" {
			t.Errorf("unrelated backup g.oldx was touched: %q", sb.read("g.oldx"))
		}
		must(t, sb.deactivate("new", false))
		must(t, sb.deactivate("top", false))
		must(t, sb.deactivate("oldx", false))
		assertSame(t, before, sb.snapshot())
	})
}

func TestDanglingBackupsAndAudit(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "x")
		sb.hostFile("f.orig", "stray from a crash")
		sb.hostFile("f.unrelated", "not ours")
		sb.moduleFile("p", "f", "p")

		got, err := sb.s.DanglingBackups()
		must(t, err)
		if len(got) != 1 || !strings.HasSuffix(got[0], "f.orig") {
			t.Fatalf("dangling = %v", got)
		}

		must(t, sb.activate("p")) // backs up to f.orig.1
		got, _ = sb.s.DanglingBackups()
		if len(got) != 1 {
			t.Fatalf("referenced backup reported as dangling: %v", got)
		}

		findings, err := sb.s.Audit("p", sb.stack)
		must(t, err)
		if len(findings) != 0 {
			t.Fatalf("fresh layer has findings: %v", findings)
		}

		sb.moduleFile("p", "later", "added after activation")
		findings, err = sb.s.Audit("p", sb.stack)
		must(t, err)
		if len(findings) != 1 || !strings.Contains(findings[0], "added to") {
			t.Fatalf("findings = %v", findings)
		}
	})
}

func TestPythonBytecodeCachesAreNotDeployed(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("p", "bin/tool", "tool")
		sb.moduleFile("p", "bin/__pycache__/tool.cpython-314.pyc", "cache")
		must(t, sb.activate("p"))
		if _, err := os.Lstat(filepath.Join(sb.host, "bin/__pycache__")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cache directory deployed: %v", err)
		}
		if sb.read("bin/tool") != "tool" {
			t.Fatal("module file not deployed")
		}
		// Running the module's scripts writes new caches; they are not drift.
		sb.moduleFile("p", "lib/__pycache__/mod.cpython-314.pyc", "cache")
		findings, err := sb.s.Audit("p", sb.stack)
		must(t, err)
		if len(findings) != 0 {
			t.Fatalf("cache reported as drift: %v", findings)
		}
		changes, err := sb.s.Stale("p")
		must(t, err)
		if len(changes) != 0 {
			t.Fatalf("cache reported as stale: %v", changes)
		}
	})
}

// pycBytes is content laid out like a CPython bytecode cache: magic number,
// flags, source stamp, then a marshalled code object.
func pycBytes(s string) string {
	return "\x2b\x0e\r\n" + "\x00\x00\x00\x00" + "\x00\x00\x00\x00\x00\x00\x00\x00" + "\xe3" + s
}

// deployOldCache adds bin/__pycache__/<name> to module p's ledger and host
// the way a version before 0.8.2 deployed it, after p was turned on with
// bin/tool. With backup, the file already at the host path is moved aside
// first. It returns the host and source paths.
func (sb *sandbox) deployOldCache(name string, backup bool) (host, src string) {
	t := sb.t
	src = sb.moduleFile("p", "bin/__pycache__/"+name, pycBytes("module cache"))
	host = filepath.Join(sb.host, "bin/__pycache__", name)
	l, err := sb.s.LoadLedger("p")
	must(t, err)
	e := l.Entries[0]
	e.Host, e.Src, e.Backup = host, src, ""
	if e.Kind == KindCopy {
		e.SHA256, err = hashFile(src)
		must(t, err)
	}
	if dir := filepath.Dir(host); !exists(dir) {
		must(t, os.MkdirAll(dir, 0o755))
		l.CreatedDirs = append(l.CreatedDirs, dir)
	}
	if backup {
		e.Backup = host + ".orig"
		must(t, os.Rename(host, e.Backup))
	}
	must(t, deploy(e))
	l.Entries = append(l.Entries, e)
	must(t, sb.s.saveLedger(l))
	return host, src
}

// pythonRewrite replaces path the way Python writes a cache: a new file
// renamed over it, so a deployed link becomes a regular file.
func pythonRewrite(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	must(t, os.WriteFile(tmp, []byte(content), 0o644))
	must(t, os.Rename(tmp, path))
}

func TestCachesDeployedBeforeTheSkipAreStale(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("p", "bin/tool", "tool")
		must(t, sb.activate("p"))
		host, src := sb.deployOldCache("tool.cpython-314.pyc", false)
		changes, err := sb.s.Stale("p")
		must(t, err)
		if len(changes) != 1 || changes[0].Op != '-' || changes[0].Host != host || changes[0].Src != src {
			t.Fatalf("changes = %v", changes)
		}

		// What the restack apply plans for a stale layer runs.
		must(t, sb.deactivate("p", false))
		must(t, sb.activate("p"))
		if exists(filepath.Dir(host)) {
			t.Fatal("cache directory still deployed")
		}
		if sb.read("bin/tool") != "tool" {
			t.Fatal("module file not redeployed")
		}
		changes, err = sb.s.Stale("p")
		must(t, err)
		if len(changes) != 0 {
			t.Fatalf("stale after the restack: %v", changes)
		}
	})
}

func TestRewrittenCacheIsRemoved(t *testing.T) {
	for _, name := range []string{"tool.cpython-314.pyc", "tool.cpython-314.opt-1.pyc", "tool..helper.cpython-314.pyc"} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t, halves[0])
			pristine := sb.snapshot()
			sb.moduleFile("p", "bin/tool", "tool")
			must(t, sb.activate("p"))
			host, _ := sb.deployOldCache(name, false)
			pythonRewrite(t, host, pycBytes("rewritten"))

			findings, err := sb.s.Audit("p", sb.stack)
			must(t, err)
			if len(findings) != 0 {
				t.Fatalf("findings = %v", findings)
			}
			must(t, sb.deactivate("p", false))
			assertSame(t, pristine, sb.snapshot())
		})
	}
}

func TestRewrittenCacheRestoresBackup(t *testing.T) {
	const rel = "bin/__pycache__/tool.cpython-314.pyc"
	for n := 1; ; n++ {
		sb := newSandbox(t, halves[0])
		// A valid cache, so a teardown that misjudged the restored original
		// as Python's rewrite would delete it.
		sb.hostFile(rel, pycBytes("original"))
		sb.moduleFile("p", "bin/tool", "tool")
		must(t, sb.activate("p"))
		host, _ := sb.deployOldCache("tool.cpython-314.pyc", true)
		pythonRewrite(t, host, pycBytes("rewritten"))

		crashed, err := runUntilCrash(sb, n, func() error { return sb.s.Deactivate(context.Background(), "p", sb.stack, false) })
		if crashed {
			err = sb.s.Deactivate(context.Background(), "p", sb.stack, false)
		}
		must(t, err)
		if got := sb.read(rel); got != pycBytes("original") {
			t.Fatalf("crash %d: original = %q", n, got)
		}
		sb.assertNoLedgers()
		if !crashed {
			if n == 1 {
				t.Fatal("no crash points were hit")
			}
			break
		}
	}
}

func TestOnlyRewrittenCachesAreRemoved(t *testing.T) {
	const name = "tool.cpython-314.pyc"
	// Each case fails exactly one of the checks; Deactivate must refuse.
	cases := []struct {
		name  string
		half  half
		file  string
		after func(t *testing.T, sb *sandbox, host string)
	}{
		{"system copy", halves[1], name, func(t *testing.T, sb *sandbox, host string) {
			pythonRewrite(t, host, pycBytes("rewritten"))
		}},
		{"not bytecode", halves[0], name, func(t *testing.T, sb *sandbox, host string) {
			// Long enough for a header, with \r\n where the magic number ends.
			pythonRewrite(t, host, "OK\r\nplain text, not bytecode")
		}},
		{"not a cache name", halves[0], "tool.pyc", func(t *testing.T, sb *sandbox, host string) {
			pythonRewrite(t, host, pycBytes("rewritten"))
		}},
		{"empty tag", halves[0], "tool..pyc", func(t *testing.T, sb *sandbox, host string) {
			pythonRewrite(t, host, pycBytes("rewritten"))
		}},
		{"symlink elsewhere", halves[0], name, func(t *testing.T, sb *sandbox, host string) {
			other := filepath.Join(sb.root, "other.pyc")
			must(t, os.WriteFile(other, []byte(pycBytes("other")), 0o644))
			must(t, os.Remove(host))
			must(t, os.Symlink(other, host))
		}},
		{"symlinked parent", halves[0], name, func(t *testing.T, sb *sandbox, host string) {
			other := filepath.Join(sb.root, "other", "__pycache__")
			must(t, os.MkdirAll(other, 0o755))
			must(t, os.WriteFile(filepath.Join(other, name), []byte(pycBytes("other")), 0o644))
			must(t, os.RemoveAll(filepath.Dir(host)))
			must(t, os.Symlink(other, filepath.Dir(host)))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sb := newSandbox(t, c.half)
			sb.moduleFile("p", "bin/tool", "tool")
			must(t, sb.activate("p"))
			host, _ := sb.deployOldCache(c.file, false)
			c.after(t, sb, host)
			before := sb.snapshot()
			if err := sb.deactivate("p", false); !errors.Is(err, ErrForeign) {
				t.Fatalf("deactivate = %v, want ErrForeign", err)
			}
			assertSame(t, before, sb.snapshot())
			if c.name == "symlinked parent" {
				data, err := os.ReadFile(filepath.Join(sb.root, "other", "__pycache__", name))
				if err != nil || string(data) != pycBytes("other") {
					t.Fatalf("file behind the symlink: %q, %v", data, err)
				}
			}
		})
	}

	t.Run("rollback removes a rewritten cache", func(t *testing.T) {
		sb := newSandbox(t, halves[0])
		pristine := sb.snapshot()
		sb.moduleFile("p", "bin/tool", "tool")
		must(t, sb.activate("p"))
		host, _ := sb.deployOldCache(name, false)
		pythonRewrite(t, host, pycBytes("rewritten"))
		must(t, sb.s.Rollback("p"))
		assertSame(t, pristine, sb.snapshot())
		sb.assertNoLedgers()
	})
}

func TestCacheNamesOutsideACacheDirectory(t *testing.T) {
	cases := []struct {
		name  string
		setup func(sb *sandbox)
	}{
		{"module directory under __pycache__", func(sb *sandbox) {
			sb.profDir = filepath.Join(sb.root, "__pycache__", "modules")
		}},
		{"target under __pycache__", func(sb *sandbox) {
			s := *sb.s
			s.Target = filepath.Join(sb.root, "__pycache__", "target")
			sb.s, sb.host = &s, filepath.Join(s.Target, "etc")
			must(sb.t, os.MkdirAll(sb.host, 0o755))
		}},
		{"file named __pycache__", func(sb *sandbox) {
			sb.moduleFile("p", "lib/__pycache__", "not a directory")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eachHalf(t, func(t *testing.T, sb *sandbox) {
				c.setup(sb)
				sb.moduleFile("p", "bin/tool", "tool")
				must(t, sb.activate("p"))
				if sb.read("bin/tool") != "tool" {
					t.Fatal("module file not deployed")
				}
				if c.name == "file named __pycache__" && sb.read("lib/__pycache__") != "not a directory" {
					t.Fatal("file named __pycache__ not deployed")
				}
				changes, err := sb.s.Stale("p")
				must(t, err)
				if len(changes) != 0 {
					t.Fatalf("stale right after activation: %v", changes)
				}
			})
		})
	}
}

func TestVerifyWritePrivilegeAcceptsMissingParents(t *testing.T) {
	dir := t.TempDir()
	if err := VerifyWritePrivilege(filepath.Join(dir, "a", "b", "c", "file")); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "plain"), nil, 0o644))
	if err := VerifyWritePrivilege(filepath.Join(dir, "plain", "child")); err == nil {
		t.Fatal("expected error when an ancestor is a regular file")
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	must(t, err)
	return info.Sys().(*syscall.Stat_t).Ino
}

// A restack turns layers off and on again. Directories that programs watch
// (a shell's plugin folder, an app's preset folder) must survive it rather
// than be replaced by new ones the watchers never see.
func TestRestackKeepsCreatedDirs(t *testing.T) {
	for _, h := range halves {
		t.Run(h.name, func(t *testing.T) {
			sb := newSandbox(t, h)
			sb.moduleFile("a", "plugins/eq/plugin.json", "{}")
			sb.moduleFile("b", "other/b.conf", "b")
			must(t, sb.activate("a"))
			must(t, sb.activate("b"))
			plugins, eq := filepath.Join(sb.host, "plugins"), filepath.Join(sb.host, "plugins/eq")
			before := []uint64{inode(t, plugins), inode(t, eq)}

			// b dropped its file; its directory has no reason to stay.
			must(t, os.Remove(filepath.Join(sb.profDir, "b", sb.s.Subdir, "etc/other/b.conf")))
			must(t, sb.s.Hold())
			must(t, sb.deactivate("b", false))
			must(t, sb.deactivate("a", false))
			must(t, sb.activate("a"))
			must(t, sb.activate("b"))
			must(t, sb.s.Release())

			if got := []uint64{inode(t, plugins), inode(t, eq)}; !slices.Equal(got, before) {
				t.Errorf("plugin directories were replaced: inodes %v, then %v", before, got)
			}
			if exists(filepath.Join(sb.host, "other")) {
				t.Error("a directory no layer needs any more was kept")
			}
			if l, err := sb.s.LoadLedger(HoldLedger); err != nil || l != nil {
				t.Errorf("hold ledger left behind: %v, %v", l, err)
			}

			// The directories are a's again: turning it off removes them.
			must(t, sb.deactivate("b", false))
			must(t, sb.deactivate("a", false))
			if exists(plugins) {
				t.Error("a's directories outlived its layer")
			}
		})
	}
}

// A restack interrupted while its layers are off leaves the hold behind;
// releasing it removes the directories nothing uses, as the deactivation
// would have.
func TestReleaseAfterInterruptedRestack(t *testing.T) {
	for _, h := range halves {
		t.Run(h.name, func(t *testing.T) {
			sb := newSandbox(t, h)
			sb.moduleFile("a", "plugins/eq/plugin.json", "{}")
			must(t, sb.activate("a"))
			must(t, sb.s.Hold())
			must(t, sb.deactivate("a", false))
			if !exists(filepath.Join(sb.host, "plugins/eq")) {
				t.Fatal("held directory removed")
			}
			must(t, sb.s.Release())
			if exists(filepath.Join(sb.host, "plugins")) {
				t.Error("unused held directory kept after release")
			}
		})
	}
}

// doctor repairs the layers the way `arcus doctor -fix` does, in its order
// (the hold first), in a new process (no hold in effect).
func (sb *sandbox) doctor() {
	sb.t.Helper()
	sb.s.holding = false
	names, err := sb.s.ListLedgers()
	must(sb.t, err)
	if slices.Contains(names, HoldLedger) {
		must(sb.t, sb.s.Release())
	}
	for _, name := range names {
		if name == HoldLedger {
			continue
		}
		l, err := sb.s.LoadLedger(name)
		must(sb.t, err)
		switch {
		case l == nil:
		case !slices.Contains(sb.stack, name):
			must(sb.t, sb.s.Rollback(name))
		case l.Status == StatusDeactivating:
			must(sb.t, sb.deactivate(name, false))
		case l.Status != StatusActive:
			must(sb.t, sb.s.Rollback(name))
			sb.stack = slices.DeleteFunc(sb.stack, func(s string) bool { return s == name })
		}
	}
}

// A crash anywhere in a restack leaves every change described by a ledger,
// and doctor's repairs leave nothing behind once the layers are off. Module
// names sort on both sides of the hold ledger, the upper layer sorts first in
// one case, and in the nested layout the upper layer creates a directory
// inside one the lower layer created.
func TestRestackCrashAtAnyPointConverges(t *testing.T) {
	for _, h := range halves {
		for _, nested := range []bool{false, true} {
			for _, names := range [][2]string{{"a", "b"}, {"0a", "0b"}, {"z", "b"}} {
				t.Run(fmt.Sprintf("%s/%s/nested=%v", h.name, names[0], nested), func(t *testing.T) {
					restackCrashes(t, h, names[0], names[1], nested)
				})
			}
		}
	}
}

func restackCrashes(t *testing.T, h half, a, b string, nested bool) {
	for n := 1; ; n++ {
		sb := newSandbox(t, h)
		sb.hostFile("keep", "host only")
		sb.moduleFile(a, "plugins/eq/x", "a")
		sb.moduleFile(b, "plugins/eq/y", "b")
		sb.moduleFile(b, "other/deep/b", "b")
		if nested {
			sb.moduleFile(b, "plugins/eq/deep/y", "b")
		}
		original := sb.snapshot()
		must(t, sb.activate(a))
		must(t, sb.activate(b))
		sb.moduleFile(a, "plugins/eq/z", "new in a")
		before := sb.snapshot()

		crashed, err := runUntilCrash(sb, n, func() error {
			must(t, sb.s.Hold())
			must(t, sb.deactivate(b, false))
			must(t, sb.deactivate(a, false))
			must(t, sb.activate(a))
			must(t, sb.activate(b))
			return sb.s.Release()
		})
		must(t, err)
		if !crashed {
			if got := sb.read("plugins/eq/z"); got != "new in a" {
				t.Errorf("after the restack, z = %q", got)
			}
			t.Logf("crashed at each of %d points", n-1)
			return
		}
		sb.assertLedgerCovers(before, original)

		sb.doctor()
		for i := len(sb.stack) - 1; i >= 0; i-- {
			must(t, sb.deactivate(sb.stack[i], false))
		}
		assertSame(t, original, sb.snapshot())
		sb.assertNoLedgers()
	}
}

// A module that turned a directory it created into a file restacks: the
// held directory, empty, makes way for the file.
func TestRestackFileReplacesHeldDir(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		bar := sb.moduleFile("a", "foo/bar", "bar")
		must(t, sb.activate("a"))
		must(t, os.RemoveAll(filepath.Dir(bar)))
		sb.moduleFile("a", "foo", "now a file")

		must(t, sb.s.Hold())
		must(t, sb.deactivate("a", false))
		must(t, sb.activate("a"))
		must(t, sb.s.Release())
		if got := sb.read("foo"); got != "now a file" {
			t.Errorf("foo = %q", got)
		}
		must(t, sb.deactivate("a", false))
		if exists(filepath.Join(sb.host, "foo")) {
			t.Error("foo outlived its layer")
		}
		sb.assertNoLedgers()
	})
}

// A hold an interrupted restack left applies to no turn-off but a restack's.
func TestStaleHoldOnlyAppliesToRestacks(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("a", "a-dir/x", "a")
		sb.moduleFile("b", "b-dir/y", "b")
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("b", false))
		sb.s.holding = false // the process died here

		must(t, sb.deactivate("a", false))
		if exists(filepath.Join(sb.host, "a-dir")) {
			t.Error("a plain turn-off kept its directory for a stale hold")
		}
		must(t, sb.s.Release())
		if exists(filepath.Join(sb.host, "b-dir")) {
			t.Error("releasing the stale hold kept b's empty directory")
		}
		sb.assertNoLedgers()
	})
}

// A hold an interrupted restack left must be released by doctor before the
// next restack can hold.
func TestHoldRefusesALeftoverHold(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		must(t, sb.s.Hold())
		sb.s.holding = false // the process died here
		if err := sb.s.Hold(); err == nil || !strings.Contains(err.Error(), "doctor -fix") {
			t.Fatalf("Hold over a leftover hold = %v", err)
		}
		must(t, sb.s.Release())
		must(t, sb.s.Hold())
		must(t, sb.s.Release())
	})
}

// A crash in the rollback of an interrupted activation, after its last file
// entry went and before it removed the directory it created inside a held
// one, leaves the held directory to that ledger rather than stranding it.
func TestReleaseHandsHeldDirsToNestedCreatedDirs(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		original := sb.snapshot()
		sb.moduleFile("a", "app/x", "a")
		must(t, sb.activate("a"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("a", false))
		app := filepath.Join(sb.host, "app")
		deep := filepath.Join(app, "deep")
		must(t, os.Mkdir(deep, 0o755))
		must(t, sb.s.saveLedger(&Ledger{Module: "b", Status: StatusActivating, CreatedDirs: []string{deep}}))

		sb.doctor()
		assertSame(t, original, sb.snapshot())
		sb.assertNoLedgers()
	})
}

// Removing a directory arcus recorded never removes a file or a symlink that
// took its place, nor anything below a parent replaced by a symlink.
func TestRemovingDirsLeavesOtherThingsAlone(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		replace := func(dir, content string) {
			must(t, os.RemoveAll(dir))
			must(t, os.WriteFile(dir, []byte(content), 0o644))
		}

		// A held directory replaced by a file.
		sb.moduleFile("a", "held/x", "a")
		must(t, sb.activate("a"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("a", false))
		replace(filepath.Join(sb.host, "held"), "user data")
		must(t, sb.s.Release())
		if got := sb.read("held"); got != "user data" {
			t.Errorf("held = %q after Release", got)
		}

		// A created directory replaced by a file, without a hold.
		sb.moduleFile("b", "made/y", "b")
		must(t, sb.activate("b"))
		replace(filepath.Join(sb.host, "made"), "user data")
		must(t, sb.deactivate("b", false))
		if got := sb.read("made"); got != "user data" {
			t.Errorf("made = %q after turn-off", got)
		}

		// A held directory replaced by a file where the module now ships one:
		// the file is backed up, not deleted, and comes back on turn-off.
		bar := sb.moduleFile("c", "foo/bar", "bar")
		must(t, sb.activate("c"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("c", false))
		replace(filepath.Join(sb.host, "foo"), "user data")
		must(t, os.RemoveAll(filepath.Dir(bar)))
		sb.moduleFile("c", "foo", "from c")
		must(t, sb.activate("c"))
		must(t, sb.s.Release())
		if got, orig := sb.read("foo"), sb.read("foo.orig"); got != "from c" || orig != "user data" {
			t.Errorf("foo = %q, foo.orig = %q", got, orig)
		}
		must(t, sb.deactivate("c", false))
		if got := sb.read("foo"); got != "user data" {
			t.Errorf("foo = %q after turn-off", got)
		}

		// A held directory whose parent became a symlink to another tree.
		sb.moduleFile("d", "p/q/x", "d")
		must(t, sb.activate("d"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("d", false))
		other := filepath.Join(sb.root, "other")
		must(t, os.MkdirAll(filepath.Join(other, "q"), 0o755))
		must(t, os.RemoveAll(filepath.Join(sb.host, "p")))
		must(t, os.Symlink(other, filepath.Join(sb.host, "p")))
		must(t, sb.s.Release())
		if !exists(filepath.Join(other, "q")) {
			t.Error("Release removed a directory below a symlinked parent")
		}
		sb.assertNoLedgers()
	})
}

// A hand-over that fails to save keeps the hold, so doctor can retry it.
func TestReleaseKeepsTheHoldWhenAHandOverFails(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("b", "d/y", "b")
		sb.moduleFile("a", "d/x", "a")
		must(t, sb.activate("b"))
		must(t, sb.activate("a"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("b", false))

		injected := errors.New("disk full")
		sb.s.saveHook = func(module string) error {
			if module == "a" {
				return injected
			}
			return nil
		}
		if err := sb.s.Release(); !errors.Is(err, injected) {
			t.Fatalf("Release = %v, want the failed save", err)
		}
		if !exists(sb.s.HoldPath()) {
			t.Fatal("the hold was dropped after a failed hand-over")
		}
		sb.s.saveHook = nil
		must(t, sb.s.Release())
		must(t, sb.deactivate("a", false))
		if exists(filepath.Join(sb.host, "d")) {
			t.Error("d outlived the layers that used it")
		}
		sb.assertNoLedgers()
	})
}

// Under a parent that can't be written, an empty held directory keeps the
// hold; one with a layer's files goes to that layer; one with only other
// files is left, as a plain turn-off leaves it. A retry counts a directory
// already handed over as taken.
func TestReleaseUnderAReadOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		readOnly := func(dir string) {
			must(t, os.Chmod(dir, 0o555))
			t.Cleanup(func() { os.Chmod(dir, 0o755) })
		}

		// p/used holds a's files; q/empty is empty: both under read-only parents.
		sb.moduleFile("a", "p/used/x", "a")
		sb.moduleFile("c", "p/used/z", "c")
		sb.moduleFile("c", "q/empty/w", "c")
		must(t, sb.activate("c"))
		must(t, sb.activate("a"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("c", false))
		readOnly(filepath.Join(sb.host, "p"))
		readOnly(filepath.Join(sb.host, "q"))

		if err := sb.s.Release(); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("Release = %v, want EACCES for the empty directory", err)
		}
		if !exists(sb.s.HoldPath()) {
			t.Fatal("the hold was dropped while an empty directory stayed")
		}
		l, err := sb.s.LoadLedger("a")
		must(t, err)
		if !slices.Contains(l.CreatedDirs, filepath.Join(sb.host, "p/used")) {
			t.Errorf("a's created dirs = %v, want p/used", l.CreatedDirs)
		}

		must(t, os.Chmod(filepath.Join(sb.host, "q"), 0o755))
		sb.warns = nil
		must(t, sb.s.Release())
		if len(sb.warns) != 0 {
			t.Errorf("retry warned: %v", sb.warns)
		}
		if exists(filepath.Join(sb.host, "q/empty")) {
			t.Error("q/empty survived the retry")
		}

		// Only another program's file in a held directory: left, with a warning.
		sb.moduleFile("e", "r/state/x", "e")
		must(t, sb.activate("e"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("e", false))
		sb.hostFile("r/state/app.json", "{}")
		readOnly(filepath.Join(sb.host, "r"))
		sb.warns = nil
		must(t, sb.s.Release())
		if sb.read("r/state/app.json") != "{}" || len(sb.warns) != 1 {
			t.Errorf("app.json = %q, warnings %v", sb.read("r/state/app.json"), sb.warns)
		}
		if exists(sb.s.HoldPath()) {
			t.Error("the hold stayed for a directory with only other files")
		}
	})
}

// A restack of a module with an unreadable bytecode cache doesn't fail: the
// walk that frees held directories skips caches like every other walk.
func TestRestackSkipsAnUnreadableCache(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.moduleFile("a", "d/x", "a")
		cache := filepath.Dir(sb.moduleFile("a", "d/__pycache__/x.cpython-314.pyc", "cache"))
		must(t, os.Chmod(cache, 0))
		t.Cleanup(func() { os.Chmod(cache, 0o755) })
		must(t, sb.activate("a"))
		must(t, sb.s.Hold())
		must(t, sb.deactivate("a", false))
		must(t, sb.activate("a"))
		must(t, sb.s.Release())
	})
}
