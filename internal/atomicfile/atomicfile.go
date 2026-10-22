// Package atomicfile provides crash-safe whole-file replacement.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data so that a reader (or a reboot) observes either
// the complete old content or the complete new content, never a torn write.
// The temp file is fsynced before the rename and the parent directory is
// fsynced after it, so the rename itself survives power loss.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file for %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath) // no-op after a successful rename
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing %q: %w", path, err)
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so that entries created, renamed or removed in it
// are durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %q for fsync: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %q: %w", dir, err)
	}
	return nil
}
