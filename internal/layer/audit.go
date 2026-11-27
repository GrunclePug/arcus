package layer

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Audit compares an active module's ledger with the live filesystem and
// returns human-readable drift findings. activeStack is the full stack.
func (s *Stacker) Audit(module string, activeStack []string) ([]string, error) {
	stack, err := s.loadStack(activeStack)
	if err != nil {
		return nil, err
	}
	pos := -1
	for i, name := range activeStack {
		if name == module {
			pos = i
		}
	}
	if pos < 0 || stack[pos] == nil {
		return nil, nil
	}
	l, above := stack[pos], stack[pos+1:]

	var symlinkHits []symlinkHit
	var entryFindings []string
	deployed := make(map[string]bool, len(l.Entries))
	for i, e := range l.Entries {
		deployed[e.Src] = true
		// Below a symlinked parent the deployment can't be judged, and
		// teardown refuses; the module's source can still be checked.
		if dir, perr := s.parentLink(e.Host); dir != "" {
			symlinkHits = append(symlinkHits, symlinkHit{dir, e.Host})
		} else if perr != nil {
			entryFindings = append(entryFindings, s.inspectFinding(e.Host, perr))
		} else {
			entryFindings = append(entryFindings, s.auditDeployment(l, i, above)...)
		}
		srcExists, serr := statExists(e.Src)
		if serr != nil {
			entryFindings = append(entryFindings, fmt.Sprintf("%s: cannot inspect source %s: %v", e.Host, e.Src, serr))
		} else if !srcExists {
			entryFindings = append(entryFindings, fmt.Sprintf("%s: source %s was deleted from the module", e.Host, e.Src))
		} else if e.Kind == KindCopy {
			sum, herr := hashFile(e.Src)
			if herr != nil {
				entryFindings = append(entryFindings, fmt.Sprintf("%s: cannot inspect source %s: %v", e.Host, e.Src, herr))
			} else if sum != e.SHA256 {
				entryFindings = append(entryFindings, fmt.Sprintf("%s: source %s changed since deployment", e.Host, e.Src))
			}
		}
	}
	findings := append(formatSymlinkGroups(symlinkHits, teardownSymlinkAdvice), entryFindings...)

	var added int
	if tree := s.tree(module); tree != "" {
		_ = walkTree(tree, func(src string, d iofs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && !deployed[src] {
				added++
			}
			return nil
		})
	}
	if added > 0 {
		findings = append(findings, fmt.Sprintf("%d file(s) added to %s/ since the module was turned on", added, s.Subdir))
	}
	return findings, nil
}

func (s *Stacker) inspectFinding(path string, err error) string {
	if s.Copy && os.Geteuid() != 0 && errors.Is(err, iofs.ErrPermission) {
		return fmt.Sprintf("%s: cannot inspect without root: %v", path, err)
	}
	return fmt.Sprintf("%s: cannot inspect: %v", path, err)
}

// auditDeployment reports the drift of entry idx's deployment and backup.
func (s *Stacker) auditDeployment(l *Ledger, idx int, above []*Ledger) []string {
	e := l.Entries[idx]
	st, err := s.planStep(l, idx, above)
	if err != nil {
		return []string{s.inspectFinding(e.Host, err)}
	}
	var findings []string
	switch {
	case st.foreign:
		findings = append(findings, fmt.Sprintf("%s was modified after activation (teardown will require -f)", st.loc))
	case st.cache:
		// A cache Python rewrote; the next restack removes it.
	case !st.remove && !st.finished:
		findings = append(findings, fmt.Sprintf("%s: deployment is missing", e.Host))
	case st.remove && e.Kind == KindCopy:
		if f := s.attrDrift(e, st.loc); f != "" {
			findings = append(findings, f)
		}
	}
	if e.Backup != "" && !st.finished {
		backupExists, berr := statExists(e.Backup)
		if berr != nil {
			findings = append(findings, s.inspectFinding(e.Backup, berr))
		} else if !backupExists {
			findings = append(findings, fmt.Sprintf("%s: recorded backup %s is missing", e.Host, e.Backup))
		}
	}
	return findings
}

