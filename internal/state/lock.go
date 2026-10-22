package state

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"syscall"
)

// Lock is an advisory flock(2) held on a half's lock file for the lifetime of
// a command. It serializes every read-modify-write of that half's state and
// ledgers across concurrent arcus processes. The kernel drops the lock if the
// process dies, so a crash can never leave a stale lock behind.
type Lock struct {
	f *os.File
}

// AcquireLock takes the lock without blocking. Read-only commands pass
// exclusive=false so that several of them may run at once. A shared lock
// opens the file read-only, so the user can take one on the system half's
// root-owned lock file; if that file doesn't exist yet, there is no state to
// protect and the returned lock is a no-op.
func AcquireLock(path string, exclusive bool) (*Lock, error) {
	var f *os.File
	var err error
	if exclusive {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	} else {
		f, err = os.Open(path)
		if errors.Is(err, iofs.ErrNotExist) {
			return &Lock{}, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another arcus process is running (lock %s is held)", path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l.f == nil {
		return nil
	}
	defer l.f.Close()
	return syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
}
