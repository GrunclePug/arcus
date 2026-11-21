package layer

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// access(2) mode bits; the syscall package does not export them on Linux.
const (
	accessW = 0x2
	accessX = 0x1
)

// walkTree walks a module tree like filepath.WalkDir but never enters a
// Python bytecode cache. Python writes __pycache__ next to a module's scripts
// whenever they run from the tree (a test, a hook importing a sibling);
// deployed, it would churn on every run and its stale links break removals
// once the source moves.
func walkTree(tree string, fn iofs.WalkDirFunc) error {
	return filepath.WalkDir(tree, func(path string, d iofs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "__pycache__" {
			return filepath.SkipDir
		}
		return fn(path, d, err)
	})
}

// inCache reports whether e lies inside a Python bytecode cache: a directory
// walkTree skips. The host path relative to the target is the path within the
// module tree.
func (s *Stacker) inCache(e Entry) bool {
	rel, _ := filepath.Rel(s.Target, e.Host)
	return slices.Contains(strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/"), "__pycache__")
}

// cacheEntry reports whether e is a home-half link to a Python bytecode cache
// file (<module>.<tag>[.opt-N].pyc), which ledgers written before 0.8.2 can
// hold. It agrees with inCache for every reachable layout, because Python never
// nests cache directories: e.Src's parent lies inside the module tree or is the
// tree itself (home or system).
func cacheEntry(e Entry) bool {
	stem, ok := strings.CutSuffix(filepath.Base(e.Src), ".pyc")
	tag := strings.LastIndexByte(stem, '.') // the module name may itself hold dots
	return e.Kind == KindLink && filepath.Base(filepath.Dir(e.Src)) == "__pycache__" &&
		ok && tag > 0 && tag < len(stem)-1
}

// isPyc reports whether path is a regular file laid out like a CPython
// bytecode cache: a magic number ending in \r\n, a flags word importlib
// accepts (at most 3), 8 bytes of source stamp, then a marshalled code object
// ('c', with or without the ref flag).
func isPyc(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var h [17]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return false
	}
	return h[2] == '\r' && h[3] == '\n' && binary.LittleEndian.Uint32(h[4:8]) <= 3 && h[16]&^0x80 == 'c'
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// unreadable reports whether path cannot be stat'd for lack of permission.
// A missing path is not unreadable: the parent could be searched, and the
// final component is absent.
func unreadable(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrPermission)
}

// isWithin reports whether path is root or lies beneath it.
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// VerifyWritePrivilege checks that the nearest existing ancestor directory of
// path is a directory this process can create entries in. Unlike checking the
// immediate parent, this accepts targets whose parent directories do not exist
// yet and will be created during activation.
func VerifyWritePrivilege(path string) error {
	dir := filepath.Dir(path)
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("cannot place %q: ancestor %q is not a directory", path, dir)
			}
			if err := syscall.Access(dir, accessW|accessX); err != nil {
				return fmt.Errorf("insufficient privilege to write into %q: %w", dir, err)
			}
			return nil
		}
		if !errors.Is(err, iofs.ErrNotExist) {
			return fmt.Errorf("inspecting %q: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
		dir = parent
	}
}