// attrDrift reports a copy whose mode or owner no longer match the ledger.
func (s *Stacker) attrDrift(e Entry, loc string) string {
	info, err := os.Lstat(loc)
	if err != nil {
		if isAbsent(err) {
			return ""
		}
		return s.inspectFinding(loc, err)
	}
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	uid, gid := -1, -1
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(st.Uid), int(st.Gid)
	}
	if mode != e.Mode || uid != e.UID || gid != e.GID {
		return fmt.Sprintf("%s: mode/owner is %s %d:%d, deployed as %s %d:%d", loc, mode, uid, gid, e.Mode, e.UID, e.GID)
	}
	return ""
}

// DanglingBackups looks next to every host path any known module can deploy
// for files named <path>.orig, <path>.<module> or <path>.<tag>.<N> that no
// ledger references. These are leftovers of crashes. The scan is bounded to
// those paths; it never crawls the target.
func (s *Stacker) DanglingBackups() ([]string, error) {
	tags := map[string]bool{"orig": true}
	modules := make([]string, 0, len(s.Dirs))
	for p := range s.Dirs {
		tags[p] = true
		modules = append(modules, p)
	}
	slices.Sort(modules)

	referenced := map[string]bool{}
	ledgers, err := s.ListLedgers()
	if err != nil {
		return nil, err
	}
	for _, name := range ledgers {
		tags[name] = true
		l, err := s.LoadLedger(name)
		if err != nil {
			return nil, err
		}
		for _, e := range l.Entries {
			if e.Backup != "" {
				referenced[e.Backup] = true
			}
			// Deployed files and their parent directories are not backups
			// even if their name looks like <other>.orig.
			referenced[e.Host] = true
			for dir := filepath.Dir(e.Host); dir != s.Target && isWithin(dir, s.Target); dir = filepath.Dir(dir) {
				referenced[dir] = true
			}
		}
	}

	seen := map[string]bool{}
	var dangling []string
	for _, p := range modules {
		tree := s.tree(p)
		err := walkTree(tree, func(src string, d iofs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			host, err := s.hostPathFor(tree, src)
			if err != nil || seen[host] {
				return err
			}
			seen[host] = true
			if dir, perr := s.parentLink(host); dir != "" || perr != nil {
				return nil
			}
			matches, _ := filepath.Glob(escapeGlob(host) + ".*")
			for _, m := range matches {
				if !referenced[m] && tags[backupTag(strings.TrimPrefix(m, host+"."))] {
					dangling = append(dangling, m)
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return dangling, nil
}

// backupTag strips an optional numeric collision suffix: "orig.2" -> "orig".
func backupTag(suffix string) string {
	if i := strings.LastIndexByte(suffix, '.'); i >= 0 && strings.Trim(suffix[i+1:], "0123456789") == "" && i+1 < len(suffix) {
		return suffix[:i]
	}
	return suffix
}

func escapeGlob(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(p)
}

// Change is one difference between a deployed layer and its module tree.
type Change struct {
	Op   byte   // '+' added to the module, '-' removed from it, '~' copy source changed
	Host string // the deployed (or to be deployed) path
	Src  string // the module file
}

// Stale lists how module's layer differs from its tree: files added to or
// removed from the module since it was turned on, and copies whose source
// content or executable bit changed. Symlinked files follow their source by
// themselves and are never stale. An empty result means the layer is current.
func (s *Stacker) Stale(module string) ([]Change, error) {
	l, err := s.LoadLedger(module)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, fmt.Errorf("module %q has no %s ledger; run 'arcus doctor'", module, s.Subdir)
	}

	var changes []Change
	recorded := make(map[string]bool, len(l.Entries))
	for _, e := range l.Entries {
		recorded[e.Src] = true
		info, err := os.Lstat(e.Src)
		switch {
		case err != nil, s.inCache(e):
			// A cache deployed before caches were skipped goes with the next redeploy.
			changes = append(changes, Change{'-', e.Host, e.Src})
		case e.Kind == KindCopy:
			sum, err := hashFile(e.Src)
			if err != nil || sum != e.SHA256 || copyMode(info.Mode()) != e.Mode {
				changes = append(changes, Change{'~', e.Host, e.Src})
			}
		}
	}

	tree := s.tree(module)
	if tree == "" {
		return changes, nil
	}
	err = walkTree(tree, func(src string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() || recorded[src] {
			return err
		}
		host, err := s.hostPathFor(tree, src)
		if err != nil {
			return err
		}
		changes = append(changes, Change{'+', host, src})
		return nil
	})
	if err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return nil, fmt.Errorf("scanning %s of %q: %w", s.Subdir, module, err)
	}
	return changes, nil
}
