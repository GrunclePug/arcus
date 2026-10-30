// Package fake puts stand-ins for pacman, yay, paru, aurlock, makepkg,
// systemctl, id and gpasswd on PATH for tests. They keep their "system" in
// text files in a temporary directory and log every call, so nothing real
// is touched.
package fake

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// System is a fake system rooted in a temporary directory.
type System struct {
	t   *testing.T
	Dir string
}

// The fakes share one package database, so the AUR helper sees what pacman
// installed: $dir/pkgs lists the installed packages, $dir/deps those
// installed as a dependency, and $dir/requires "<package> <dependency>"
// lines, and $dir/meta "<package> provides|conflicts <name>" lines, which
// -Qi, -Si, -Qip and makepkg --printsrcinfo report. Like pacman, -Rns refuses to remove a package that another one
// still needs and also removes the dependencies nothing needs any more.
// Every call is appended to $dir/log. If $dir/slow exists, a package
// transaction writes $dir/started, sleeps that many seconds and writes
// $dir/done when it finishes.
const pkgScript = `#!/bin/sh
dir=%DIR%
echo "$(basename "$0") $*" >> "$dir/log"
if [ "$1" = --removemake ]; then shift; fi
op=$1; shift
names=
for a in "$@"; do
  case $a in
    -*) ;;
    *.pkg.tar.*) names="$names $(basename "$a" | sed 's/-[^-]*-[^-]*-[^-]*\.pkg\.tar\..*$//')" ;;
    *) names="$names $a" ;;
  esac
done
touch "$dir/pkgs" "$dir/deps" "$dir/requires" "$dir/meta"
meta() {
  awk -v p="$1" -v f="$2" '$1 == p && $2 == f { printf "%s  ", $3 }' "$dir/meta"
}
info() {
  printf 'Name            : %s\nProvides        : %s\nConflicts With  : %s\n' "$1" "$(meta "$1" provides)" "$(meta "$1" conflicts)"
}
required() {
  while read -r p d; do grep -qx "$p" "$dir/pkgs" && echo "$d"; done < "$dir/requires"
}
drop() {
  for f in pkgs deps; do grep -vx "$1" "$dir/$f" > "$dir/$f.tmp"; mv "$dir/$f.tmp" "$dir/$f"; done
}
case $op in
  -Qq) cat "$dir/pkgs"; exit 0 ;;
  -Qqe) out=$(grep -vxF -f "$dir/deps" "$dir/pkgs"); [ -n "$out" ] || exit 1; echo "$out"; exit 0 ;;
  -Qqt) required > "$dir/req"; out=$(grep -vxF -f "$dir/req" "$dir/pkgs"); [ -n "$out" ] || exit 1; echo "$out"; exit 0 ;;
  -Qi)
    [ -n "$names" ] || names=$(cat "$dir/pkgs")
    for p in $names; do
      grep -qx "$p" "$dir/pkgs" || { echo "error: package '$p' was not found" >&2; exit 1; }
      by=$(while read -r q d; do [ "$d" = "$p" ] && grep -qx "$q" "$dir/pkgs" && printf '%s  ' "$q"; done < "$dir/requires")
      info "$p"
      printf 'Required By     : %s\n\n' "${by:-None}"
    done
    exit 0 ;;
  -Si|-Qip) for p in $names; do info "$p"; echo; done; exit 0 ;;
esac
if [ -f "$dir/fail" ] && grep -qx "$(basename "$0")" "$dir/fail"; then exit 1; fi
if [ -f "$dir/slow" ]; then touch "$dir/started"; sleep "$(cat "$dir/slow")"; fi
case $op in
  -S|-U) for p in $names; do grep -qx "$p" "$dir/pkgs" || echo "$p" >> "$dir/pkgs"; done ;;
  -D)
    for p in $names; do
      grep -vx "$p" "$dir/deps" > "$dir/deps.tmp"; mv "$dir/deps.tmp" "$dir/deps"
      [ "$1" = --asdeps ] && echo "$p" >> "$dir/deps"
    done ;;
  -Rdd) for p in $names; do drop "$p"; done ;;
  -Rns)
    for p in $names; do
      while read -r q d; do
        [ "$d" = "$p" ] && grep -qx "$q" "$dir/pkgs" || continue
        case " $names " in *" $q "*) ;; *) echo "error: $q requires $p" >&2; exit 1 ;; esac
      done < "$dir/requires"
    done
    removed=" $names "
    for p in $names; do drop "$p"; done
    changed=1
    while [ -n "$changed" ]; do
      changed=
      while read -r q d; do
        case $removed in *" $q "*) ;; *) continue ;; esac
        grep -qx "$d" "$dir/deps" && ! required | grep -qx "$d" || continue
        drop "$d"; removed="$removed$d "; changed=1
      done < "$dir/requires"
    done ;;
esac
if [ -f "$dir/slow" ]; then touch "$dir/done"; fi
`

// makepkg builds the PKGBUILD in the current directory into
// $PKGDEST/<pkgname>-<pkgver>-<pkgrel>-x86_64.pkg.tar.zst, whose content is
// the recipe's files, so a different recipe gives a different package.
// --printsrcinfo reports what $dir/meta lists for the package.
const makepkgScript = `#!/bin/sh
dir=%DIR%
if [ "$1" = --printsrcinfo ]; then
  . ./PKGBUILD
  echo "pkgbase = $pkgname"
  touch "$dir/meta"
  awk -v p="$pkgname" '$1 == p { printf "\t%s = %s\n", $2, $3 }' "$dir/meta"
  echo "pkgname = $pkgname"
  exit 0
fi
echo "makepkg $* in $(basename "$PWD")" >> "$dir/log"
if [ -f "$dir/fail" ] && grep -qx makepkg "$dir/fail"; then exit 1; fi
. ./PKGBUILD
mkdir -p "$PKGDEST"
cat $(find . -type f | sort) > "$PKGDEST/$pkgname-$pkgver-$pkgrel-x86_64.pkg.tar.zst"
`

