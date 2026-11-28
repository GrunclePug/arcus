// Package system runs the tools arcus changes the system with: pacman, the
// AUR helper, makepkg, systemctl and gpasswd. Names passed in are validated
// by the module loader and never start with "-", so they can't be read as
// options.
package system

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Runner runs commands with the terminal attached, so password, provider
// and confirmation prompts work. Commands are deliberately not tied to a
// context: a child shares arcus's process group and gets a terminal Ctrl-C
// itself, and arcus waits for it rather than killing a package transaction
// halfway.
type Runner struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// NoConfirm passes --noconfirm to pacman and the AUR helper (arcus -y).
	NoConfirm bool
}

func (r *Runner) run(name string, args ...string) error {
	return r.runIn("", nil, name, args...)
}

// runIn runs a command in dir with extra environment variables.
func (r *Runner) runIn(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdin, r.Stdout, r.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (r *Runner) pkgArgs(op string, names []string) []string {
	args := []string{op}
	if op == "-S" {
		args = append(args, "--needed")
	}
	if r.NoConfirm {
		args = append(args, "--noconfirm")
	}
	return append(args, names...)
}

// PacmanInstall installs packages from the repositories.
func (r *Runner) PacmanInstall(pkgs []string) error {
	return r.run("pacman", r.pkgArgs("-S", pkgs)...)
}

// PacmanRemove removes packages and the dependencies nothing else needs.
func (r *Runner) PacmanRemove(pkgs []string) error {
	return r.run("pacman", r.pkgArgs("-Rns", pkgs)...)
}

// PacmanMark sets the install reason of packages: explicitly installed, or
// installed as a dependency.
func (r *Runner) PacmanMark(explicit bool, pkgs []string) error {
	reason := "--asdeps"
	if explicit {
		reason = "--asexplicit"
	}
	return r.run("pacman", append([]string{"-D", reason}, pkgs...)...)
}

// PacmanInstallFiles installs package files built by makepkg. Unlike -S it
// has no --needed: a rebuilt package of the same version is reinstalled.
// They are explicitly installed even if they replace a package that was
// installed as a dependency.
func (r *Runner) PacmanInstallFiles(files []string) error {
	return r.run("pacman", r.pkgArgs("-U", append([]string{"--asexplicit"}, files...))...)
}

// PacmanRemoveConflicting removes installed packages that packages about
// to be installed conflict with. Dependency checks are skipped: what needs
// a conflicting package is usually satisfied by its replacement, which is
// installed next, in the same root stage.
func (r *Runner) PacmanRemoveConflicting(pkgs []string) error {
	return r.run("pacman", r.pkgArgs("-Rdd", pkgs)...)
}

// Makepkg builds the PKGBUILD in dir, as the user, writing the package to
// pkgdest and downloads to srcdest. Missing dependencies are installed
// through sudo and removed again after the build.
func (r *Runner) Makepkg(dir, pkgdest, srcdest string) error {
	args := []string{"--syncdeps", "--rmdeps", "--force", "--clean"}
	if r.NoConfirm {
		args = append(args, "--noconfirm")
	}
	return r.runIn(dir, []string{"PKGDEST=" + pkgdest, "SRCDEST=" + srcdest}, "makepkg", args...)
}

// AURInstall installs packages with the AUR helper, as the user, and
// removes the packages it installed only to build them.
func (r *Runner) AURInstall(helper string, pkgs []string) error {
	return r.run(helper, append([]string{"--removemake"}, r.pkgArgs("-S", pkgs)...)...)
}

// AURRemove removes packages with the AUR helper, as the user.
func (r *Runner) AURRemove(helper string, pkgs []string) error {
	return r.run(helper, r.pkgArgs("-Rns", pkgs)...)
}

// AURMarkDeps marks packages as installed as a dependency, through the AUR
// helper, which asks sudo for pacman itself.
func (r *Runner) AURMarkDeps(helper string, pkgs []string) error {
	return r.run(helper, append([]string{"-D", "--asdeps"}, pkgs...)...)
}

// userScope is how arcus addresses user units: through the unit files
// (--root=/), which needs no running user manager. The installer's chroot
// and shells outside a login session have none. Queries read the unit
// directories directly, for the same reason.
var userScope = []string{"--user", "--root=/"}

// userManager reports whether the user's systemd instance is reachable, so
// user units can be started and stopped, not only enabled and disabled.
func userManager() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}

