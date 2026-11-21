package layer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stored reads a file in the sandbox's state directory, or "<missing>".
func (sb *sandbox) stored(rel string) string {
	data, err := os.ReadFile(filepath.Join(sb.s.StateDir, rel))
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

// legacy turns the store backups of module's ledger into the sidecar files
// older versions left next to the host, so the ledger looks like theirs.
func (sb *sandbox) legacy(module, tag string) {
	sb.t.Helper()
	l, err := sb.s.LoadLedger(module)
	must(sb.t, err)
	for i := range l.Entries {
		e := &l.Entries[i]
		if e.Backup == "" {
			continue
		}
		sidecar := uniquePath(e.Host + "." + tag)
		must(sb.t, os.Rename(e.Backup, sidecar))
		e.Backup = sidecar
	}
	must(sb.t, sb.s.saveLedger(l))
}

func TestBackupsLiveInTheStateDir(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("deep/dir/f", "ORIGINAL")
		sb.moduleFile("a", "deep/dir/f", "a")
		before := sb.snapshot()

		must(t, sb.activate("a"))
		want := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "deep", "dir", "f")
		l, err := sb.s.LoadLedger("a")
		must(t, err)
		if l.Entries[0].Backup != want || sb.stored("backups/orig/etc/deep/dir/f") != "ORIGINAL" {
			t.Fatalf("backup = %q, want %q", l.Entries[0].Backup, want)
		}
		if matches, _ := filepath.Glob(filepath.Join(sb.host, "deep/dir/*")); len(matches) != 1 {
			t.Errorf("sidecar files next to the host: %v", matches)
		}

		must(t, sb.deactivate("a", false))
		assertSame(t, before, sb.snapshot())
		// The emptied directories are gone; the tag directory stays.
		if exists(filepath.Join(sb.s.StateDir, "backups", "orig", "etc")) || !exists(filepath.Join(sb.s.StateDir, "backups", "orig")) {
			t.Error("store directories were not pruned up to the tag directory")
		}
	})
}

// With B over A, B's backup holds A's deployment in backups/a. Taking A off
// splices B onto A's own backup.
func TestSpliceWithBackupsInTheStore(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("a", "f", "a")
		sb.moduleFile("b", "f", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		if sb.stored("backups/orig/etc/f") != "ORIGINAL" || !exists(filepath.Join(sb.s.StateDir, "backups", "a", "etc", "f")) {
			t.Fatalf("store: orig=%q", sb.stored("backups/orig/etc/f"))
		}

		must(t, sb.deactivate("a", false))
		if sb.s.Copy && sb.read("f") != "b" {
			t.Fatalf("top layer disturbed: %q", sb.read("f"))
		}
		if exists(filepath.Join(sb.s.StateDir, "backups", "a", "etc")) {
			t.Error("A's backup directory was not pruned")
		}
		lb, _ := sb.s.LoadLedger("b")
		if lb.Entries[0].Backup != filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "f") {
			t.Errorf("B was not spliced onto the original: %q", lb.Entries[0].Backup)
		}

		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
		if dangling, err := sb.s.DanglingBackups(); err != nil || len(dangling) != 0 {
			t.Errorf("dangling = %v, %v", dangling, err)
		}
	})
}

func TestForeignFilesAreMovedIntoTheStateDir(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("d/g.cfg", "ORIGINAL")
		sb.moduleFile("g", "d/g.cfg", "AAAA")
		for i, edit := range []string{"BBBB", "CCCC"} {
			must(t, sb.activate("g"))
			p := filepath.Join(sb.host, "d/g.cfg")
			must(t, os.Remove(p))
			must(t, os.WriteFile(p, []byte(edit), 0o644))
			sb.warns = nil
			must(t, sb.deactivate("g", true))

			want := filepath.Join(sb.s.StateDir, "foreign", "etc", "d", "g.cfg")
			if i == 1 {
				want += ".1"
			}
			if got, _ := os.ReadFile(want); string(got) != edit {
				t.Errorf("%s = %q, want %q", want, got, edit)
			}
			if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], want) {
				t.Errorf("warning must name %s: %v", want, sb.warns)
			}
			if matches, _ := filepath.Glob(filepath.Join(sb.host, "d", "*")); len(matches) != 1 {
				t.Errorf("sidecar files next to the host: %v", matches)
			}
		}
	})
}

