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
// ('c', with or without the ref flag). Lookup or read errors are returned so
// unreadable files aren't misclassified.
func isPyc(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if isAbsent(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if isAbsent(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	var h [17]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return h[2] == '\r' && h[3] == '\n' && binary.LittleEndian.Uint32(h[4:8]) <= 3 && h[16]&^0x80 == 'c', nil
}

// isAbsent reports whether err proves nothing exists at the path: either it
// is not found, or an ancestor is not a directory.
func isAbsent(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func statExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if isAbsent(err) {
		return false, nil
	}
	return false, err
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
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
func missingDirs(dir string) ([]string, error) {
	var missing []string
	for {
		ok, err := statExists(dir)
		if err != nil {
			return nil, fmt.Errorf("inspecting %q: %w", dir, err)
		}
		if ok {
			break
		}
		missing = append([]string{dir}, missing...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return missing, nil
}

// uniquePath returns base if it is free (not on disk and not in reserved),
// otherwise base.1, base.2, ...
func uniquePath(base string, reserved map[string]bool) (string, error) {
	for i := 0; ; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s.%d", base, i)
		}
		if reserved[candidate] {
			continue
		}
		ok, err := statExists(candidate)
		if err != nil {
			return "", fmt.Errorf("checking %q: %w", candidate, err)
		}
		if !ok {
			return candidate, nil
		}
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
// Lookup or read errors are returned so unreadable files aren't treated as
// foreign or absent.
func owns(e Entry, loc string) (bool, error) {
	info, err := os.Lstat(loc)
	if err != nil {
		if isAbsent(err) {
			return false, nil
		}
		return false, err
	}
	switch e.Kind {
	case KindLink:
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		target, err := os.Readlink(loc)
		if err != nil {
			if isAbsent(err) {
				return false, nil
			}
			return false, err
		}
		return target == e.Src, nil
	case KindCopy:
		if !info.Mode().IsRegular() {
			return false, nil
		}
		sum, err := hashFile(loc)
		if err != nil {
			if isAbsent(err) {
				return false, nil
			}
			return false, err
		}
		return sum == e.SHA256, nil
	}
	return false, nil
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
	tmp, err := uniquePath(link+".arcus-tmp", nil)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := renameDurable(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
