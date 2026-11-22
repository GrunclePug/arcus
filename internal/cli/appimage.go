package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grunclepug/arcus/internal/appimage"
	"github.com/grunclepug/arcus/internal/plan"
	"github.com/grunclepug/arcus/internal/state"
)

// appImage carries out an AppImage step of the user half: a download or a
// take-over. It refuses if the file or its record no longer call for what
// the plan showed.
func (a *app) appImage(h *half, st *state.State, s plan.Step, in plan.Input, save func() error) error {
	m := in.Modules[s.Module]
	if m == nil {
		return fmt.Errorf("module %q does not exist", s.Module)
	}
	for _, p := range s.Names {
		d, ok := m.AppImageAt(p)
		if !ok {
			return fmt.Errorf("module %s declares no AppImage at ~/%s", s.Module, p)
		}
		path := filepath.Join(a.paths.Home, p)
		// Checked at plan time too; the file system may have changed since.
		if dir := h.layer.SymlinkedParent(path); dir != "" {
			target, _ := os.Readlink(dir)
			return fmt.Errorf("%s: parent %s is a symlink (to %s); remove it first", path, dir, target)
		}
		act, err := appimage.Decide(path, d.SHA256, st.AppImages[p])
		if err != nil {
			return err
		}
		planned := s.Op == plan.InstallAppImage && (act == appimage.Install || act == appimage.Update) ||
			s.Op == plan.Adopt && act == appimage.Adopt
		if !planned {
			return fmt.Errorf("~/%s changed since the plan was shown; run the command again", p)
		}

		if act == appimage.Adopt {
			if err := makeExecutable(path); err != nil {
				return err
			}
		} else {
			old := ""
			if act == appimage.Update {
				old = st.AppImages[p]
			}
			fmt.Fprintf(a.stdout, "Downloading %s\n", d.URL)
			placed, err := appimage.Fetch(a.ctx, d.URL, d.SHA256, path, old)
			if !placed {
				return err
			}
			if err != nil { // placed, but not yet synced: record it anyway
				st.SetAppImage(s.Module, p, d.SHA256)
				return errors.Join(err, save())
			}
		}
		st.SetAppImage(s.Module, p, d.SHA256)
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

// makeExecutable adds the owner's executable bit to path, unless it is set
// (a pinned file someone else owns may be adopted but not changed). The
// other bits stay: a private file stays private.
func makeExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0o100 != 0 {
		return nil
	}
	return os.Chmod(path, info.Mode()|0o100)
}