// A ledger written before backups moved names a sidecar file next to the
// host; it is spliced and restored from there.
func TestLegacySidecarBackupStillRestores(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("a", "f", "a")
		sb.moduleFile("b", "f", "b")
		before := sb.snapshot()
		must(t, sb.activate("a"))
		must(t, sb.activate("b"))
		sb.legacy("a", "orig")
		sb.legacy("b", "a")
		if sb.read("f.orig") != "ORIGINAL" {
			t.Fatalf("setup: %q", sb.read("f.orig"))
		}

		must(t, sb.deactivate("a", false))
		must(t, sb.deactivate("b", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
	})
}

func TestRenameMovesStoreAndLegacyBackups(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		eachHalf(t, func(t *testing.T, sb *sandbox) {
			sb.hostFile("f", "ORIGINAL")
			sb.moduleFile("old", "f", "old")
			sb.moduleFile("top", "f", "top")
			must(t, sb.activate("old"))
			must(t, sb.activate("top"))
			if legacy {
				sb.legacy("top", "old")
			}

			oldDir, newDir := sb.s.Dirs["old"], filepath.Join(sb.profDir, "new")
			must(t, os.Rename(oldDir, newDir))
			delete(sb.s.Dirs, "old")
			sb.s.Dirs["new"] = newDir
			must(t, sb.s.Rename("old", "new", oldDir, newDir, sb.stack))
			sb.stack = []string{"new", "top"}

			l, _ := sb.s.LoadLedger("top")
			want := filepath.Join(sb.s.StateDir, "backups", "new", "etc", "f")
			if legacy {
				want = filepath.Join(sb.host, "f.new")
			}
			if l.Entries[0].Backup != want || !exists(want) {
				t.Errorf("legacy=%v: backup = %q, want %q", legacy, l.Entries[0].Backup, want)
			}
			if !legacy && exists(filepath.Join(sb.s.StateDir, "backups", "old")) {
				t.Error("the old backup directory was not pruned")
			}
			must(t, sb.deactivate("new", false))
			must(t, sb.deactivate("top", false))
			if sb.read("f") != "ORIGINAL" {
				t.Errorf("legacy=%v: f = %q", legacy, sb.read("f"))
			}
		})
	}
}

func TestDanglingBackupsInTheStore(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("f", "x")
		sb.moduleFile("p", "f", "p")
		must(t, sb.activate("p"))
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "stray")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o755))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o644))

		got, err := sb.s.DanglingBackups()
		must(t, err)
		if len(got) != 1 || got[0] != stray {
			t.Fatalf("dangling = %v, want only %s", got, stray)
		}
	})
}

// The cross-filesystem path of moveDurable, called directly: a test can't
// force EXDEV.
func TestCopyDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	must(t, os.WriteFile(src, []byte("content"), 0o640))
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, os.Chtimes(src, mtime, mtime))
	must(t, os.Symlink("target", filepath.Join(dir, "link")))
	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))

	out := filepath.Join(dir, "out")
	must(t, os.Mkdir(out, 0o755))
	must(t, copyDurable(src, filepath.Join(out, "dst")))
	info, err := os.Lstat(filepath.Join(out, "dst"))
	must(t, err)
	if data, _ := os.ReadFile(filepath.Join(out, "dst")); string(data) != "content" || info.Mode().Perm() != 0o640 || !info.ModTime().Equal(mtime) {
		t.Errorf("copy = %q %v %v", data, info.Mode(), info.ModTime())
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Errorf("owner = %d, want %d", st.Uid, os.Getuid())
	}

	must(t, copyDurable(filepath.Join(dir, "link"), filepath.Join(out, "link")))
	if got, err := os.Readlink(filepath.Join(out, "link")); err != nil || got != "target" {
		t.Errorf("link = %q, %v", got, err)
	}

	if err := copyDurable(filepath.Join(dir, "fifo"), filepath.Join(out, "fifo")); err == nil {
		t.Error("a fifo must be refused")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 2 {
		t.Errorf("files in out/ = %v, want only dst and link", entries)
	}
}