// Enable enables units; with now it also starts them. User units start only
// when the user's manager is reachable; otherwise they start at the next
// login.
func (r *Runner) Enable(user bool, units []string, now bool) error {
	if !user {
		args := []string{"enable"}
		if now {
			args = append(args, "--now")
		}
		return r.run("systemctl", append(args, units...)...)
	}
	if err := r.run("systemctl", append(slices.Concat(userScope, []string{"enable"}), units...)...); err != nil {
		return err
	}
	if now && userManager() {
		return r.run("systemctl", append([]string{"--user", "start"}, units...)...)
	}
	return nil
}

// Disable disables units; with now it also stops them.
func (r *Runner) Disable(user bool, units []string, now bool) error {
	if !user {
		args := []string{"disable"}
		if now {
			args = append(args, "--now")
		}
		return r.run("systemctl", append(args, units...)...)
	}
	if now && userManager() {
		if err := r.run("systemctl", append([]string{"--user", "stop"}, units...)...); err != nil {
			return err
		}
	}
	return r.run("systemctl", append(slices.Concat(userScope, []string{"disable"}), units...)...)
}

// JoinGroup adds user to a supplementary group.
func (r *Runner) JoinGroup(user, group string) error {
	return r.run("gpasswd", "-a", user, group)
}

// LeaveGroup removes user from a supplementary group.
func (r *Runner) LeaveGroup(user, group string) error {
	return r.run("gpasswd", "-d", user, group)
}

// output runs a read-only command and returns what it prints. pacman and
// systemctl exit 1 when nothing matches, which is no output, not an error.
func output(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return "", nil
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// query runs a read-only command and returns the whitespace-separated
// fields of its output.
func query(name string, args ...string) (map[string]bool, error) {
	out, err := output(name, args...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for line := range strings.Lines(out) {
		for _, f := range strings.Fields(line) {
			set[f] = true
		}
	}
	return set, nil
}

// Installed returns every installed package, from the repositories or the AUR.
func Installed() (map[string]bool, error) {
	return query("pacman", "-Qq")
}

// Explicit returns the packages installed explicitly, not as a dependency.
func Explicit() (map[string]bool, error) {
	return query("pacman", "-Qqe")
}

// Foreign returns the installed packages that no sync repository has:
// from the AUR or built by hand. pacman decides that from its sync
// databases, and when one is missing, corrupt or unusable it still lists
// packages, wrongly, and only says so on stderr, whatever its exit status.
// So any error there, or the warning that a database is missing, fails
// this; other warnings, such as an outdated pacman.conf directive, don't.
// Exit 1 without output is no foreign package. pacman runs untranslated so
// its messages can be recognised.
func Foreign() (map[string]bool, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pacman", "-Qqm")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	for line := range strings.Lines(stderr.String()) {
		if !strings.HasPrefix(line, "warning:") || strings.Contains(line, "database file for") {
			return nil, fmt.Errorf("pacman -Qqm: %s", strings.TrimSpace(line))
		}
	}
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0) {
		return nil, fmt.Errorf("pacman -Qqm: %w", err)
	}
	set := map[string]bool{}
	for _, f := range strings.Fields(stdout.String()) {
		set[f] = true
	}
	return set, nil
}

// Unrequired returns the installed packages that no installed package
// depends on.
func Unrequired() (map[string]bool, error) {
	return query("pacman", "-Qqt")
}

// Package is what pacman reports about a package, as far as arcus needs
// it. Provides, Conflicts, Depends and BuildDepends keep their version
// constraints ("foo=1.2", "bar<2").
type Package struct {
	Provides, Conflicts, RequiredBy []string
	Depends                         []string // what it needs installed
	// BuildDepends, only from a PKGBUILD, is what makepkg --syncdeps
	// needs installed to build it: see SrcInfo.
	BuildDepends []string
}

// Info returns what pacman reports about packages, by name: installed ones
// with -Qi (all of them when no names are given), ones in the repositories
// with -Si, and package files with -Qip.
func Info(op string, names ...string) (map[string]Package, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pacman", append([]string{op}, names...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // the field names, untranslated
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pacman %s: %w: %s", op, err, strings.TrimSpace(stderr.String()))
	}
	return parseInfo(stdout.String()), nil
}

// RequiredBy returns, for each of the installed packages named, the
// installed packages that depend on it, as pacman -Qi lists them.
func RequiredBy(names []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(names) == 0 {
		return out, nil
	}
	info, err := Info("-Qi", names...)
	if err != nil {
		return nil, err
	}
	for name, p := range info {
		out[name] = p.RequiredBy
	}
	return out, nil
}

