// Package appimage inspects and downloads the vendor AppImages that modules
// declare in $HOME.
package appimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/grunclepug/arcus/internal/atomicfile"
	"github.com/grunclepug/arcus/internal/module"
	"github.com/grunclepug/arcus/internal/state"
)

// Action is what brings the file at an AppImage's path in line with its
// declaration.
type Action int

const (
	Keep    Action = iota // nothing to do
	Install               // nothing there: download it
	Update                // the file arcus installed, unchanged, but the pin moved: download over it
	Replace               // neither the pinned file nor arcus's unchanged one: set it aside, then download
	Adopt                 // not recorded, and the pinned file or (self_update) any file: record it as it is
	Record                // recorded and fine as it is, but the record is out of date
)

// File is what Decide or Sum found at a path.
type File struct {
	SHA256 string // "" when the file wasn't read
	Stamp  state.Stamp
}

// Decide returns what brings the file at path in line with d. rec is
// arcus's record of the path, nil if it has none. A self-updating AppImage
// that arcus recorded is never read: the app owns its content. Anything
// but a regular file at path is an error; arcus doesn't replace it.
func Decide(path string, d module.AppImage, rec *state.AppImage) (Action, File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return Install, File{}, nil
	}
	if err != nil {
		return Keep, File{}, err
	}
	if !info.Mode().IsRegular() {
		return Keep, File{}, fmt.Errorf("%s is there and isn't a regular file: move it away, then apply again", path)
	}
	if rec != nil && d.SelfUpdate {
		if rec.SelfUpdate {
			return Keep, File{}, nil
		}
		return Record, File{}, nil
	}
	f, err := Sum(path, rec)
	if err != nil {
		return Keep, File{}, err
	}
	switch {
	case f.SHA256 == d.SHA256 && rec == nil:
		return Adopt, f, nil
	case f.SHA256 == d.SHA256 && (rec.SHA256 != d.SHA256 || rec.SelfUpdate || rec.Stamp != f.Stamp):
		// A new stamp (touch, chmod, a restore) is recorded too, or every
		// plan would read the file again.
		return Record, f, nil
	case f.SHA256 == d.SHA256:
		return Keep, f, nil
	case rec == nil && d.SelfUpdate:
		return Adopt, f, nil
	case rec != nil && f.SHA256 == rec.SHA256:
		return Update, f, nil
	}
	return Replace, f, nil
}