func TestMoveDurable(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644))
	must(t, moveDurable(filepath.Join(dir, "a"), filepath.Join(dir, "b"), nil))
	if exists(filepath.Join(dir, "a")) || !exists(filepath.Join(dir, "b")) {
		t.Error("file was not moved")
	}
	if err := moveDurable(filepath.Join(dir, "missing"), filepath.Join(dir, "c"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("moving a missing file: %v", err)
	}
}

// A crash between the copy into place and the removal of the source leaves
// both; turning the module off converges.
func TestInterruptedCopyMoveConverges(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		must(t, sb.activate("p"))

		// Teardown crashed after the backup was copied back to the host.
		func() {
			afterCopy = func() { panic(crash{}) }
			defer func() { afterCopy = nil; _ = recover() }()
			_ = sb.deactivate("p", false)
		}()
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		if !l.Entries[0].Copying || !exists(l.Entries[0].Backup) || sb.read("f") != "ORIGINAL" {
			t.Fatalf("setup: entry %+v, f = %q", l.Entries[0], sb.read("f"))
		}
		findings, err := sb.s.Audit("p", sb.stack)
		must(t, err)
		if len(findings) == 0 || !strings.Contains(findings[0], "duplicate") {
			t.Errorf("audit of the duplicate: %v", findings)
		}

		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if dangling, _ := sb.s.DanglingBackups(); len(dangling) != 0 {
			t.Errorf("dangling after convergence: %v", dangling)
		}
	})
}

// A host file identical to the backup is the user's edit unless the ledger
// records a copy in progress, even across filesystems: with matching bytes
// and mode it may still differ in what a copy can't compare, such as an ACL.
// The backup must not be dropped.
func TestIdenticalEditIsNotADuplicate(t *testing.T) {
	for _, exdev := range []bool{false, true} {
		eachHalf(t, func(t *testing.T, sb *sandbox) {
			copyMoves(t, exdev)
			sb.hostFile("f", "ORIGINAL")
			sb.moduleFile("p", "f", "p")
			must(t, sb.activate("p"))
			p := filepath.Join(sb.host, "f")
			must(t, os.Remove(p))
			must(t, os.WriteFile(p, []byte("ORIGINAL"), 0o644))

			if err := sb.deactivate("p", false); !errors.Is(err, ErrForeign) {
				t.Fatalf("exdev=%v: deactivate = %v, want ErrForeign", exdev, err)
			}
			if sb.stored("backups/orig/etc/f") != "ORIGINAL" {
				t.Errorf("exdev=%v: the backup was dropped", exdev)
			}
		})
	}
}

// A directory where a file was deployed can't be copied to another
// filesystem; -f leaves it next to the host and says so.
func TestForeignDirectoryStaysInPlaceAcrossFilesystems(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("g", "ORIGINAL")
		sb.moduleFile("p", "g", "p")
		must(t, sb.activate("p"))
		p := filepath.Join(sb.host, "g")
		must(t, os.Remove(p))
		must(t, os.MkdirAll(filepath.Join(p, "sub"), 0o755))
		must(t, os.WriteFile(filepath.Join(p, "sub", "mine"), []byte("mine"), 0o644))

		must(t, sb.deactivate("p", true))
		if sb.read("g") != "ORIGINAL" || sb.read("g.arcus-foreign/sub/mine") != "mine" {
			t.Errorf("g = %q, aside = %q", sb.read("g"), sb.read("g.arcus-foreign/sub/mine"))
		}
		if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], "stays here") {
			t.Errorf("warning = %v", sb.warns)
		}
	})
}