// parseInfo reads the Name, Provides, Conflicts With and Required By fields
// of pacman -Qi or -Si output. A long value may go on over indented lines.
func parseInfo(info string) map[string]Package {
	out := map[string]Package{}
	var name, field string
	for line := range strings.Lines(info) {
		value := line
		if line != "" && line[0] != ' ' && line[0] != '\t' {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				field = ""
				continue
			}
			field, value = strings.TrimSpace(k), v
		}
		if field == "Name" {
			name = strings.TrimSpace(value)
			out[name] = Package{}
			continue
		}
		p := out[name]
		for _, f := range strings.Fields(value) {
			if f == "None" {
				continue
			}
			switch field {
			case "Provides":
				p.Provides = append(p.Provides, f)
			case "Conflicts With":
				p.Conflicts = append(p.Conflicts, f)
			case "Depends On":
				p.Depends = append(p.Depends, f)
			case "Required By":
				p.RequiredBy = append(p.RequiredBy, f)
			}
		}
		out[name] = p
	}
	return out
}

// SrcInfo returns what the PKGBUILD in dir declares for pkg, from makepkg
// --printsrcinfo: what the package provides, conflicts with and depends on,
// and what makepkg --syncdeps needs installed to build it. It runs the
// PKGBUILD, so only as the user.
func SrcInfo(dir, pkg string) (Package, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("makepkg", "--printsrcinfo")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Package{}, fmt.Errorf("makepkg --printsrcinfo in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	return parseSrcInfo(stdout.String(), pkg, carch()), nil
}

// carch is the architecture makepkg builds for: $CARCH when it is set,
// which makepkg prefers to makepkg.conf, otherwise the machine's, which is
// what the stock /etc/makepkg.conf sets. A CARCH set only in a makepkg.conf
// file isn't seen; then the _<arch> lists of another architecture are read.
// Exporting CARCH makes arcus use it too.
func carch() string {
	if a := os.Getenv("CARCH"); a != "" {
		return a
	}
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return ""
	}
	var b []byte
	for _, c := range u.Machine { // int8 or uint8, by platform
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// parseSrcInfo reads what .SRCINFO text declares for pkg the way makepkg
// builds it. Provides, Conflicts and Depends: the list pkg's own section
// sets, otherwise the pkgbase list with its _<arch> variant, then pkg's own
// _<arch> variant (makepkg merges the pkgbase variants after sourcing the
// PKGBUILD and a package's own after its package function). BuildDepends:
// the pkgbase depends, makedepends and checkdepends with their variants,
// which makepkg --syncdeps installs; checkdepends always count, even when
// no check() runs. An emptied list prints as "depends = " and sets it to
// nothing.
//
// .SRCINFO can't tell everything makepkg does: "depends+=(x)" in a package
// function prints like an override, so the pkgbase depends_<arch> it keeps
// is dropped here; an arch "+=" next to a plain override can list a
// dependency the built package doesn't have; and for an arch=(any) recipe
// makepkg omits makedepends_<arch>, which the build still needs.
func parseSrcInfo(info, pkg, arch string) Package {
	base, own := map[string][]string{}, map[string][]string{}
	section := base
	for line := range strings.Lines(info) {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			if k, ok = strings.CutSuffix(line, " ="); !ok {
				continue
			}
			v = ""
		}
		switch k {
		case "pkgbase":
			section = base
			continue
		case "pkgname":
			section = nil
			if v == pkg {
				section = own
			}
			continue
		}
		if section == nil {
			continue
		}
		if _, set := section[k]; !set {
			section[k] = []string{}
		}
		if v != "" {
			section[k] = append(section[k], v)
		}
	}
	field := func(k string) []string {
		list, set := own[k]
		if !set {
			list = slices.Concat(base[k], base[k+"_"+arch])
		}
		return slices.Concat(list, own[k+"_"+arch])
	}
	build := func(k string) []string { return slices.Concat(base[k], base[k+"_"+arch]) }
	return Package{
		Provides:     field("provides"),
		Conflicts:    field("conflicts"),
		Depends:      field("depends"),
		BuildDepends: slices.Concat(build("depends"), build("makedepends"), build("checkdepends")),
	}
}

// EnabledUnits returns the concrete units enabled in dirs[0] (from UnitDirs):
// symlinks inside *.wants, *.requires and *.upholds directories, plus
// top-level non-template alias symlinks named in the target unit's [Install]
// Alias= section. Masks (/dev/null or 0-byte unit files) and dangling links
// whose unit file or template does not exist in dirs are skipped. Alias-only
// template instances are not supported. All symlink resolution is confined to
// root (empty defaults to "/").
func EnabledUnits(root string, dirs []string) (map[string]bool, error) {
	enabled := map[string]bool{}
	if len(dirs) == 0 {
		return enabled, nil
	}
	root = cleanRoot(root)
	dir0, fi, devNull, err := resolveInRoot(root, dirs[0])
	if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || devNull {
		return enabled, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return enabled, nil
	}
	entries, err := os.ReadDir(dir0)
	if err != nil {
		return nil, err
	}

	// Pass 1: *.wants, *.requires, *.upholds directories (stat-only, so a
	// root-only 0600 unit file in /etc/systemd/system never breaks non-root
	// callers).
	for _, e := range entries {
		if !e.IsDir() || !isDepDir(e.Name()) {
			continue
		}
		depDir := filepath.Join(dir0, e.Name())
		subs, err := os.ReadDir(depDir)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if sub.Type()&iofs.ModeSymlink == 0 {
				continue
			}
			name := sub.Name()
			if IsTemplate(name) {
				continue
			}
			_, _, depDevNull, rerr := resolveInRoot(root, filepath.Join(depDir, name))
			if depDevNull {
				continue
			}
			if rerr != nil && !isBrokenLink(rerr) {
				return nil, rerr
			}
			_, st, err := unitFile(root, dirs, name)
			if err != nil {
				return nil, err
			}
			if st == unitFound {
				enabled[name] = true
			}
		}
	}

	// Pass 2: top-level non-template alias symlinks in dirs[0]. Resolving the
	// first-hop parent directory inside root accepts merged-/usr targets
	// (/lib/systemd/system) and alias-only linked units
	// (alias.service -> /etc/systemd/system/real.service -> /opt/real.service)
	// while ignoring external linked unit fragments (/opt/ext/custom.conf or
	// home/-deployed units).
	dirSet := unitDirSet(root, dirs)
	dir0Rel := toRelInRoot(root, dir0)
	for _, e := range entries {
		if e.Type()&iofs.ModeSymlink == 0 {
			continue
		}
		linkName := e.Name()
		targetBase, inTree, ok, err := topLinkFirstHop(root, dir0Rel, filepath.Join(dir0, linkName), dirSet)
		if err != nil {
			return nil, err
		}
		if !ok || !inTree || targetBase == "" || targetBase == "." || targetBase == ".." || targetBase == "/" || targetBase == linkName || IsTemplate(targetBase) {
			continue
		}
		if enabled[targetBase] {
			continue
		}
		unitPath, st, err := unitFile(root, dirs, targetBase)
		if err != nil {
			return nil, err
		}
		if st != unitFound {
			continue
		}
		inst, err := readInstall(unitPath, "", false)
		if err != nil {
			if errors.Is(err, iofs.ErrPermission) {
				continue
			}
			return nil, err
		}
		if slices.Contains(inst.alias, linkName) {
			enabled[targetBase] = true
		}
	}
	return enabled, nil
}

