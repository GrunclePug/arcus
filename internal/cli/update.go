package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grunclepug/arcus/internal/atomicfile"
)

// Releases is where arcus looks up its latest release.
const Releases = "https://api.github.com/repos/grunclepug/arcus/releases/latest"

// release is the part of GitHub's release JSON that update reads.
type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r *release) version() string { return strings.TrimPrefix(r.Tag, "v") }

func (r *release) asset(name string) (string, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no %s", r.Tag, name)
}

// latest asks GitHub for the latest release.
func (a *app) latest(ctx context.Context) (*release, error) {
	data, err := fetch(ctx, a.releases)
	if err != nil {
		return nil, err
	}
	var r release
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("reading the latest release: %w", err)
	}
	if _, ok := semver(r.version()); !ok {
		return nil, fmt.Errorf("the latest release has an unexpected tag %q", r.Tag)
	}
	return &r, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// semver parses MAJOR.MINOR.PATCH.
func semver(v string) ([3]int, bool) {
	var n [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// newer reports whether version v is newer than the running one. A build
// without a release version (dev) is never older.
func (a *app) newer(v string) bool {
	have, ok := semver(a.version)
	want, _ := semver(v)
	if !ok {
		return false
	}
	for i := range have {
		if want[i] != have[i] {
			return want[i] > have[i]
		}
	}
	return false
}

// cmdUpdate replaces the running binary with the latest release, verified
// against the release's sha256sums.txt. A build from a clone is updated
// from the clone instead; update only knows releases.
func (a *app) cmdUpdate(args []string) error {
	var o opts
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.BoolVar(&o.yes, "y", false, "don't ask for confirmation")
	fs.BoolVar(&o.dry, "n", false, "only show what would change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("update takes no arguments")
	}
	if _, ok := semver(a.version); !ok {
		return fmt.Errorf("this arcus is version %q, not a release; update it from its clone", a.version)
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	r, err := a.latest(ctx)
	if err != nil {
		return err
	}
	if !a.newer(r.version()) {
		fmt.Fprintf(a.stdout, "arcus %s is up to date.\n", a.version)
		return nil
	}
	exe, err := a.exe()
	if err != nil {
		return fmt.Errorf("locating the arcus binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Update %s from %s to %s.\n", exe, a.version, r.version())
	if o.dry {
		return nil
	}
	if !o.yes && !a.confirm("Proceed?") {
		return errors.New("aborted")
	}

	sums, err := a.download(ctx, r, "sha256sums.txt")
	if err != nil {
		return err
	}
	want, err := checksum(sums, "arcus")
	if err != nil {
		return err
	}
	bin, err := a.download(ctx, r, "arcus")
	if err != nil {
		return err
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("the downloaded arcus doesn't match sha256sums.txt of %s; nothing was changed", r.Tag)
	}

	if syscall.Access(filepath.Dir(exe), accessW) == nil {
		if err := atomicfile.Write(exe, bin, 0o755); err != nil {
			return err
		}
	} else {
		dir, err := os.MkdirTemp("", "arcus-update-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		file := filepath.Join(dir, "arcus")
		if err := os.WriteFile(file, bin, 0o644); err != nil {
			return err
		}
		if err := a.sudo([]string{"-update", "-hash", want, file}); err != nil {
			return err
		}
	}
	fmt.Fprintf(a.stdout, "Updated to arcus %s.\n", r.version())
	return nil
}

const accessW = 0x2 // access(2) W_OK

func (a *app) download(ctx context.Context, r *release, name string) ([]byte, error) {
	url, err := r.asset(name)
	if err != nil {
		return nil, err
	}
	data, err := fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", name, err)
	}
	return data, nil
}

// checksum finds name's SHA-256 in sha256sum output.
func checksum(sums []byte, name string) (string, error) {
	s := bufio.NewScanner(bytes.NewReader(sums))
	for s.Scan() {
		if f := strings.Fields(s.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("sha256sums.txt lists no %s", name)
}

// installUpdate is exec-system -update: the root side replaces its own
// binary with file, only if file still has the hash the user's arcus
// verified.
func (a *app) installUpdate(hash string, args []string) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		return errors.New("exec-system -update: want -hash <sha256> <absolute file>")
	}
	bin, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if got := sha256.Sum256(bin); hex.EncodeToString(got[:]) != hash {
		return errors.New("the downloaded arcus changed after it was verified; nothing was changed")
	}
	exe, err := a.exe()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	return atomicfile.Write(exe, bin, 0o755)
}

// doctorVersion notes a newer release. It says nothing when GitHub can't be
// reached or arcus isn't a release build.
func (a *app) doctorVersion(r *report) {
	if a.releases == "" {
		return
	}
	if _, ok := semver(a.version); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	defer cancel()
	rel, err := a.latest(ctx)
	if err != nil || !a.newer(rel.version()) {
		return
	}
	fmt.Fprintln(r.w, "Version:")
	fmt.Fprintf(r.w, "  · arcus %s is out; this is %s\n", rel.version(), a.version)
	fmt.Fprintln(r.w, "    → 'arcus update', or in a clone: git pull && make build && sudo make install")
}