// setXattr gives path an extended attribute, or skips the test where the
// filesystem has none.
func setXattr(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Setxattr(path, "user.arcus-test", []byte("1"), 0); err != nil {
		t.Skipf("no extended attributes here: %v", err)
	}
}

// A copy would drop an ACL or label, which could widen access to the file
// once it is restored; such a file stays next to the host under -f.
func TestFileWithXattrsIsNotCopiedAcrossFilesystems(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	must(t, os.WriteFile(src, []byte("x"), 0o600))
	setXattr(t, src)
	if err := copyDurable(src, filepath.Join(dir, "dst")); !errors.Is(err, errCannotCopy) {
		t.Errorf("copyDurable = %v, want errCannotCopy", err)
	}
	if exists(filepath.Join(dir, "dst")) {
		t.Error("the copy was made")
	}

	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.moduleFile("p", "g", "p")
		must(t, sb.activate("p"))
		p := filepath.Join(sb.host, "g")
		must(t, os.Remove(p))
		must(t, os.WriteFile(p, []byte("edited"), 0o600))
		setXattr(t, p)

		must(t, sb.deactivate("p", true))
		if got := sb.read("g.arcus-foreign"); got != "edited" {
			t.Errorf("aside = %q", got)
		}
		if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], "stays here") {
			t.Errorf("warning = %v", sb.warns)
		}
	})
}

func TestStoreDirectoriesArePrivate(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("d/f", "ORIGINAL")
		sb.moduleFile("p", "d/f", "p")
		must(t, sb.activate("p"))
		for _, rel := range []string{"backups", "backups/orig", "backups/orig/etc/d"} {
			info, err := os.Stat(filepath.Join(sb.s.StateDir, rel))
			must(t, err)
			if info.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want 0700", rel, info.Mode().Perm())
			}
		}
	})
}

// A file -f moved into foreign/ sits where a later edit's directory would
// go. The edit stays next to the host instead of failing every later -f.
func TestForeignFileWhereTheStoreNeedsADirectory(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		edit := func(rel, content string) {
			p := filepath.Join(sb.host, rel)
			must(t, os.Remove(p))
			must(t, os.WriteFile(p, []byte(content), 0o644))
		}
		sb.moduleFile("f", "d/x", "f")
		must(t, sb.activate("f"))
		edit("d/x", "edited x")
		must(t, sb.deactivate("f", true))
		if sb.stored("foreign/etc/d/x") != "edited x" {
			t.Fatalf("setup: foreign/etc/d/x = %q", sb.stored("foreign/etc/d/x"))
		}

		sb.moduleFile("g", "d/x/y", "g")
		must(t, sb.activate("g"))
		edit("d/x/y", "edited y")
		sb.warns = nil
		must(t, sb.deactivate("g", true))
		if got := sb.read("d/x/y.arcus-foreign"); got != "edited y" {
			t.Errorf("aside = %q", got)
		}
		if len(sb.warns) != 1 || !strings.Contains(sb.warns[0], "stays here") {
			t.Errorf("warning = %v", sb.warns)
		}
		sb.assertNoLedgers()
	})
}

// A symlink in the store is never followed: the edit would land wherever it
// points.
func TestSymlinkInTheStoreIsNotFollowed(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		elsewhere := filepath.Join(sb.root, "elsewhere")
		must(t, os.Mkdir(elsewhere, 0o755))
		must(t, os.MkdirAll(filepath.Join(sb.s.StateDir, "foreign", "etc"), 0o700))
		must(t, os.Symlink(elsewhere, filepath.Join(sb.s.StateDir, "foreign", "etc", "d")))

		sb.moduleFile("g", "d/y", "g")
		must(t, sb.activate("g"))
		p := filepath.Join(sb.host, "d/y")
		must(t, os.Remove(p))
		must(t, os.WriteFile(p, []byte("edited"), 0o644))
		must(t, sb.deactivate("g", true))
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("moved through the symlink: %v", entries)
		}
		if got := sb.read("d/y.arcus-foreign"); got != "edited" {
			t.Errorf("aside = %q", got)
		}
	})
}