func unitDirSet(root string, dirs []string) map[string]bool {
	dirSet := map[string]bool{}
	for _, d := range dirs {
		clean := filepath.Clean(d)
		dirSet[clean] = true
		if rd, rfi, rdev, rerr := resolveInRoot(root, d); rerr == nil && !rdev && rfi.IsDir() {
			dirSet[rd] = true
		}
	}
	return dirSet
}

// topLinkFirstHop inspects the first hop of symlink linkPath in dirs[0] and
// reports its target basename and whether its first-hop parent directory lies
// inside dirSet. Masks (/dev/null), broken links, and unreadable parents
// return ok = false.
func topLinkFirstHop(root, dir0Rel, linkPath string, dirSet map[string]bool) (targetBase string, inTree, ok bool, err error) {
	rawTarget, err := os.Readlink(linkPath)
	if errors.Is(err, iofs.ErrNotExist) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	var firstRel string
	if filepath.IsAbs(rawTarget) {
		firstRel = toRelInRoot(root, filepath.Clean(rawTarget))
	} else {
		firstRel = filepath.Clean(filepath.Join(dir0Rel, rawTarget))
	}
	if firstRel == "/dev/null" {
		return "", false, false, nil
	}
	parentPath := fromRelInRoot(root, filepath.Dir(firstRel))
	resolvedParent, pfi, pdev, perr := resolveInRoot(root, parentPath)
	if pdev || isBrokenLink(perr) || errors.Is(perr, iofs.ErrPermission) {
		return "", false, false, nil
	}
	if perr != nil {
		return "", false, false, perr
	}
	inTree = pfi.IsDir() && (dirSet[resolvedParent] || dirSet[parentPath])
	return filepath.Base(firstRel), inTree, true, nil
}

func isDepDir(name string) bool {
	return strings.HasSuffix(name, ".wants") || strings.HasSuffix(name, ".requires") || strings.HasSuffix(name, ".upholds")
}

