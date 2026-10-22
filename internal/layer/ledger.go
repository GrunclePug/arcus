package layer

import (
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// Ledger statuses. Anything other than StatusActive means an operation on the
// module was interrupted and must be reconciled by `arcus doctor -fix`.
const (
	StatusActivating   = "activating"
	StatusActive       = "active"
	StatusDeactivating = "deactivating"
)

// Deployment kinds.
const (
	KindLink = "link" // host path is a symlink to Src (home half)
	KindCopy = "copy" // host path is a regular file whose content hashes to SHA256 (system half)
)

// Entry records one host path a module deployed onto.
//
// Backup is the exact path the displaced predecessor was renamed to (empty if
// the host path did not exist). It is recorded rather than re-derived from the
// stack, so teardown never has to guess which ".orig"/".<layer>" file is ours.
//
// Copies also record the mode (octal) and owner they were deployed with, so
// drift can be reported. An absent uid or gid is 0 (root).
type Entry struct {
	Host   string `json:"host"`
	Src    string `json:"src"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   string `json:"mode,omitempty"`
	UID    int    `json:"uid,omitempty"`
	GID    int    `json:"gid,omitempty"`
	Backup string `json:"backup,omitempty"`
}

// LedgerVersion is the ledger file format. LoadLedger refuses any other.
const LedgerVersion = 1

// Ledger is the write-ahead journal of a module's layer in one half. Every
// entry is persisted before its host mutation happens, so after a crash the
// ledger always describes a superset of what is on disk.
type Ledger struct {
	Version     int      `json:"version"`
	Module      string   `json:"module"`
	Status      string   `json:"status"`
	Entries     []Entry  `json:"entries"`
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

func (l *Ledger) find(host string) int {
	for i := range l.Entries {
		if l.Entries[i].Host == host {
			return i
		}
	}
	return -1
}

func (s *Stacker) ledgerDir() string {
	return filepath.Join(s.StateDir, "ledger")
}

func (s *Stacker) ledgerPath(module string) string {
	return filepath.Join(s.ledgerDir(), module+".json")
}

// LoadLedger returns the module's ledger, or nil if it has none.
func (s *Stacker) LoadLedger(module string) (*Ledger, error) {
	data, err := os.ReadFile(s.ledgerPath(module))
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading ledger for %q: %w", module, err)
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parsing ledger for %q: %w", module, err)
	}
	if l.Version != LedgerVersion {
		return nil, fmt.Errorf("ledger for %q has format %d; this arcus reads format %d", module, l.Version, LedgerVersion)
	}
	return &l, nil
}

func (s *Stacker) saveLedger(l *Ledger) error {
	l.Version = LedgerVersion
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding ledger for %q: %w", l.Module, err)
	}
	if err := atomicfile.Write(s.ledgerPath(l.Module), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("saving ledger for %q: %w", l.Module, err)
	}
	return nil
}

func (s *Stacker) removeLedger(module string) error {
	if err := os.Remove(s.ledgerPath(module)); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("removing ledger for %q: %w", module, err)
	}
	return atomicfile.SyncDir(s.ledgerDir())
}

// ListLedgers returns the names of all modules that have a ledger on disk.
func (s *Stacker) ListLedgers() ([]string, error) {
	entries, err := os.ReadDir(s.ledgerDir())
	if errors.Is(err, iofs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing ledgers: %w", err)
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// loadStack loads the ledgers for a stack slice in order. Modules without a
// ledger yield nil.
func (s *Stacker) loadStack(names []string) ([]*Ledger, error) {
	out := make([]*Ledger, len(names))
	for i, name := range names {
		l, err := s.LoadLedger(name)
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}