// A stray file in backups/ (left by a crash) where a backup's directory
// would go: the backup is kept next to the host, and restores from there.
func TestBackupBesideTheHostWhenTheStoreIsBlocked(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "d")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o700))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o600))
		sb.hostFile("d/x", "ORIGINAL")
		sb.moduleFile("p", "d/x", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		if got := sb.read("d/x.orig"); got != "ORIGINAL" {
			t.Errorf("backup = %q", got)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		if got := sb.stored("backups/orig/etc/d"); got != "left by a crash" {
			t.Errorf("stray = %q", got)
		}
	})
}

// A backup already in the store for a makes the next backup of a "a.1",
// the path a host file named a.1 mirrors to. A plan backing up both must
// give them different paths, or the second move overwrites the first
// original.
func TestBackupPathsInOnePlanDoNotCollide(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "a")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o700))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o600))
		sb.hostFile("a", "A ORIGINAL")
		sb.hostFile("a.1", "A.1 ORIGINAL")
		sb.moduleFile("p", "a", "p")
		sb.moduleFile("p", "a.1", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		if l.Entries[0].Backup == l.Entries[1].Backup {
			t.Fatalf("both backups go to %s", l.Entries[0].Backup)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		if got := sb.stored("backups/orig/etc/a"); got != "left by a crash" {
			t.Errorf("stray = %q", got)
		}
	})
}

// With a backup of a already in the store, a's next backup takes the name
// a.1, the directory a host file a.1/b mirrors into. That backup stays
// beside the host, or activation would fail creating a.1/ over a file.
func TestBackupPathsInOnePlanDoNotNeedAReservedFileAsADirectory(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "a")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o700))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o600))
		sb.hostFile("a", "A ORIGINAL")
		sb.hostFile("a.1/b", "B ORIGINAL")
		sb.moduleFile("p", "a", "p")
		sb.moduleFile("p", "a.1/b", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		if got := sb.read("a.1/b.orig"); got != "B ORIGINAL" {
			t.Errorf("a.1/b's backup beside the host = %q", got)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
	})
}

// When a.1/b's backup has to stay beside the host, its name a.1/b.orig may
// be a path the same plan deploys; the backup takes the next free name
// instead, or activation finds a.1/b.orig occupied and aborts.
func TestBackupBesideTheHostDoesNotTakeAPlannedPath(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		stray := filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "a")
		must(t, os.MkdirAll(filepath.Dir(stray), 0o700))
		must(t, os.WriteFile(stray, []byte("left by a crash"), 0o600))
		sb.hostFile("a", "A ORIGINAL")
		sb.hostFile("a.1/b", "B ORIGINAL")
		sb.moduleFile("p", "a", "p")
		sb.moduleFile("p", "a.1/b", "p")
		sb.moduleFile("p", "a.1/b.orig", "p")
		sb.moduleFile("p", "a.1/b.orig.1/c", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		if got := sb.read("a.1/b.orig.2"); got != "B ORIGINAL" {
			t.Errorf("a.1/b's backup beside the host = %q", got)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
	})
}

// A name a plan reserves as a backup's directory is not chosen for a file,
// and a backup below a name reserved for a file is blocked.
func TestReservedSkipsDirectoriesAndBlocksBelowFiles(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a"), nil, 0o600))
	r := newReserved()
	if got := r.unused(filepath.Join(dir, "a.1", "b")); got != filepath.Join(dir, "a.1", "b") {
		t.Fatalf("unused = %s", got)
	}
	if got := r.unused(filepath.Join(dir, "a")); got != filepath.Join(dir, "a.2") {
		t.Errorf("unused = %s, want a.2: a.1 is a directory of the plan", got)
	}
	if !r.blocked(filepath.Join(dir, "a.2", "c")) || r.blocked(filepath.Join(dir, "a.1", "c")) {
		t.Error("blocked does not follow the reserved files")
	}
}