func isBrokenLink(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

// UnitDirs are the directories systemd reads persistent unit files from,
// most important first: the system's under root, or the user's, with their
// own in configHome (~/.config) and dataHome (~/.local/share). /usr/local
// is where modules deploy their own.
func UnitDirs(root, configHome, dataHome string, user bool) []string {
	if user {
		return []string{
			filepath.Join(configHome, "systemd/user"),
			filepath.Join(root, "etc/systemd/user"),
			filepath.Join(dataHome, "systemd/user"),
			filepath.Join(root, "usr/local/share/systemd/user"),
			filepath.Join(root, "usr/share/systemd/user"),
			filepath.Join(root, "usr/local/lib/systemd/user"),
			filepath.Join(root, "usr/lib/systemd/user"),
		}
	}
	return []string{
		filepath.Join(root, "etc/systemd/system"),
		filepath.Join(root, "usr/local/lib/systemd/system"),
		filepath.Join(root, "usr/lib/systemd/system"),
	}
}

// WithAlso returns units together with what their [Install] sections name
// in Also=, recursively: the units systemctl enable enables with them. Each
// unit file is resolved in dirs inside root (empty defaults to "/") following
// systemd.unit(5) precedence: exact name across dirs, then its template across
// dirs. For bare templates reached through Also=, DefaultInstance= is emitted
// as a concrete instance and updates %i for subsequent Also= lines in that
// template. Unreadable (permission denied) unit files add nothing.
func WithAlso(root string, dirs, units []string) ([]string, error) {
	root = cleanRoot(root)
	var out []string
	emitted := map[string]bool{}
	visited := map[string]bool{}
	emit := func(u string) {
		if !emitted[u] {
			emitted[u] = true
			out = append(out, u)
		}
	}
	type item struct {
		unit    string
		initial bool
	}
	queue := make([]item, len(units))
	for i, u := range units {
		queue[i] = item{unit: u, initial: true}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		u := it.unit
		if it.initial {
			emit(u)
		}
		if visited[u] {
			continue
		}
		visited[u] = true
		tpl := IsTemplate(u)
		if !tpl {
			emit(u)
		}
		path, st, err := unitFile(root, dirs, u)
		if err != nil {
			return nil, err
		}
		if st != unitFound {
			continue
		}
		inst, err := readInstall(path, instanceOf(u), tpl)
		if err != nil {
			if errors.Is(err, iofs.ErrPermission) {
				continue
			}
			return nil, err
		}
		if tpl && inst.defaultInstance != "" {
			// Emit the concrete default instance without marking it visited so
			// an explicit override for that instance is still walked if also
			// declared directly.
			emit(instantiate(u, inst.defaultInstance))
		}
		for _, a := range inst.also {
			queue = append(queue, item{unit: a})
		}
	}
	return out, nil
}

// DisableImpact describes what `systemctl disable` on a unit takes along
// through [Install] Also= in addition to the unit itself.
type DisableImpact struct {
	// Units are the concrete units systemctl disable reaches through [Install]
	// Also= (excluding the starting unit itself, sorted), including enabled
	// instances of any bare template reached through Also=.
	Units []string
	// ByEnable are the concrete units systemctl enable would also enable
	// through Also= (installable concrete targets and installable template
	// DefaultInstance= values, excluding the starting unit itself, sorted).
	ByEnable []string
	// Templates maps each non-masked bare template reached through Also= to
	// the currently enabled concrete instances of that template (sorted),
	// including templates with no enabled instances.
	Templates map[string][]string
}

// DisableClosure walks [Install] Also= (including unenabled Also=-only
// intermediate units) starting from unit, returning what
// `systemctl disable unit` would also disable. Bare templates reached via
// Also= (unless masked) record all matching enabled instances without
// recursing into per-instance overrides, while walking the template itself
// with DefaultInstance=. Unreadable (permission denied) unit files are
// skipped.
func DisableClosure(root string, dirs []string, enabled map[string]bool, unit string) (DisableImpact, error) {
	root = cleanRoot(root)
	unitSet := map[string]bool{}
	byEnableSet := map[string]bool{}
	tplSeen := map[string]map[string]bool{}
	visited := map[string]bool{}

	type item struct {
		unit  string
		start bool
	}
	queue := []item{{unit: unit, start: true}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		u := it.unit
		if visited[u] {
			continue
		}
		visited[u] = true
		tpl := IsTemplate(u)
		path, st, err := unitFile(root, dirs, u)
		if err != nil {
			return DisableImpact{}, err
		}
		var inst installSection
		if st == unitFound {
			inst, err = readInstall(path, instanceOf(u), tpl)
			if err != nil {
				if errors.Is(err, iofs.ErrPermission) {
					st = unitNotFound
				} else {
					return DisableImpact{}, err
				}
			}
		}
		if !it.start {
			if tpl {
				if st != unitMasked {
					if tplSeen[u] == nil {
						tplSeen[u] = map[string]bool{}
					}
					for e := range enabled {
						if Template(e) == u {
							tplSeen[u][e] = true
							unitSet[e] = true
						}
					}
					if st == unitFound && inst.defaultInstance != "" {
						def := instantiate(u, inst.defaultInstance)
						unitSet[def] = true
						if inst.installableFor(def) {
							byEnableSet[def] = true
						}
					}
				}
			} else if u != unit {
				unitSet[u] = true
				if st == unitFound && inst.installableFor(u) {
					byEnableSet[u] = true
				}
			}
		}
		if st == unitFound {
			for _, a := range inst.also {
				queue = append(queue, item{unit: a})
			}
		}
	}

	delete(unitSet, unit)
	delete(byEnableSet, unit)

	var out DisableImpact
	if len(unitSet) > 0 {
		out.Units = slices.Sorted(maps.Keys(unitSet))
	}
	if len(byEnableSet) > 0 {
		out.ByEnable = slices.Sorted(maps.Keys(byEnableSet))
	}
	if len(tplSeen) > 0 {
		out.Templates = make(map[string][]string, len(tplSeen))
		for tpl, insts := range tplSeen {
			if len(insts) > 0 {
				out.Templates[tpl] = slices.Sorted(maps.Keys(insts))
			} else {
				out.Templates[tpl] = nil
			}
		}
	}
	return out, nil
}

// Installable reports whether unit has [Install] directives that
// `systemctl enable` can use to enable it (non-empty WantedBy=, RequiredBy=,
// UpheldBy=, or Alias= for non-instance units; Also= alone does not count).
// Returns iofs.ErrPermission when the unit file cannot be read so callers
// can handle unreadable root-only files according to context.
func Installable(root string, dirs []string, unit string) (bool, error) {
	root = cleanRoot(root)
	path, st, err := unitFile(root, dirs, unit)
	if err != nil {
		return false, err
	}
	if st != unitFound {
		return false, nil
	}
	inst, err := readInstall(path, instanceOf(unit), false)
	if err != nil {
		return false, err
	}
	return inst.installableFor(unit), nil
}

// IsLinkedUnit reports whether unit has a top-level external symlink in
// dirs[0] whose first-hop parent lies outside systemd's unit search
// directories (such as a `systemctl link` or `home/`-deployed unit).
// `systemctl disable` removes such top-level symlinks and `systemctl enable`
// cannot recreate them.
func IsLinkedUnit(root string, dirs []string, unit string) (bool, error) {
	if len(dirs) == 0 {
		return false, nil
	}
	root = cleanRoot(root)
	dir0, fi, devNull, err := resolveInRoot(root, dirs[0])
	if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || devNull {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.IsDir() {
		return false, nil
	}
	p := filepath.Join(dir0, unit)
	lfi, lerr := os.Lstat(p)
	if errors.Is(lerr, iofs.ErrNotExist) || errors.Is(lerr, syscall.ENOTDIR) {
		return false, nil
	}
	if lerr != nil {
		return false, lerr
	}
	if lfi.Mode()&iofs.ModeSymlink == 0 {
		return false, nil
	}
	_, inTree, ok, err := topLinkFirstHop(root, toRelInRoot(root, dir0), p, unitDirSet(root, dirs))
	if err != nil {
		return false, err
	}
	return ok && !inTree, nil
}

// Template returns the template of an instance (a@b.service gives
// a@.service) and any other unit unchanged.
func Template(unit string) string {
	at, dot := strings.Index(unit, "@"), strings.LastIndex(unit, ".")
	if at < 0 || dot < at {
		return unit
	}
	return unit[:at+1] + unit[dot:]
}

// IsTemplate reports whether unit is a bare template unit (a@.service), not an
// instance (a@b.service) or plain unit.
func IsTemplate(unit string) bool {
	at, dot := strings.Index(unit, "@"), strings.LastIndex(unit, ".")
	return at > 0 && dot == at+1 && dot < len(unit)-1
}

// instanceOf returns the instance part of an instantiated unit (a@b.service
// gives "b") and "" for plain units and bare templates.
func instanceOf(unit string) string {
	at, dot := strings.Index(unit, "@"), strings.LastIndex(unit, ".")
	if at <= 0 || dot <= at+1 || dot >= len(unit)-1 {
		return ""
	}
	return unit[at+1 : dot]
}

// instantiate splices inst into a bare template (a@.service + "b" -> a@b.service).
func instantiate(tpl, inst string) string {
	at := strings.Index(tpl, "@")
	return tpl[:at+1] + inst + tpl[at+1:]
}

type unitStatus int

const (
	unitNotFound unitStatus = iota
	unitMasked
	unitFound
)

// unitFile resolves unit across dirs inside root using systemd.unit(5)
// precedence: exact unit name across dirs, then Template(unit) across dirs if
// not found. Stat-only: never opens file contents. A mask (/dev/null or 0-byte
// file), broken/looping symlink, or non-regular file in a higher-priority dir
// returns unitMasked immediately and shadows lower dirs and the template.
func unitFile(root string, dirs []string, unit string) (string, unitStatus, error) {
	path, st, err := lookupUnit(root, dirs, unit)
	if err != nil || st != unitNotFound {
		return path, st, err
	}
	if tpl := Template(unit); tpl != unit {
		return lookupUnit(root, dirs, tpl)
	}
	return "", unitNotFound, nil
}

func lookupUnit(root string, dirs []string, name string) (string, unitStatus, error) {
	for _, dir := range dirs {
		rdir, dfi, ddev, derr := resolveInRoot(root, dir)
		if ddev || isBrokenLink(derr) {
			continue
		}
		if derr != nil {
			return "", unitNotFound, derr
		}
		if !dfi.IsDir() {
			continue
		}
		p := filepath.Join(rdir, name)
		fi, err := os.Lstat(p)
		if errors.Is(err, iofs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			continue
		}
		if err != nil {
			return "", unitNotFound, err
		}
		if fi.Mode()&iofs.ModeSymlink != 0 {
			resolved, rfi, devNull, rerr := resolveInRoot(root, p)
			if devNull || isBrokenLink(rerr) {
				return "", unitMasked, nil
			}
			if rerr != nil {
				return "", unitNotFound, rerr
			}
			if !rfi.Mode().IsRegular() || rfi.Size() == 0 {
				return "", unitMasked, nil
			}
			return resolved, unitFound, nil
		}
		if fi.Mode().IsRegular() {
			if fi.Size() == 0 {
				return "", unitMasked, nil
			}
			return p, unitFound, nil
		}
		return "", unitMasked, nil
	}
	return "", unitNotFound, nil
}

func cleanRoot(root string) string {
	if root == "" {
		return "/"
	}
	return filepath.Clean(root)
}

func toRelInRoot(root, p string) string {
	p = filepath.Clean(p)
	if root == "/" {
		return p
	}
	if p == root {
		return "/"
	}
	if strings.HasPrefix(p, root+string(filepath.Separator)) {
		return filepath.Clean("/" + p[len(root):])
	}
	return p
}

func fromRelInRoot(root, rel string) string {
	rel = filepath.Clean(rel)
	if root == "/" {
		return rel
	}
	if rel == "/" {
		return root
	}
	return filepath.Join(root, rel)
}

func splitPath(rel string) []string {
	rel = strings.Trim(filepath.Clean(rel), "/")
	if rel == "" || rel == "." {
		return nil
	}
	return strings.Split(rel, "/")
}

// resolveInRoot resolves every symlink component of p inside root (up to 40
// hops), clamping relative escapes and prefixing unprefixed absolute targets
// with root so sandboxed roots never touch the host filesystem. Any symlink
// resolving to /dev/null (directly or via relative ..) returns devNull = true.
func resolveInRoot(root, p string) (resolved string, info os.FileInfo, devNull bool, err error) {
	root = cleanRoot(root)
	rel := toRelInRoot(root, p)
	if rel == "/dev/null" {
		return "/dev/null", nil, true, nil
	}
	const maxHops = 40
	hops := 0
	cur := "/"
	parts := splitPath(rel)
	for len(parts) > 0 {
		comp := parts[0]
		parts = parts[1:]
		nextRel := filepath.Join(cur, comp)
		if nextRel == "/dev/null" {
			return "/dev/null", nil, true, nil
		}
		host := fromRelInRoot(root, nextRel)
		fi, lerr := os.Lstat(host)
		if lerr != nil {
			return "", nil, false, lerr
		}
		if fi.Mode()&iofs.ModeSymlink != 0 {
			hops++
			if hops > maxHops {
				return "", nil, false, &os.PathError{Op: "readlink", Path: host, Err: syscall.ELOOP}
			}
			target, rerr := os.Readlink(host)
			if rerr != nil {
				return "", nil, false, rerr
			}
			var targetRel string
			if filepath.IsAbs(target) {
				targetRel = toRelInRoot(root, target)
			} else {
				targetRel = filepath.Clean(filepath.Join(cur, target))
			}
			if targetRel == "/dev/null" {
				return "/dev/null", nil, true, nil
			}
			cur = "/"
			parts = append(splitPath(targetRel), parts...)
			continue
		}
		cur = nextRel
		if len(parts) == 0 {
			return host, fi, false, nil
		}
		if !fi.IsDir() {
			return "", nil, false, &os.PathError{Op: "stat", Path: host, Err: syscall.ENOTDIR}
		}
	}
	host := fromRelInRoot(root, cur)
	fi, lerr := os.Lstat(host)
	if lerr != nil {
		return "", nil, false, lerr
	}
	return host, fi, false, nil
}

type installSection struct {
	also            []string
	alias           []string
	wantedBy        []string
	requiredBy      []string
	upheldBy        []string
	defaultInstance string
}

func (s installSection) installableFor(unit string) bool {
	if len(s.wantedBy) > 0 || len(s.requiredBy) > 0 || len(s.upheldBy) > 0 {
		return true
	}
	return instanceOf(unit) == "" && slices.ContainsFunc(s.alias, func(a string) bool { return a != "" })
}

// readInstall reads the [Install] section of path. When isTemplate is true,
// DefaultInstance= updates the instance used for %i expansion on subsequent
// Also= lines, matching systemd's line-by-line parser; otherwise initialInst
// is fixed throughout.
func readInstall(path, initialInst string, isTemplate bool) (installSection, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return installSection{}, err
	}
	var s installSection
	curInst := initialInst
	install := false
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			install = line == "[Install]"
			continue
		}
		if !install {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Also":
			for _, raw := range strings.Fields(value) {
				if expanded, ok := expandAlso(raw, curInst); ok {
					s.also = append(s.also, expanded)
				}
			}
		case "Alias":
			fields, ok := splitAliasWords(value)
			if !ok {
				continue
			}
			if len(fields) == 0 {
				s.alias = nil
			} else {
				s.alias = append(s.alias, fields...)
			}
		case "WantedBy":
			parseFields(value, &s.wantedBy)
		case "RequiredBy":
			parseFields(value, &s.requiredBy)
		case "UpheldBy":
			parseFields(value, &s.upheldBy)
		case "DefaultInstance":
			v := validInstance(strings.TrimSpace(value))
			s.defaultInstance = v
			if isTemplate {
				curInst = v
			}
		}
	}
	return s, nil
}