const systemctlScript = `#!/bin/sh
dir=%DIR%
echo "systemctl $*" >> "$dir/log"
db="$dir/units"
if [ "$1" = --user ]; then db="$dir/user-units"; shift; fi
if [ "$1" = --root=/ ]; then shift; fi
op=$1; shift
touch "$db"
case $op in
  list-unit-files) out=$(while read -r u; do [ -n "$u" ] || continue; p=disabled; grep -qsx "$u" "$db-preset" && p=enabled; echo "$u enabled $p"; done < "$db"); [ -n "$out" ] || exit 1; echo "$out"; exit 0 ;;
esac
for u in "$@"; do
  case $u in -*) continue ;; esac
  case $op in
    enable) grep -qx "$u" "$db" || echo "$u" >> "$db" ;;
    disable) grep -vx "$u" "$db" > "$db.tmp"; mv "$db.tmp" "$db" ;;
  esac
done
`

const idScript = `#!/bin/sh
dir=%DIR%
touch "$dir/groups"
tr '\n' ' ' < "$dir/groups"; echo
`

const gpasswdScript = `#!/bin/sh
dir=%DIR%
echo "gpasswd $*" >> "$dir/log"
touch "$dir/groups"
case $1 in
  -a) grep -qx "$3" "$dir/groups" || echo "$3" >> "$dir/groups" ;;
  -d) grep -vx "$3" "$dir/groups" > "$dir/groups.tmp"; mv "$dir/groups.tmp" "$dir/groups" ;;
esac
`

// New installs the fakes in a temporary directory and puts it first on PATH
// for the rest of the test.
func New(t *testing.T) *System {
	t.Helper()
	s := &System{t: t, Dir: t.TempDir()}
	bin := filepath.Join(s.Dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"pacman": pkgScript, "yay": pkgScript, "paru": pkgScript, "aurlock": pkgScript, "makepkg": makepkgScript,
		"systemctl": systemctlScript, "id": idScript, "gpasswd": gpasswdScript,
	}
	for name, script := range scripts {
		script = strings.ReplaceAll(script, "%DIR%", "'"+s.Dir+"'")
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s
}

func (s *System) lines(name string) []string {
	data, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (s *System) set(name string, values []string) {
	content := strings.Join(values, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func sorted(v []string) []string {
	slices.Sort(v)
	return v
}

// Packages returns the installed packages, sorted.
func (s *System) Packages() []string { return sorted(s.lines("pkgs")) }

// SetPackages replaces the installed packages.
func (s *System) SetPackages(pkgs ...string) { s.set("pkgs", pkgs) }

// Deps returns the packages installed as a dependency, sorted.
func (s *System) Deps() []string { return sorted(s.lines("deps")) }

// SetDeps marks installed packages as installed as a dependency.
func (s *System) SetDeps(pkgs ...string) { s.set("deps", pkgs) }

// SetMeta replaces what packages provide and conflict with:
// "<package> provides|conflicts <name>" lines.
func (s *System) SetMeta(lines ...string) { s.set("meta", lines) }

// SetRequires replaces the dependencies: "<package> <dependency>" pairs.
func (s *System) SetRequires(pairs ...string) { s.set("requires", pairs) }

// Units returns the enabled system (or user) units, sorted.
func (s *System) Units(user bool) []string {
	if user {
		return sorted(s.lines("user-units"))
	}
	return sorted(s.lines("units"))
}

// SetUnits replaces the enabled system (or user) units.
func (s *System) SetUnits(user bool, units ...string) {
	if user {
		s.set("user-units", units)
	} else {
		s.set("units", units)
	}
}

// SetPresets replaces the system (or user) units that the vendor preset
// enables; list-unit-files reports every other unit as preset disabled.
func (s *System) SetPresets(user bool, units ...string) {
	if user {
		s.set("user-units-preset", units)
	} else {
		s.set("units-preset", units)
	}
}

// Groups returns the main user's groups, sorted.
func (s *System) Groups() []string { return sorted(s.lines("groups")) }

// SetGroups replaces the main user's groups.
func (s *System) SetGroups(groups ...string) { s.set("groups", groups) }

// Log returns every changing or querying call made so far, in order.
func (s *System) Log() []string { return s.lines("log") }

// Fail makes the named tool (pacman, yay, ...) fail its next transactions.
func (s *System) Fail(tool string) { s.set("fail", []string{tool}) }

// Slow makes package transactions take seconds, writing a "started" marker
// when they begin and a "done" marker when they finish.
func (s *System) Slow(seconds string) { s.set("slow", []string{seconds}) }

// Started reports whether a slow package transaction has begun.
func (s *System) Started() bool {
	_, err := os.Stat(filepath.Join(s.Dir, "started"))
	return err == nil
}

// Done reports whether a slow package transaction ran to completion.
func (s *System) Done() bool {
	_, err := os.Stat(filepath.Join(s.Dir, "done"))
	return err == nil
}
