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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
// and shells outside a login session have none. Queries go through the
// files for system units too, for the same reason.
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
// fields of its output, using only the first field of each line when
// firstField is set.
func query(firstField bool, name string, args ...string) (map[string]bool, error) {
	out, err := output(name, args...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if firstField && len(fields) > 0 {
			fields = fields[:1]
		}
		for _, f := range fields {
			set[f] = true
		}
	}
	return set, nil
}

// Installed returns every installed package, from the repositories or the AUR.
func Installed() (map[string]bool, error) {
	return query(false, "pacman", "-Qq")
}

// Explicit returns the packages installed explicitly, not as a dependency.
func Explicit() (map[string]bool, error) {
	return query(false, "pacman", "-Qqe")
}

// Unrequired returns the installed packages that no installed package
// depends on.
func Unrequired() (map[string]bool, error) {
	return query(false, "pacman", "-Qqt")
}

// Package is what pacman reports about a package, as far as arcus needs
// it. Provides and Conflicts keep their version constraints ("foo=1.2",
// "bar<2").
type Package struct {
	Provides, Conflicts, RequiredBy []string
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
			case "Required By":
				p.RequiredBy = append(p.RequiredBy, f)
			}
		}
		out[name] = p
	}
	return out
}

// SrcInfo returns what the PKGBUILD in dir declares for pkg, from makepkg
// --printsrcinfo. It runs the PKGBUILD, so only as the user.
func SrcInfo(dir, pkg string) (Package, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("makepkg", "--printsrcinfo")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return Package{}, fmt.Errorf("makepkg --printsrcinfo in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	return parseSrcInfo(stdout.String(), pkg), nil
}

// parseSrcInfo reads the provides and conflicts of pkg from .SRCINFO text.
// A field set in pkg's own section replaces the one of the pkgbase section.
func parseSrcInfo(info, pkg string) Package {
	var base, own Package
	ownSet := map[string]bool{}
	section := "pkgbase"
	for line := range strings.Lines(info) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		if k == "pkgname" {
			section = v
			continue
		}
		var p *Package
		switch section {
		case "pkgbase":
			p = &base
		case pkg:
			p = &own
		default:
			continue
		}
		switch k {
		case "provides":
			p.Provides = append(p.Provides, v)
		case "conflicts":
			p.Conflicts = append(p.Conflicts, v)
		default:
			continue
		}
		if section == pkg {
			ownSet[k] = true
		}
	}
	if !ownSet["provides"] {
		own.Provides = base.Provides
	}
	if !ownSet["conflicts"] {
		own.Conflicts = base.Conflicts
	}
	return own
}

// listEnabled is the systemctl call that lists the enabled system or user
// unit files: unit, state and vendor preset on each line.
func listEnabled(user bool) []string {
	args := []string{"list-unit-files", "--state=enabled", "--no-legend", "--no-pager"}
	if user {
		return slices.Concat(userScope, args)
	}
	return slices.Concat([]string{"--root=/"}, args)
}

// EnabledUnits returns the enabled system or user units.
func EnabledUnits(user bool) (map[string]bool, error) {
	return query(true, "systemctl", listEnabled(user)...)
}

// EnabledOffPreset returns the enabled system or user units that their
// vendor preset doesn't enable, so a module or a hand did, sorted.
func EnabledOffPreset(user bool) ([]string, error) {
	out, err := output("systemctl", listEnabled(user)...)
	if err != nil {
		return nil, err
	}
	var units []string
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) > 0 && (len(f) < 3 || f[2] != "enabled") {
			units = append(units, f[0])
		}
	}
	slices.Sort(units)
	return units, nil
}

// UnitDirs are the directories systemd reads unit files from, most
// important first: the system's under root, or the user's, with their own
// in configHome (~/.config).
func UnitDirs(root, configHome string, user bool) []string {
	if user {
		return []string{filepath.Join(configHome, "systemd/user"), filepath.Join(root, "etc/systemd/user"), filepath.Join(root, "usr/lib/systemd/user")}
	}
	return []string{filepath.Join(root, "etc/systemd/system"), filepath.Join(root, "usr/lib/systemd/system")}
}

// WithAlso returns units together with what their [Install] sections name
// in Also=, recursively: the units systemctl enable enables with them. Each
// unit file is the first one found in dirs; an instance (a@b.service) is
// read from its template (a@.service). A unit without a file adds nothing.
func WithAlso(dirs, units []string) []string {
	var out []string
	seen := map[string]bool{}
	queue := slices.Clone(units)
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
		queue = append(queue, installAlso(dirs, Template(u))...)
	}
	return out
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

// installAlso reads the Also= lines of a unit file's [Install] section.
func installAlso(dirs []string, file string) []string {
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		var also []string
		install := false
		for line := range strings.Lines(string(data)) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "[") {
				install = line == "[Install]"
				continue
			}
			if key, value, ok := strings.Cut(line, "="); install && ok && strings.TrimSpace(key) == "Also" {
				also = append(also, strings.Fields(value)...)
			}
		}
		return also
	}
	return nil
}

// Groups returns the groups user belongs to.
func Groups(user string) (map[string]bool, error) {
	return query(false, "id", "-nG", user)
}