// Sum returns the SHA-256 and stamp of the regular file at path. While the
// file still has rec's stamp it has rec's content, which is returned
// without reading it.
func Sum(path string, rec *state.AppImage) (File, error) {
	// Non-blocking, so a FIFO put there since the caller looked fails below
	// instead of hanging the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s isn't a regular file", path)
	}
	stamp := stampOf(info)
	if rec != nil && rec.SHA256 != "" && rec.Stamp == stamp {
		return File{SHA256: rec.SHA256, Stamp: stamp}, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return File{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if after, err := f.Stat(); err != nil || stampOf(after) != stamp {
		return File{}, fmt.Errorf("%s changed while it was read; try again", path)
	}
	return File{SHA256: hex.EncodeToString(h.Sum(nil)), Stamp: stamp}, nil
}

// Removable reports, for every AppImage st records, whether turning it off
// may remove its file: it is still a regular file and either self-updating
// or unchanged since arcus recorded it. A file below a directory that is a
// symlink (symlinkedParent returns it) is not: removing it would follow the
// link, into a dotfiles checkout say. Paths are relative to home.
func Removable(home string, st *state.State, symlinkedParent func(string) string) (map[string]bool, error) {
	removable := map[string]bool{}
	for _, paths := range st.Owned[state.HomeAppImages] {
		for _, p := range paths {
			rec := st.AppImage(p)
			path := filepath.Join(home, p)
			if rec == nil || symlinkedParent(path) != "" {
				continue
			}
			info, err := os.Lstat(path)
			if errors.Is(err, iofs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if rec.SelfUpdate {
				removable[p] = true
				continue
			}
			f, err := Sum(path, rec)
			if err != nil {
				return nil, err
			}
			removable[p] = f.SHA256 == rec.SHA256
		}
	}
	return removable, nil
}

// HTTPSOnly is an http.Client CheckRedirect that refuses to follow a
// redirect away from https, and more than 10 redirects, as the default does.
func HTTPSOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %s is not https", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// Client is the HTTP client for AppImage downloads.
var Client = &http.Client{CheckRedirect: HTTPSOnly}

// StallTimeout is how long a download may go without receiving anything.
var StallTimeout = time.Minute

var errStalled = errors.New("stalled")

// Fetch downloads url into a temp file next to path and checks it against
// want before anything else happens. Then it makes the file executable and
// fsyncs it, and puts it at path:
//   - when updating or moving a file aside, first check that it still has
//     the expected stamp, and when updating, check the old file's content
//     too once it is out of the way;
//   - when installing, or after aside moved the old file away, put it in
//     place only if the path is still empty.
//
// On any failure before that the temp file and the directories Fetch
// created are removed, and path is as it was. Temp files an earlier run
// left (Leftovers) are removed first, never the files an interrupted swap
// left (Swaps). It returns the stamp of the installed file. The download is
// never run.
func Fetch(ctx context.Context, client *http.Client, url, want, path string, expect *File, aside func() error) (stamp state.Stamp, err error) {
	dir := filepath.Dir(path)
	leftovers, err := Leftovers(path)
	if err != nil {
		return stamp, err
	}
	for _, l := range leftovers {
		if err := os.Remove(l); err != nil {
			return stamp, err
		}
	}
	created := missingDirs(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return stamp, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		removeDirs(created)
		return stamp, err
	}
	placed := false
	defer func() {
		tmp.Close()
		if !placed {
			_ = os.Remove(tmp.Name())
			removeDirs(created)
		}
	}()

	got, err := download(ctx, client, url, tmp)
	if err != nil {
		return stamp, fmt.Errorf("downloading %s: %w; nothing was installed", url, err)
	}
	if got != want {
		return stamp, fmt.Errorf("the download of %s doesn't match the pinned sha256: expected %s, got %s; nothing was installed", url, want, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return stamp, err
	}
	if err := tmp.Sync(); err != nil {
		return stamp, err
	}
	for _, d := range created {
		if err := atomicfile.SyncDir(filepath.Dir(d)); err != nil {
			return stamp, err
		}
	}
	if err := place(tmp.Name(), path, expect, aside); err != nil {
		return stamp, err
	}
	placed = true
	// The temp name is only a second link now; one left by a failure here
	// is a leftover the next Fetch or doctor -fix removes.
	_ = os.Remove(tmp.Name())
	if err := atomicfile.SyncDir(dir); err != nil {
		return stamp, err
	}
	info, err := tmp.Stat() // after the rename or unlink, which change ctime
	if err != nil {
		return stamp, err
	}
	return stampOf(info), nil
}

// beforeReplace runs right before place swaps a new version in; tests use
// it to change the old file at the last moment.
var beforeReplace = func() {}

// swapIn swaps the new version in for the old file, and swapBack puts a
// changed old file back; tests make them fail.
var swapIn, swapBack = exchange, exchange

// place puts the verified temp file at path, as Fetch describes.
func place(tmp, path string, expect *File, aside func() error) error {
	if expect != nil {
		info, err := os.Lstat(path)
		if err != nil || stampOf(info) != expect.Stamp {
			return fmt.Errorf("%s changed while the new version was downloaded; it was left as it is", path)
		}
		if aside == nil {
			return replace(tmp, path, *expect)
		}
	}
	if aside != nil {
		if err := aside(); err != nil {
			return err
		}
	}
	// A hard link, unlike a rename, fails if anything is at path.
	err := os.Link(tmp, path)
	if errors.Is(err, iofs.ErrExist) {
		return fmt.Errorf("%s appeared while it was downloaded; it was left as it is and nothing was installed", path)
	}
	return err
}

// replace swaps the new version at tmp in for the old file at path with
// renameat2's RENAME_EXCHANGE, then checks the old file against expect, its
// content included: an edit can keep both size and mtime. A change that
// landed after the last check is swapped back rather than lost.
// Where the kernel or filesystem can't exchange files nothing is replaced,
// since a plain rename would overwrite such a change.
//
// The new version is swapped in from a .swap name (Swaps), where the old
// file then sits until it is checked. Nothing removes .swap files, so a
// crash there leaves the old file, maybe with a late change, for the user
// to inspect. The rename gives the old file a new ctime, so of its stamp
// only size, mtime and inode are compared.
func replace(tmp, path string, expect File) error {
	swap := strings.TrimSuffix(tmp, ".tmp") + ".swap"
	if err := os.Link(tmp, swap); err != nil { // a link never replaces what is there
		return err
	}
	beforeReplace()
	err := swapIn(swap, path)
	if err != nil {
		_ = os.Remove(swap) // the new version, never swapped in
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) {
			return fmt.Errorf("%s wasn't updated: this kernel or filesystem can't swap two files atomically (renameat2 RENAME_EXCHANGE), so a change made during the download could be lost; it was left as it is", path)
		}
		return err
	}
	info, err := os.Lstat(swap)
	if err == nil && info.Size() == expect.Stamp.Size && info.ModTime().UnixNano() == expect.Stamp.MTime && stampOf(info).Inode == expect.Stamp.Inode && sumIs(swap, expect.SHA256) {
		// The old file as it was decided. One a failed removal leaves stays, as
		// any .swap file does.
		_ = os.Remove(swap)
		return nil
	}
	if err := swapBack(swap, path); err != nil {
		return fmt.Errorf("%s changed while the new version was downloaded, and putting it back failed: %v; the changed file is at %s", path, err, swap)
	}
	_ = os.Remove(swap) // the new version again
	return fmt.Errorf("%s changed while the new version was downloaded; it was left as it is", path)
}

// sumIs reports whether the file at path has the SHA-256 sum.
func sumIs(path, sum string) bool {
	f, err := Sum(path, nil)
	return err == nil && f.SHA256 == sum
}

// Leftovers returns the temp files that an interrupted Fetch of path left
// next to it: .<name>.<digits>.tmp, downloads that were never put in place.
func Leftovers(path string) ([]string, error) { return named(path, ".tmp") }

// Swaps returns the files an update of path interrupted during its swap
// left next to it: .<name>.<digits>.swap. Each holds the new version or the
// old file, which may have changed during the download, so arcus never
// removes them.
func Swaps(path string) ([]string, error) { return named(path, ".swap") }

// named returns the regular files .<name>.<digits><suffix> next to path.
// There are none where path's directory is missing or a file. module's
// tempNameRegex refuses a declared path of this form; the two must agree.
func named(path, suffix string) ([]string, error) {
	dir, base := filepath.Split(path)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []string
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), "."+base+".")
		random, match := strings.CutSuffix(rest, suffix)
		if ok && match && random != "" && strings.Trim(random, "0123456789") == "" && e.Type().IsRegular() {
			found = append(found, filepath.Join(dir, e.Name()))
		}
	}
	return found, nil
}

