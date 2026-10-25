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
func (r *Runner) PacmanInstallFiles(files []string) error {
	return r.run("pacman", r.pkgArgs("-U", files)...)
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

// query runs a read-only command and returns the whitespace-separated
// fields of its output, using only the first field of each line when
// firstField is set. pacman and systemctl exit 1 when nothing matches,
// which is an empty set, not an error.
func query(firstField bool, name string, args ...string) (map[string]bool, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stdout.Len() == 0 && stderr.Len() == 0 {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	set := map[string]bool{}
	for line := range strings.Lines(stdout.String()) {
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

// RequiredBy returns, for each of the installed packages named, the
// installed packages that depend on it, as pacman -Qi lists them.
func RequiredBy(names []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(names) == 0 {
		return out, nil
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("pacman", append([]string{"-Qi"}, names...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C") // the field names, untranslated
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pacman -Qi: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseRequiredBy(stdout.String()), nil
}

// parseRequiredBy reads the Name and Required By fields of pacman -Qi
// output. A long value may go on over indented lines.
func parseRequiredBy(info string) map[string][]string {
	out := map[string][]string{}
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
		switch field {
		case "Name":
			name = strings.TrimSpace(value)
			out[name] = nil
		case "Required By":
			for _, f := range strings.Fields(value) {
				if f != "None" {
					out[name] = append(out[name], f)
				}
			}
		}
	}
	return out
}

// EnabledUnits returns the enabled system or user units.
func EnabledUnits(user bool) (map[string]bool, error) {
	args := []string{"list-unit-files", "--state=enabled", "--no-legend", "--no-pager"}
	if user {
		return query(true, "systemctl", slices.Concat(userScope, args)...)
	}
	return query(true, "systemctl", slices.Concat([]string{"--root=/"}, args)...)
}

// Groups returns the groups user belongs to.
func Groups(user string) (map[string]bool, error) {
	return query(false, "id", "-nG", user)
}