// A copy across filesystems that appeared before fsyncing its directory
// failed keeps the ledger's mark, so recovery drops the duplicate instead of
// taking it for the user's edit: activation rolls back, and teardown
// converges when run again.
func TestFailedSyncAfterACopyKeepsTheMark(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		failOnce := func() {
			syncCopyDir = func(string) error {
				syncCopyDir = syncDir
				return errors.New("injected fsync failure")
			}
		}
		t.Cleanup(func() { syncCopyDir = syncDir })

		failOnce()
		if err := sb.activate("p"); err == nil || !strings.Contains(err.Error(), "injected") || strings.Contains(err.Error(), "rollback incomplete") {
			t.Fatalf("activate = %v", err)
		}
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()

		must(t, sb.activate("p"))
		failOnce()
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate = %v", err)
		}
		if l, _ := sb.s.LoadLedger("p"); l == nil || !l.Entries[0].Copying {
			t.Fatalf("the mark was cleared: %+v", l)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if dangling, _ := sb.s.DanglingBackups(); len(dangling) != 0 {
			t.Errorf("dangling after convergence: %v", dangling)
		}
	})
}

// A restore whose directory fsync failed leaves the host's copy published
// but perhaps not durable, so teardown drops the duplicate backup only once
// the host's directory is synced: never if that sync fails too.
func TestDuplicateBackupStaysUntilTheHostIsSynced(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		var synced []string
		failOnce := func() {
			syncCopyDir = func(path string) error {
				synced = append(synced, path)
				syncCopyDir = syncDir
				return errors.New("injected fsync failure")
			}
		}
		t.Cleanup(func() { syncCopyDir = syncDir })

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		host, backup := l.Entries[0].Host, l.Entries[0].Backup
		failOnce()
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate = %v", err)
		}
		failOnce()
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate with the host unsynced = %v", err)
		}
		if want := []string{host, host}; !slices.Equal(synced, want) {
			t.Errorf("synced %v, want %v", synced, want)
		}
		if got, err := os.ReadFile(backup); err != nil || string(got) != "ORIGINAL" {
			t.Fatalf("the backup went before the host was durable: %q, %v", got, err)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if exists(backup) {
			t.Errorf("the duplicate backup %s stayed", backup)
		}
	})
}

// A host directory that can be written and searched but not read can't be
// opened for fsync. The duplicate backup still goes once the host is
// durable, by syncing the filesystem instead.
func TestDuplicateBackupGoesWhenTheHostDirectoryIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		t.Cleanup(func() { syncCopyDir = syncDir })

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		backup := l.Entries[0].Backup
		syncCopyDir = func(string) error {
			syncCopyDir = syncDir
			return errors.New("injected fsync failure")
		}
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate = %v", err)
		}

		must(t, os.Chmod(sb.host, 0o300))
		err = sb.deactivate("p", false)
		must(t, os.Chmod(sb.host, 0o755))
		must(t, err)
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if exists(backup) {
			t.Errorf("the duplicate backup %s stayed", backup)
		}
	})
}

// A filesystem forced read-only after a failed commit skips writeback, so a
// syncfs that succeeds there proves nothing; the duplicate backup stays.
func TestDuplicateBackupStaysOnAReadOnlyFilesystem(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		t.Cleanup(func() { syncCopyDir = syncDir; fstatfs = syscall.Fstatfs })

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		backup := l.Entries[0].Backup
		syncCopyDir = func(string) error {
			syncCopyDir = syncDir
			return errors.New("injected fsync failure")
		}
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate = %v", err)
		}

		fstatfs = func(fd int, st *syscall.Statfs_t) error {
			if err := syscall.Fstatfs(fd, st); err != nil {
				return err
			}
			st.Flags |= stRdonly
			return nil
		}
		must(t, os.Chmod(sb.host, 0o300))
		err = sb.deactivate("p", false)
		must(t, os.Chmod(sb.host, 0o755))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("deactivate on a read-only filesystem = %v", err)
		}
		if got, err := os.ReadFile(backup); err != nil || string(got) != "ORIGINAL" {
			t.Fatalf("the backup went on a read-only filesystem: %q, %v", got, err)
		}
	})
}

