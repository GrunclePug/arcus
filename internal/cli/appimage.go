package cli

import (
	"errors"
	"fmt"
	iofs "io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/grunclepug/arcus/internal/appimage"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
)

func (a *app) httpClient() *http.Client {
	if a.client != nil {
		return a.client
	}
	return appimage.Client
}

// appImage carries out an AppImage step of the user half: a download, a
// take-over or a record update. It refuses if the file or its record no
// longer call for what the plan showed.
func (a *app) appImage(h *half, st *state.State, s plan.Step, in plan.Input, save func() error) error {
	m := in.Modules[s.Module]
	if m == nil {
		return fmt.Errorf("module %q does not exist", s.Module)
	}
	for _, p := range s.Names {
		_, d, ok := m.AppImageAt(p)
		if !ok {
			return fmt.Errorf("module %s declares no AppImage at ~/%s", s.Module, p)
		}
		path := filepath.Join(a.paths.Home, p)
		rec := st.AppImage(p)
		act, f, err := appimage.Decide(path, d, rec)
		if err != nil {
			return err
		}
		if plan.AppImageOps[act] != s.Op {
			return fmt.Errorf("~/%s changed since the plan was shown; run the command again", p)
		}

		r := state.AppImage{SHA256: f.SHA256, SelfUpdate: d.SelfUpdate, Stamp: f.Stamp}
		switch act {
		case appimage.Record:
			if f.SHA256 == "" { // self_update turned on: the file wasn't read
				r.SHA256, r.Stamp = rec.SHA256, rec.Stamp
			}
		case appimage.Install, appimage.Update, appimage.Replace:
			var expect *appimage.File
			var aside func() error
			switch act {
			case appimage.Update:
				expect = &f
			case appimage.Replace:
				expect = &f
				aside = func() error {
					where, err := h.layer.SetAside(path)
					if err == nil {
						fmt.Fprintf(a.stdout, "Moved ~/%s to %s.\n", p, where)
					}
					return err
				}
			}
			fmt.Fprintf(a.stdout, "Downloading %s\n", d.URL)
			if r.Stamp, err = appimage.Fetch(a.ctx, a.httpClient(), d.URL, d.SHA256, path, expect, aside); err != nil {
				return err
			}
			r.SHA256 = d.SHA256
		}
		st.SetAppImage(s.Module, p, r)
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// doctorAppImages reports the declared AppImages that are missing, and the
// pinned ones (no self_update) that don't match their pin. Getting them
// back is a download, which is apply's job, so -fix leaves them; it only
// removes the temp files an interrupted download left. The files an
// interrupted update left are also reported for AppImages that are off but
// recorded or declared by a module: turning one off doesn't remove them.
func (a *app) doctorAppImages(h *half, st *state.State, in plan.Input, fix bool, r *report) {
	found := false
	audited := map[string]bool{}
	for _, name := range in.Desired {
		m := in.Modules[name]
		for _, p := range m.AppImagePaths() {
			_, d, _ := m.AppImageAt(p)
			path := filepath.Join(a.paths.Home, p)
			audited[p] = true
			if h.layer.OwnData(path) {
				continue // reported by the planner
			}
			if dir := h.layer.SymlinkedParent(path); dir != "" {
				target, _ := os.Readlink(dir)
				r.problem("AppImage ~/%s of %s: parent %s is a symlink (to %s); remove it first", p, name, dir, target)
				continue
			}
			a.doctorLeftovers(path, fix, r)
			info, err := os.Lstat(path)
			switch {
			case errors.Is(err, iofs.ErrNotExist):
				r.problem("AppImage ~/%s of %s is missing", p, name)
				found = true
				continue
			case err != nil:
				r.problem("AppImage ~/%s of %s: %v", p, name, err)
				continue
			case !info.Mode().IsRegular():
				r.problem("AppImage ~/%s of %s isn't a regular file", p, name)
				continue
			case d.SelfUpdate:
				continue
			}
			f, err := appimage.Sum(path, st.AppImage(p))
			if err != nil {
				r.problem("AppImage ~/%s of %s: %v", p, name, err)
				continue
			}
			if f.SHA256 != d.SHA256 {
				r.problem("AppImage ~/%s of %s doesn't match its pinned sha256 (it has %s)", p, name, f.SHA256)
				found = true
			}
		}
	}
	if found {
		fmt.Fprintln(r.w, "    → 'arcus apply' downloads the pinned version; a changed file is moved aside to <path>.arcus-foreign first")
	}

	var others []string
	for _, paths := range st.Owned[state.HomeAppImages] {
		others = append(others, paths...)
	}
	for _, m := range in.Modules {
		others = append(others, m.AppImagePaths()...)
	}
	slices.Sort(others)
	for _, p := range slices.Compact(others) {
		path := filepath.Join(a.paths.Home, p)
		if !audited[p] && !h.layer.OwnData(path) && h.layer.SymlinkedParent(path) == "" {
			a.doctorSwaps(path, r)
		}
	}
}

// doctorLeftovers reports the temp files an interrupted download of path
// left, and removes them with fix: no arcus is downloading while doctor
// holds the lock. It also reports the files an interrupted update left
// mid-swap (doctorSwaps). The caller has checked that no directory above
// path is a symlink, whose target isn't arcus's to clean.
func (a *app) doctorLeftovers(path string, fix bool, r *report) {
	if !a.doctorSwaps(path, r) {
		return
	}
	leftovers, err := appimage.Leftovers(path)
	if err != nil {
		r.problem("%v", err)
		return
	}
	for _, l := range leftovers {
		r.fixable("%s was left by an interrupted download", l)
		if !fix || !r.repairing() {
			continue
		}
		if err := os.Remove(l); err != nil {
			r.failed(err)
		} else {
			r.repaired("removed it")
		}
	}
}

// doctorSwaps reports the files an interrupted update of path left
// mid-swap, which only the user can tell apart, so -fix leaves them. It
// returns false if the directory couldn't be listed.
func (a *app) doctorSwaps(path string, r *report) bool {
	swaps, err := appimage.Swaps(path)
	if err != nil {
		r.problem("%v", err)
		return false
	}
	for _, s := range swaps {
		r.problem("%s was left by an interrupted update: it is the new version, or the old file with any change made during the download; inspect it, then remove it", s)
	}
	return true
}
