// Package appimage inspects and downloads the vendor AppImages that modules
// declare in $HOME. arcus acts on such a file only while it is byte for byte
// what arcus recorded installing; once it changed, through the app updating
// itself or the user, it is theirs.
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

	"github.com/grunclepug/arcus/internal/atomicfile"
	"github.com/grunclepug/arcus/internal/module"
)

// Action is what brings the file at an AppImage's path in line with its pin.
type Action int

const (
	Keep      Action = iota // the recorded pin is there: nothing to do
	Install                 // nothing there: download the pin
	Update                  // the file arcus recorded, but the pin moved: download over it
	Adopt                   // the pinned file, unrecorded or not executable: record it, make it executable
	Unchanged               // recorded, but it changed since: it is the app's or the user's now
)

// Client downloads AppImages. Tests swap it for their server's client.
var Client = http.DefaultClient

// Decide returns what brings the file at path in line with pin, given rec,
// the sha256 arcus recorded for it ("" for none). It refuses, with an error,
// a leftover temp file, anything but a regular file, and an unrecorded file
// that isn't the pinned one. It reads the file only where it has to: never
// for the recorded pin that is executable. A file it can't read counts as
// not matching. The caller checks for a symlinked parent first.
func Decide(path, pin, rec string) (Action, error) {
	if err := CheckTemp(path); err != nil {
		return Keep, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, iofs.ErrNotExist) {
		return Install, nil
	}
	if err != nil {
		return Keep, err
	}
	if !info.Mode().IsRegular() {
		return Keep, fmt.Errorf("%s is there and isn't a regular file: move it away, then apply again", path)
	}
	executable := info.Mode()&0o100 != 0
	if rec != "" && rec == pin && executable {
		return Keep, nil
	}
	sum, _ := Sum(path) // "" when unreadable, which matches nothing
	switch {
	case sum == pin:
		return Adopt, nil
	case rec == "":
		return Keep, fmt.Errorf("%s is there and isn't the pinned AppImage: move it away, then apply again", path)
	case sum == rec:
		return Update, nil
	}
	return Unchanged, nil
}

// CheckTemp refuses a temp file an interrupted download left beside path.
// arcus never removes one it didn't create in the same run.
func CheckTemp(path string) error {
	tmp := path + module.TempSuffix
	if _, err := os.Lstat(tmp); err == nil {
		return fmt.Errorf("%s was left by an interrupted download: remove it, then apply again", tmp)
	}
	return nil
}

// Sum returns the SHA-256 of the regular file at path.
func Sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Removable reports whether path is a regular file with the content rec,
// the file arcus recorded. Any error makes it false: arcus can't prove the
// file is its own. The caller checks for a symlinked parent.
func Removable(path, rec string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	sum, err := Sum(path)
	return err == nil && sum == rec
}

// Fetch downloads url into <path>.arcus-tmp, checks it against pin, makes
// it executable and puts it at path. With old set (an update), the file at
// path must still be Removable as old, and the download is renamed over
// it; without, it is linked into place, which never replaces a file that
// appeared meanwhile. placed reports that the new file is at path, which
// holds even if a later step (the directory sync) failed. Short of that,
// nothing is left of the download; directories created for it may remain.
func Fetch(ctx context.Context, url, pin, path, old string) (placed bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	tmpPath := path + module.TempSuffix
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, iofs.ErrExist) {
		return false, fmt.Errorf("%s was left by an interrupted download: remove it, then apply again", tmpPath)
	}
	if err != nil {
		return false, err
	}
	// Only a temp file this run created is removed.
	defer func() {
		tmp.Close()
		if !placed || old == "" {
			if rmErr := os.Remove(tmpPath); rmErr != nil && !errors.Is(rmErr, iofs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()

	got, err := download(ctx, url, tmp)
	if err != nil {
		return false, fmt.Errorf("downloading %s: %w; nothing was installed", url, err)
	}
	if got != pin {
		return false, fmt.Errorf("the download of %s doesn't match the pinned sha256: expected %s, got %s; nothing was installed", url, pin, got)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if old != "" {
		if !Removable(path, old) {
			return false, fmt.Errorf("%s changed while the new version was downloaded; it was left as it is", path)
		}
		if err := os.Rename(tmpPath, path); err != nil {
			return false, err
		}
		return true, atomicfile.SyncDir(dir)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, iofs.ErrExist) {
			return false, fmt.Errorf("%s appeared while it was downloaded; it was left as it is and nothing was installed", path)
		}
		return false, err
	}
	// The deferred removal drops the temp name; the sync after it covers both.
	if err := os.Remove(tmpPath); err != nil {
		return true, err
	}
	return true, atomicfile.SyncDir(dir)
}

// download writes the body of url to w and returns its SHA-256.
func download(ctx context.Context, url string, w io.Writer) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