// missingDirs returns the ancestors of dir (inclusive) that do not exist yet,
// shallowest first, i.e. the directories os.MkdirAll(dir) would create.
func missingDirs(dir string) []string {
	var missing []string
	for !exists(dir) {
		missing = append([]string{dir}, missing...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return missing
}

// uniquePath returns base if it is free, otherwise base.1, base.2, ...
func uniquePath(base string) string {
	path := base
	for i := 1; exists(path); i++ {
		path = fmt.Sprintf("%s.%d", base, i)
	}
	return path
}

// reserved holds the paths a plan will create: the files it deploys, the
// backups it chose and the directories they need. A plan's destinations
// don't exist until it runs, so a backup named a.1 because a's was taken
// would otherwise also be chosen for a host file named a.1, or need a.1 as
// the directory of a.1/b, and a backup beside the host could take the name
// of a file the plan deploys.
type reserved struct{ files, dirs map[string]bool }

func newReserved() reserved {
	return reserved{files: map[string]bool{}, dirs: map[string]bool{}}
}

// blocked reports whether a directory path needs is a file already reserved.
func (r reserved) blocked(path string) bool {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		if r.files[dir] {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}

// unused is uniquePath that also skips the paths reserved as files or
// directories, and reserves the one it returns and its directories.
func (r reserved) unused(base string) string {
	path := base
	for i := 1; exists(path) || r.files[path] || r.dirs[path]; i++ {
		path = fmt.Sprintf("%s.%d", base, i)
	}
	r.add(path)
	return path
}

// add reserves path as a file and its directories.
func (r reserved) add(path string) {
	r.files[path] = true
	for dir := filepath.Dir(path); !r.dirs[dir]; dir = filepath.Dir(dir) {
		r.dirs[dir] = true
	}
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readRegular reads a module file that is about to be copied as root. It
// refuses symlinks, so a user-writable module can't make root copy a file
// the user could not read.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(f)
}

// copyMode is the mode a system file is deployed with: 0755 if the module
// file is executable by anyone, 0644 otherwise. It never depends on the
// user's umask and never carries write bits for group or others.
func copyMode(src os.FileMode) string {
	if src&0o111 != 0 {
		return "0755"
	}
	return "0644"
}

func parseMode(mode string) (os.FileMode, error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", mode, err)
	}
	return os.FileMode(m), nil
}

// owns reports whether loc currently holds exactly the deployment described by e.
func owns(e Entry, loc string) bool {
	info, err := os.Lstat(loc)
	if err != nil {
		return false
	}
	switch e.Kind {
	case KindLink:
		if info.Mode()&os.ModeSymlink == 0 {
			return false
		}
		target, err := os.Readlink(loc)
		return err == nil && target == e.Src
	case KindCopy:
		if !info.Mode().IsRegular() {
			return false
		}
		sum, err := hashFile(loc)
		return err == nil && sum == e.SHA256
	}
	return false
}

// renameDurable renames and fsyncs both parent directories.
func renameDurable(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if err := atomicfile.SyncDir(filepath.Dir(to)); err != nil {
		return err
	}
	if filepath.Dir(from) != filepath.Dir(to) {
		return atomicfile.SyncDir(filepath.Dir(from))
	}
	return nil
}

func removeDurable(path string) error {
	if err := os.Remove(path); err != nil {
		return err
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

// symlinkReplace points link at target, atomically replacing anything at link.
func symlinkReplace(target, link string) error {
	tmp := uniquePath(link + ".arcus-tmp")
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := renameDurable(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// moveDurable moves a file or symlink and fsyncs the directories involved.
// Backups and moved-aside files live in the state directory, which can be on
// another filesystem than the host path (/etc and a separate /var), where
// rename fails with EXDEV; then the file is copied and the original removed.
// A copy keeps content, mode, owner and mtime, and a hard link becomes a
// plain file. It would lose extended attributes (POSIX ACLs, SELinux labels,
// capabilities) and could widen access to the file, so a regular file that
// has any is not copied, nor are directories and special files
// (errCannotCopy).
//
// A move never replaces a file at to; it fails with an error wrapping
// fs.ErrExist instead.
//
// A crash can leave both copies, never neither: the destination only appears
// by an atomic rename of a fully written, fsynced temp file, and the
// original is removed only after that rename and its directory fsync. When
// journal is given, it is called with true before the copy and with false
// once the source is gone, so the ledger records the window in which both
// can exist, and planStep recognises an identical pair inside it as an
// interrupted move:
//   - activation (host to backups/): the host still holds the original and
//     the backup duplicates it. Rollback removes the duplicate; nothing was
//     deployed yet.
//   - teardown (backups/ to host): the host already holds the restored
//     original and the backup duplicates it. Re-running removes the duplicate.
//   - moving a foreign file aside (no journal): the source is still in place
//     and the next run moves it again, to the next free name; one more file
//     in foreign/.
//
// A temp file left by a crash ends in ".arcus-tmp" next to its destination. In
// the backup store doctor reports it as a backup no ledger references; next to
// a host path it is a harmless stray.
func moveDurable(from, to string, journal func(copying bool) error) error {
	if exists(to) {
		return fmt.Errorf("moving %q: %q %w", from, to, iofs.ErrExist)
	}
	err := error(syscall.EXDEV)
	if !forceExdev || filepath.Dir(from) == filepath.Dir(to) {
		err = renameDurable(from, to)
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if journal != nil {
		if err := journal(true); err != nil {
			return err
		}
	}
	if err := copyDurable(from, to); err != nil {
		err = fmt.Errorf("copying %q across filesystems: %w", from, err)
		if journal != nil && !exists(to) {
			// No copy appeared: copyDurable removes its temp file. One that
			// appeared before a later step failed keeps the mark, so the
			// duplicate is recognised.
			err = errors.Join(err, journal(false))
		}
		return err
	}
	if afterCopy != nil {
		afterCopy()
	}
	if err := removeDurable(from); err != nil {
		return err
	}
	if journal != nil {
		return journal(false)
	}
	return nil
}

// Test seams: forceExdev makes every move between two directories take the
// copy path, as if they were on different mounts, afterCopy runs between
// the copy appearing and the source going, where tests crash,
// syncCopyDir fsyncs the directory a copy appeared in, where tests fail, and
// syncFS runs syncfs(2) when that directory can't be opened for fsync.
var (
	forceExdev  bool
	afterCopy   func()
	syncCopyDir = syncDir
	syncFS      = syncfs
	fstatfs     = syscall.Fstatfs
)

// stRdonly is ST_RDONLY in statfs(2)'s f_flags.
const stRdonly = 1

// syncDir fsyncs the parent directory of path. A directory that can be
// written and searched but not read can't be opened for fsync; then
// syncfs(2) on path itself flushes that one filesystem, the directory entry
// included, and reports writeback errors (Linux 5.8+). If path can't be
// opened either, the error stands rather than claiming durability.
func syncDir(path string) error {
	err := atomicfile.SyncDir(filepath.Dir(path))
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	f, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if openErr != nil {
		return fmt.Errorf("%w; syncfs fallback: %w", err, openErr)
	}
	defer f.Close()
	// A filesystem forced read-only after a failed commit skips writeback,
	// so syncfs succeeding there proves nothing.
	var st syscall.Statfs_t
	if err := fstatfs(int(f.Fd()), &st); err != nil {
		return fmt.Errorf("statfs via %q: %w", path, err)
	}
	if st.Flags&stRdonly != 0 {
		return fmt.Errorf("the filesystem of %q is read-only; its directory entry may not be durable", path)
	}
	if err := syncFileFS(f); err != nil {
		return fmt.Errorf("syncfs via %q: %w", path, err)
	}
	return nil
}

// syncFileFS runs syncFS on the descriptor of f.
func syncFileFS(f *os.File) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var syncErr error
	if err := raw.Control(func(fd uintptr) {
		syncErr = syncFS(fd)
	}); err != nil {
		return err
	}
	return syncErr
}

// syncfs flushes the filesystem containing fd via syncfs(2) and returns any
// writeback error.
func syncfs(fd uintptr) error {
	for {
		_, _, errno := syscall.Syscall(sysSyncfs, fd, 0, 0)
		if errno == 0 {
			return nil
		}
		if errno != syscall.EINTR {
			return errno
		}
	}
}

// errCannotCopy is returned for what a copy across filesystems can't carry:
// anything but a regular file or a symlink, or a regular file with extended
// attributes.
var errCannotCopy = errors.New("cannot be copied across filesystems")

// copyDurable writes a copy of from at to, keeping a regular file's content,
// mode, owner and mtime or recreating a symlink, and fsyncs it. Anything
// else, and a regular file with extended attributes, is refused. to only ever
// appears complete.
func copyDurable(from, to string) error {
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if err := copyable(from, info); err != nil {
		return err
	}
	tmp := uniquePath(to + ".arcus-tmp")
	if info.Mode()&os.ModeSymlink != 0 {
		err = copyLink(from, tmp, info)
	} else {
		err = copyFile(from, tmp, info)
	}
	if err == nil {
		err = os.Rename(tmp, to)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncCopyDir(to)
}

// copyable returns errCannotCopy for what copyDurable refuses or can't
// read: anything but a regular file or a symlink, or a regular file with
// extended attributes or that can't be opened for reading.
func copyable(path string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return nil
	case info.Mode().IsRegular():
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("%w: %w", err, errCannotCopy)
		}
		f.Close()
		return noXattrs(path)
	}
	return fmt.Errorf("%s is neither a regular file nor a symlink: %w", path, errCannotCopy)
}

// movable returns why the file from can't be moved between storeDir, a
// directory in the state directory, and hostDir, in either direction: across
// mounts a move copies, and some files can't be copied. Both directories
// must exist.
func movable(from, storeDir, hostDir string) error {
	if !crossMount(storeDir, hostDir) {
		return nil
	}
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	return copyable(from, info)
}

// crossMount reports whether a rename between storeDir and hostDir fails with
// EXDEV. It is exact where comparing devices is not: a bind mount reports the
// same device but still refuses the rename. It renames a name that doesn't
// exist in storeDir, which is arcus's own: the kernel compares the two
// directories' mounts before it looks the source up, so nothing moves.
func crossMount(storeDir, hostDir string) bool {
	if storeDir == hostDir {
		return false
	}
	if forceExdev {
		return true
	}
	probe := uniquePath(filepath.Join(storeDir, ".arcus-probe"))
	return errors.Is(syscall.Rename(probe, filepath.Join(hostDir, ".arcus-probe")), syscall.EXDEV)
}

// existingDir returns dir or its nearest ancestor that exists.
func existingDir(dir string) string {
	for {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

// noXattrs refuses a file with extended attributes, which a copy would drop.
// A symlink's access isn't governed by its own ACL or capabilities, so only
// regular files are checked.
func noXattrs(path string) error {
	n, err := syscall.Listxattr(path, nil)
	switch {
	case errors.Is(err, syscall.ENOTSUP):
		return nil
	case err != nil:
		return fmt.Errorf("listing the extended attributes of %s: %w", path, err)
	case n > 0:
		return fmt.Errorf("%s has extended attributes (an ACL, a label or capabilities): %w", path, errCannotCopy)
	}
	return nil
}

func copyLink(from, tmp string, info os.FileInfo) error {
	target, err := os.Readlink(from)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return os.Lchown(tmp, int(st.Uid), int(st.Gid))
	}
	return nil
}

func copyFile(from, tmp string, info os.FileInfo) error {
	src, err := os.OpenFile(from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	// Owner before mode: chown clears setuid and setgid.
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := dst.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return err
		}
	}
	if err := dst.Chmod(info.Mode()); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return dst.Close()
}

// identical reports whether a and b are the same file or symlink as a copy
// makes them: the same owner, and the same link target or a regular file
// with the same content and permissions.
func identical(a, b string) bool {
	ia, errA := os.Lstat(a)
	ib, errB := os.Lstat(b)
	if errA != nil || errB != nil {
		return false
	}
	sa, okA := ia.Sys().(*syscall.Stat_t)
	sb, okB := ib.Sys().(*syscall.Stat_t)
	if !okA || !okB || sa.Uid != sb.Uid || sa.Gid != sb.Gid {
		return false
	}
	switch {
	case ia.Mode()&os.ModeSymlink != 0 && ib.Mode()&os.ModeSymlink != 0:
		ta, errA := os.Readlink(a)
		tb, errB := os.Readlink(b)
		return errA == nil && errB == nil && ta == tb
	case ia.Mode().IsRegular() && ib.Mode().IsRegular():
		if ia.Mode().Perm() != ib.Mode().Perm() {
			return false
		}
		sa, errA := hashFile(a)
		sb, errB := hashFile(b)
		return errA == nil && errB == nil && sa == sb
	}
	return false
}
