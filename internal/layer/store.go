package layer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// Files arcus displaces live in the half's state directory instead of next
// to the host file, so $HOME and /etc stay free of sidecar files:
//
//	<StateDir>/backups/<tag>/<path below Target>   what a layer replaced
//	<StateDir>/foreign/<path below Target>         edited files -f moved aside

func (s *Stacker) backupsDir() string { return filepath.Join(s.StateDir, "backups") }

func (s *Stacker) foreignDir() string { return filepath.Join(s.StateDir, "foreign") }

// storePath mirrors host below dir. A host outside Target (a ledger moved
// from another root) keeps its absolute path below dir rather than escaping it.
func (s *Stacker) storePath(dir, host string) string {
	return filepath.Join(dir, strings.TrimLeft(s.relTarget(host), string(filepath.Separator)))
}

// storeOr returns path, a destination in the store, or sidecar when an
// existing directory on its way below the state directory is not a real
// directory: a file arcus kept there earlier where a directory would now
// go, or a symlink. Creating path's directories would fail there, or follow
// the symlink out of the store, so the file stays next to the host instead,
// as older versions kept it.
func (s *Stacker) storeOr(path, sidecar string) string {
	for dir := filepath.Dir(path); dir != s.StateDir && isWithin(dir, s.StateDir); dir = filepath.Dir(dir) {
		if info, err := os.Lstat(dir); err == nil && !info.IsDir() {
			return sidecar
		}
	}
	return path
}

// backupPath is where the file displaced from host is kept for tag: "orig"
// for the host's own file, or the module whose deployment it was.
func (s *Stacker) backupPath(host, tag string) string {
	return s.storePath(filepath.Join(s.backupsDir(), tag), host)
}

// BackupTag returns the tag of a backup in the store, or "" for any other
// path, such as the sidecar files older arcus versions left next to the host.
func (s *Stacker) BackupTag(backup string) string {
	if backup == s.backupsDir() || !isWithin(backup, s.backupsDir()) {
		return ""
	}
	rel, err := filepath.Rel(s.backupsDir(), backup)
	if err != nil {
		return ""
	}
	tag, _, _ := strings.Cut(rel, string(filepath.Separator))
	return tag
}

// moveIn moves from to a path in the store, creating its parent directories.
// They are arcus's own data, so no ledger records them. journal is as for
// moveDurable.
func moveIn(from, to string, journal func(copying bool) error) error {
	if err := mkdirAllDurable(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	return moveDurable(from, to, journal)
}

// restoredDirMode is the mode of a directory a restore recreates because
// the user removed it along with the deployment. Its old mode is unknown. On
// the home half it may have been private, like ~/.ssh, so it comes back
// readable only by the user; a mode too strict is safer than one that
// exposes the restored file. The system half's directories in /etc and
// /usr/local are read by other users and services, as when activation
// creates them.
func (s *Stacker) restoredDirMode() fs.FileMode {
	if s.Copy {
		return 0o755
	}
	return 0o700
}

// mkdirAllDurable creates dir and its missing parents, then fsyncs the
// directory that gained each of them: a file moved into dir is lost with
// its directory if the directory's own entry never reached the disk.
func mkdirAllDurable(dir string, mode fs.FileMode) error {
	missing := missingDirs(dir)
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}
	for _, d := range missing {
		if err := atomicfile.SyncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}

// pruneStore removes the directories that taking path out of the store left
// empty, up to its tag directory (backups) or foreign/ itself. It is
// housekeeping: a directory that can't go is simply left. Paths outside the
// store, such as legacy sidecar backups, are left alone.
func (s *Stacker) pruneStore(path string) {
	var stop string
	switch {
	case s.BackupTag(path) != "":
		stop = filepath.Join(s.backupsDir(), s.BackupTag(path))
	case path != s.foreignDir() && isWithin(path, s.foreignDir()):
		stop = s.foreignDir()
	default:
		return
	}
	for dir := filepath.Dir(path); dir != stop && isWithin(dir, stop); dir = filepath.Dir(dir) {
		if syscall.Rmdir(dir) != nil {
			return
		}
		_ = atomicfile.SyncDir(filepath.Dir(dir))
	}
}

// relTarget is host below Target, or host itself if it lies outside.
func (s *Stacker) relTarget(host string) string {
	rel, err := filepath.Rel(s.Target, host)
	if err != nil || !filepath.IsLocal(rel) {
		return host
	}
	return rel
}