// A writeback error from the syncfs(2) fallback for an unreadable host
// directory must fail teardown and keep both the backup and the Copying mark.
func TestDuplicateBackupStaysWhenSyncfsFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		t.Cleanup(func() { syncCopyDir = syncDir; syncFS = syncfs })

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		backup := l.Entries[0].Backup
		syncCopyDir = func(string) error {
			syncCopyDir = syncDir
			return errors.New("injected fsync failure")
		}
		if err := sb.deactivate("p", false); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("deactivate = %v", err)
		}

		hostInfo, err := os.Stat(sb.host)
		must(t, err)
		hostDev := hostInfo.Sys().(*syscall.Stat_t).Dev
		fileInfo, err := os.Lstat(l.Entries[0].Host)
		must(t, err)
		fileIno := fileInfo.Sys().(*syscall.Stat_t).Ino

		var calls int
		var fdStat syscall.Stat_t
		syncFS = func(fd uintptr) error {
			calls++
			if ferr := syscall.Fstat(int(fd), &fdStat); ferr != nil {
				return ferr
			}
			return syscall.EIO
		}

		must(t, os.Chmod(sb.host, 0o300))
		err = sb.deactivate("p", false)
		must(t, os.Chmod(sb.host, 0o755))
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("deactivate with EIO from syncfs = %v, want EIO", err)
		}
		if calls != 1 {
			t.Errorf("syncFS calls = %d, want 1", calls)
		}
		if fdStat.Dev != hostDev || fdStat.Ino != fileIno {
			t.Errorf("syncFS fd = (dev %d, ino %d), want restored host file (dev %d, ino %d)", fdStat.Dev, fdStat.Ino, hostDev, fileIno)
		}
		if got, err := os.ReadFile(backup); err != nil || string(got) != "ORIGINAL" {
			t.Fatalf("the backup went despite syncfs EIO: %q, %v", got, err)
		}
		if l, err := sb.s.LoadLedger("p"); err != nil || l == nil || !l.Entries[0].Copying {
			t.Fatalf("the Copying mark was lost: %+v, %v", l, err)
		}

		if err := syncfs(^uintptr(0)); !errors.Is(err, syscall.EBADF) {
			t.Errorf("syncfs(invalid fd) = %v, want EBADF", err)
		}

		syncFS = syncfs
		must(t, os.Chmod(sb.host, 0o300))
		err = sb.deactivate("p", false)
		must(t, os.Chmod(sb.host, 0o755))
		must(t, err)
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if exists(backup) {
			t.Errorf("the duplicate backup %s stayed after recovery", backup)
		}
	})
}

// An original that can't be read can't be copied into a store on another
// filesystem; it is kept beside the host instead of failing the activation.
func TestUnreadableOriginalIsBackedUpBesideTheHost(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		must(t, os.Chmod(sb.hostFile("g", "ORIGINAL"), 0))
		sb.moduleFile("p", "g", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		if want := filepath.Join(sb.host, "g.orig"); l.Entries[0].Backup != want {
			t.Errorf("backup = %q, want %q", l.Entries[0].Backup, want)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
	})
}

// A move never replaces what is at its destination, whether it renames or
// copies.
func TestMoveDurableRefusesAnOccupiedDestination(t *testing.T) {
	for _, exdev := range []bool{false, true} {
		copyMoves(t, exdev)
		dir := t.TempDir()
		must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
		from, to := filepath.Join(dir, "from"), filepath.Join(dir, "sub", "to")
		must(t, os.WriteFile(from, []byte("from"), 0o644))
		must(t, os.WriteFile(to, []byte("to"), 0o644))
		if err := moveDurable(from, to, nil); !errors.Is(err, os.ErrExist) {
			t.Errorf("exdev=%v: moveDurable = %v, want ErrExist", exdev, err)
		}
		if got, _ := os.ReadFile(to); string(got) != "to" || !exists(from) {
			t.Errorf("exdev=%v: to = %q, from exists = %v", exdev, got, exists(from))
		}
	}
}