// download writes the body of url to w and returns its SHA-256. It gives
// up when ctx ends or nothing arrives for StallTimeout.
func download(ctx context.Context, client *http.Client, url string, w io.Writer) (string, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall := time.AfterFunc(StallTimeout, func() { cancel(errStalled) })
	defer stall.Stop()
	fail := func(err error) error {
		if errors.Is(context.Cause(ctx), errStalled) {
			return fmt.Errorf("nothing arrived for %v", StallTimeout)
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	h := sha256.New()
	body := readFunc(func(p []byte) (int, error) {
		n, err := resp.Body.Read(p)
		if n > 0 {
			stall.Reset(StallTimeout)
		}
		return n, err
	})
	if _, err := io.Copy(io.MultiWriter(w, h), body); err != nil {
		return "", fail(err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// missingDirs returns dir and its ancestors that don't exist, outermost first.
func missingDirs(dir string) []string {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil || d == filepath.Dir(d) {
			break
		}
		missing = append(missing, d)
	}
	slices.Reverse(missing)
	return missing
}

// removeDirs removes the directories created, innermost first, as long as
// they are empty.
func removeDirs(created []string) {
	for _, d := range slices.Backward(created) {
		if syscall.Rmdir(d) != nil { // never a file that took a directory's place
			return
		}
	}
}

func stampOf(info os.FileInfo) state.Stamp {
	s := state.Stamp{Size: info.Size(), MTime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		s.CTime, s.Inode = st.Ctim.Nano(), st.Ino
	}
	return s
}