func parseFields(value string, dst *[]string) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		*dst = nil
	} else {
		*dst = append(*dst, fields...)
	}
}

// expandAlso expands %% -> % and %i -> inst in a single left-to-right pass.
// Any other % specifier, trailing %, or %i with an empty inst fails so no
// unexpanded specifier is ever emitted.
func expandAlso(raw, inst string) (string, bool) {
	if !strings.Contains(raw, "%") {
		return raw, true
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			continue
		}
		if i+1 >= len(raw) {
			return "", false
		}
		i++
		switch raw[i] {
		case '%':
			b.WriteByte('%')
		case 'i':
			if inst == "" {
				return "", false
			}
			b.WriteString(inst)
		default:
			return "", false
		}
	}
	return b.String(), true
}

func validInstance(v string) string {
	if v == "" || strings.ContainsAny(v, "/ \t\r\n") {
		return ""
	}
	return v
}

// splitAliasWords splits a systemd Alias= value on whitespace while unquoting
// single- and double-quoted words and retaining literal backslashes
// (matching systemd's config_parse_strv: EXTRACT_UNQUOTE | EXTRACT_RETAIN_ESCAPE).
func splitAliasWords(s string) ([]string, bool) {
	var words []string
	i, n := 0, len(s)
	for i < n {
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
			i++
		}
		if i >= n {
			break
		}
		var b strings.Builder
		var quote byte
		for i < n {
			c := s[i]
			if quote != 0 {
				if c == quote {
					quote = 0
					i++
					continue
				}
				b.WriteByte(c)
				i++
				continue
			}
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				break
			}
			if c == '"' || c == '\'' {
				quote = c
				i++
				continue
			}
			b.WriteByte(c)
			i++
		}
		if quote != 0 {
			return nil, false
		}
		words = append(words, b.String())
	}
	return words, true
}

// Groups returns the groups user belongs to. Group names from a directory
// service can hold spaces, so they are read NUL-separated.
func Groups(user string) (map[string]bool, error) {
	out, err := output("id", "-nGz", user)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for g := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\x00") {
		if g != "" {
			set[g] = true // as is: a name with odd spaces is refused, not changed
		}
	}
	return set, nil
}

// PrimaryGroup returns user's primary group.
func PrimaryGroup(user string) (string, error) {
	out, err := output("id", "-gn", user)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(out, "\n"), nil
}