// Under a bind mount a rename fails with EXDEV though both paths report the
// same device, so a move copies there too. The journalled copy lets a crash
// between the copy and the removal of its source converge, in activation
// (rolled back) and in teardown (re-run).
func TestInterruptedCopyUnderABindMountConverges(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		sb.hostFile("f", "ORIGINAL")
		sb.moduleFile("p", "f", "p")
		before := sb.snapshot()
		crashAfterCopy := func(op func() error) {
			t.Helper()
			afterCopy = func() { panic(crash{}) }
			defer func() {
				afterCopy = nil
				if _, ok := recover().(crash); !ok {
					t.Fatal("the move did not copy")
				}
			}()
			_ = op()
		}

		crashAfterCopy(func() error { return sb.s.Activate(context.Background(), "p", sb.stack) })
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		host, backup := l.Entries[0].Host, l.Entries[0].Backup
		hi, err := os.Stat(host)
		must(t, err)
		bi, err := os.Stat(backup)
		must(t, err)
		if hi.Sys().(*syscall.Stat_t).Dev != bi.Sys().(*syscall.Stat_t).Dev {
			t.Fatal("the sandbox spans two devices")
		}
		must(t, sb.s.Rollback("p"))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()

		must(t, sb.activate("p"))
		crashAfterCopy(func() error { return sb.deactivate("p", false) })
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		sb.assertNoLedgers()
		if dangling, _ := sb.s.DanglingBackups(); len(dangling) != 0 {
			t.Errorf("dangling after convergence: %v", dangling)
		}
	})
}

// A labelled original can't be copied into a store on another filesystem;
// it is kept beside the host instead of failing the activation.
func TestUncopyableOriginalIsBackedUpBesideTheHost(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		copyMoves(t, true)
		setXattr(t, sb.hostFile("g", "ORIGINAL"))
		sb.moduleFile("p", "g", "p")
		before := sb.snapshot()

		must(t, sb.activate("p"))
		l, err := sb.s.LoadLedger("p")
		must(t, err)
		if want := filepath.Join(sb.host, "g.orig"); l.Entries[0].Backup != want || sb.read("g.orig") != "ORIGINAL" {
			t.Errorf("backup = %q, want %q", l.Entries[0].Backup, want)
		}
		must(t, sb.deactivate("p", false))
		assertSame(t, before, sb.snapshot())
		if n, err := syscall.Listxattr(filepath.Join(sb.host, "g"), nil); err != nil || n == 0 {
			t.Errorf("the restored original lost its attributes: %d, %v", n, err)
		}
	})
}

// A backup that can't be restored (it gained a label, and the store is now
// on another filesystem) is found before the deployment is removed, and the
// removal is refused with nothing changed.
func TestUnrestorableBackupIsRefusedBeforeTeardown(t *testing.T) {
	eachHalf(t, func(t *testing.T, sb *sandbox) {
		sb.hostFile("g", "ORIGINAL")
		sb.moduleFile("p", "g", "p")
		must(t, sb.activate("p"))
		setXattr(t, filepath.Join(sb.s.StateDir, "backups", "orig", "etc", "g"))
		copyMoves(t, true)
		deployed := sb.snapshot()

		err := sb.deactivate("p", false)
		if err == nil || !strings.Contains(err.Error(), "extended attributes") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("deactivate = %v", err)
		}
		assertSame(t, deployed, sb.snapshot())
		if l, _ := sb.s.LoadLedger("p"); l == nil || l.Status != StatusActive || len(l.Entries) != 1 {
			t.Errorf("ledger = %+v", l)
		}
		if sb.stored("backups/orig/etc/g") != "ORIGINAL" {
			t.Error("the backup left the store")
		}
	})
}

// The probe tells mounts apart without moving anything: /proc is always
// another mount.
func TestCrossMount(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	if crossMount(dir, filepath.Join(dir, "sub")) {
		t.Error("one mount reported as two")
	}
	if !crossMount(dir, "/proc") {
		t.Error("/proc reported as the same mount")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the probe left files: %v", entries)
	}
}
