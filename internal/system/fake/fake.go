// Package fake puts stand-ins for pacman, yay, paru, aurlock, makepkg,
// systemctl, id and gpasswd on PATH for tests. They keep their "system" in
// text files in a temporary directory and log every call, so nothing real
// is touched.
package fake

import (
	"errors"
	iofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// System is a fake system rooted in a temporary directory.
type System struct {
	t          *testing.T
	Dir        string
	Root       string
	ConfigHome string
}

// The fakes share one package database, so the AUR helper sees what pacman
// installed: $dir/pkgs lists the installed packages, $dir/deps those
// installed as a dependency, $dir/foreign those no sync repository has
// (-Qqm lists the installed ones), and $dir/requires "<package> <dependency>"
// lines, and $dir/meta "<package> <field> <name>" lines, which -Qi, -Si,
// -Qip and makepkg report: provides and conflicts, and for own builds
// depends, makedepends and checkdepends (of the PKGBUILD, which makepkg
// needs to build) and pkgdepends (the package's own depends, which only
// installing it needs). Like pacman, -U refuses to install a package whose
// depends or pkgdepends are neither installed nor installed with it, and
// -Rns refuses to remove a package that another one still needs and also
// removes the dependencies nothing needs any more.
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
touch "$dir/pkgs" "$dir/deps" "$dir/foreign" "$dir/requires" "$dir/meta"
meta() {
  awk -v p="$1" -v f="$2" '$1 == p && $2 == f { printf "%s  ", $3 }' "$dir/meta"
}
info() {
  printf 'Name            : %s\nProvides        : %s\nConflicts With  : %s\nDepends On      : %s\n' \
    "$1" "$(meta "$1" provides)" "$(meta "$1" conflicts)" "$(meta "$1" depends)$(meta "$1" pkgdepends)"
}
# satisfied <dependency> <packages>: one of the packages is or provides it.
satisfied() {
  for q in $2; do
    [ "$q" = "$1" ] && return 0
    meta "$q" provides | tr ' ' '\n' | sed 's/=.*//' | grep -qx "$1" && return 0
  done
  return 1
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
  -Qqm)
    [ "$LC_ALL" = C ] || { echo "error: -Qqm must run with LC_ALL=C" >&2; exit 2; }
    echo "warning: config file /etc/pacman.conf, line 10: directive 'TotalDownload' in section 'options' not recognized." >&2
    [ -f "$dir/norepos" ] && { echo "error: no usable package repositories configured." >&2; exit 1; }
    [ -f "$dir/corrupt" ] && { echo "error: could not open file /var/lib/pacman/sync/core.db: Unrecognized archive format" >&2; cat "$dir/pkgs"; exit 0; }
    [ -f "$dir/nosync" ] && { echo "warning: database file for 'core' does not exist (use '-Sy' to download)" >&2; cat "$dir/pkgs"; exit 0; }
    out=$(grep -xF -f "$dir/foreign" "$dir/pkgs"); [ -n "$out" ] || exit 1; echo "$out"; exit 0 ;;
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
if [ "$op" = -U ]; then
  for p in $names; do
    for dep in $(meta "$p" depends) $(meta "$p" pkgdepends); do
      dep=$(echo "$dep" | sed 's/[<>=].*//')
      satisfied "$dep" "$(cat "$dir/pkgs") $names" || { echo "error: $p needs $dep" >&2; exit 1; }
    done
  done
fi
case $op in
  -S|-U)
    for p in $names; do
      grep -qx "$p" "$dir/pkgs" || echo "$p" >> "$dir/pkgs"
      case " $* " in *" --asexplicit "*) grep -vx "$p" "$dir/deps" > "$dir/deps.tmp"; mv "$dir/deps.tmp" "$dir/deps" ;; esac
    done ;;
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
// --printsrcinfo reports what $dir/meta lists for the package: pkgdepends
// as the depends of its own section, the rest in the pkgbase section. Like
// makepkg --syncdeps, it fails unless the depends, makedepends and (with a
// check function) checkdepends that $dir/meta lists are installed or
// provided by an installed package.
const makepkgScript = `#!/bin/sh
dir=%DIR%
if [ "$1" = --printsrcinfo ]; then
  . ./PKGBUILD
  echo "pkgbase = $pkgname"
  touch "$dir/meta"
  awk -v p="$pkgname" '$1 == p && $2 != "pkgdepends" { printf "\t%s = %s\n", $2, $3 }' "$dir/meta"
  echo "pkgname = $pkgname"
  awk -v p="$pkgname" '$1 == p && $2 == "pkgdepends" { printf "\tdepends = %s\n", $3 }' "$dir/meta"
  exit 0
fi
echo "makepkg $* in $(basename "$PWD")" >> "$dir/log"
if [ -f "$dir/fail" ] && grep -qx makepkg "$dir/fail"; then exit 1; fi
. ./PKGBUILD
check=; ! grep -q '^check()' PKGBUILD || check=checkdepends
for dep in $(awk -v p="$pkgname" -v c="$check" '$1 == p && ($2 == "depends" || $2 == "makedepends" || $2 == c) { print $3 }' "$dir/meta" | sed 's/[<>=].*//'); do
  for q in $(cat "$dir/pkgs"); do
    [ "$q" = "$dep" ] && continue 2
    awk -v p="$q" '$1 == p && $2 == "provides" { print $3 }' "$dir/meta" | sed 's/=.*//' | grep -qx "$dep" && continue 2
  done
  echo "missing build dependency: $dep" >&2; exit 1
done
mkdir -p "$PKGDEST"
cat $(find . -type f | sort) > "$PKGDEST/$pkgname-$pkgver-$pkgrel-x86_64.pkg.tar.zst"
`

// systemctlScript writes and removes real enablement symlinks inside the
// sandboxed root/config directories read from $dir/unit-root and
// $dir/unit-config. Like real systemctl, it leaves top-level links (masks,
// home/-deployed units) untouched on disable and removes empty *.wants dirs.
// Unlike real systemctl, enable creates a stub [Unit] file under
// <root>/usr/lib/systemd/{system,user} when neither the unit nor its template
// exists yet, and links into multi-user.target.wants or default.target.wants
// without requiring a WantedBy= line.
const systemctlScript = `#!/bin/sh
dir=%DIR%
printf 'systemctl %s\n' "$*" >> "$dir/log"
user=0
if [ "$1" = --user ]; then user=1; shift; fi
if [ "$1" = --root=/ ]; then shift; fi
op=$1; shift
case $op in
  start|stop) exit 0 ;;
  enable|disable) ;;
  *) printf 'fake systemctl: unsupported op %s\n' "$op" >&2; exit 1 ;;
esac
root=$(cat "$dir/unit-root")
cfg=$(cat "$dir/unit-config")
if [ "$user" = 1 ]; then
  scope="$cfg/systemd/user"
  wants="default.target.wants"
  lib="$root/usr/lib/systemd/user"
  target_prefix="/usr/lib/systemd/user"
  data="$(dirname "$cfg")/.local/share/systemd/user"
else
  scope="$root/etc/systemd/system"
  wants="multi-user.target.wants"
  lib="$root/usr/lib/systemd/system"
  target_prefix="/usr/lib/systemd/system"
fi
has_file() {
  f=$1
  if [ "$user" = 1 ]; then
    for d in "$cfg/systemd/user" "$root/etc/systemd/user" "$data" "$root/usr/local/share/systemd/user" "$root/usr/share/systemd/user" "$root/usr/local/lib/systemd/user" "$root/usr/lib/systemd/user"; do
      if [ -e "$d/$f" ] || [ -L "$d/$f" ]; then return 0; fi
    done
  else
    for d in "$root/etc/systemd/system" "$root/usr/local/lib/systemd/system" "$root/usr/lib/systemd/system"; do
      if [ -e "$d/$f" ] || [ -L "$d/$f" ]; then return 0; fi
    done
  fi
  return 1
}
for u in "$@"; do
  case $u in -*) continue ;; esac
  tpl=$u
  case $u in
    *@*.*)
      rest=${u#*@}
      suffix=.${rest##*.}
      if [ "$rest" = "$suffix" ]; then
        printf 'fake systemctl: bare template %s\n' "$u" >&2
        exit 1
      fi
      tpl="${u%%@*}@${suffix}"
      ;;
  esac
  case $op in
    enable)
      if ! has_file "$u" && ! has_file "$tpl"; then
        mkdir -p "$lib"
        printf '[Unit]\n' > "$lib/$tpl"
      fi
      mkdir -p "$scope/$wants"
      ln -sfn "$target_prefix/$tpl" "$scope/$wants/$u"
      ;;
    disable)
      for dep in "$scope"/*.wants "$scope"/*.requires "$scope"/*.upholds; do
        [ -d "$dep" ] || continue
        if [ -L "$dep/$u" ]; then
          rm -f "$dep/$u"
          rmdir "$dep" 2>/dev/null || true
        fi
      done
      ;;
  esac
done
`

// idScript answers -nG and -nGz from $dir/groups and -gn from $dir/primary, or with
// the user's own name, the group Arch gives a new user.
const idScript = `#!/bin/sh
dir=%DIR%
if [ "$1" = -gn ]; then
  if [ -s "$dir/primary" ]; then cat "$dir/primary"; else echo "$2"; fi
  exit 0
fi
touch "$dir/groups"
if [ "$1" = -nGz ]; then tr '\n' '\0' < "$dir/groups"; exit 0; fi
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
// for the rest of the test. Unit links default to a sandboxed root inside Dir.
func New(t *testing.T) *System {
	t.Helper()
	s := &System{t: t, Dir: t.TempDir()}
	s.SetUnitDirs(filepath.Join(s.Dir, "root"), filepath.Join(s.Dir, "root/home/u/.config"))
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

// SetForeign replaces the packages no sync repository has; -Qqm lists
// those that are installed.
func (s *System) SetForeign(pkgs ...string) { s.set("foreign", pkgs) }

// NoSync makes -Qqm behave as without sync databases: it warns and lists
// every installed package.
func (s *System) NoSync() { s.set("nosync", []string{"1"}) }

// NoRepos makes -Qqm fail as pacman does with no usable repository: an
// error and exit 1, with no output.
func (s *System) NoRepos() { s.set("norepos", []string{"1"}) }

// CorruptSync makes -Qqm behave as with a corrupt sync database: an error,
// every installed package listed, and exit 0.
func (s *System) CorruptSync() { s.set("corrupt", []string{"1"}) }

// SetMeta replaces what packages provide, conflict with and depend on:
// "<package> <field> <name>" lines (see pkgScript).
func (s *System) SetMeta(lines ...string) { s.set("meta", lines) }

// SetRequires replaces the dependencies: "<package> <dependency>" pairs.
func (s *System) SetRequires(pairs ...string) { s.set("requires", pairs) }

// SetUnitDirs points fake systemctl, SetUnits and Units at the given sandboxed
// root and XDG_CONFIG_HOME (read dynamically by fake systemctl).
func (s *System) SetUnitDirs(root, configHome string) {
	s.Root, s.ConfigHome = root, configHome
	s.set("unit-root", []string{root})
	s.set("unit-config", []string{configHome})
}

func (s *System) unitScope(user bool) (scope, wants, lib, targetPrefix string, search []string) {
	if user {
		data := filepath.Join(filepath.Dir(s.ConfigHome), ".local/share/systemd/user")
		return filepath.Join(s.ConfigHome, "systemd/user"), "default.target.wants",
			filepath.Join(s.Root, "usr/lib/systemd/user"), "/usr/lib/systemd/user",
			[]string{
				filepath.Join(s.ConfigHome, "systemd/user"),
				filepath.Join(s.Root, "etc/systemd/user"),
				data,
				filepath.Join(s.Root, "usr/local/share/systemd/user"),
				filepath.Join(s.Root, "usr/share/systemd/user"),
				filepath.Join(s.Root, "usr/local/lib/systemd/user"),
				filepath.Join(s.Root, "usr/lib/systemd/user"),
			}
	}
	return filepath.Join(s.Root, "etc/systemd/system"), "multi-user.target.wants",
		filepath.Join(s.Root, "usr/lib/systemd/system"), "/usr/lib/systemd/system",
		[]string{
			filepath.Join(s.Root, "etc/systemd/system"),
			filepath.Join(s.Root, "usr/local/lib/systemd/system"),
			filepath.Join(s.Root, "usr/lib/systemd/system"),
		}
}

func fakeTemplate(unit string) string {
	at, dot := strings.Index(unit, "@"), strings.LastIndex(unit, ".")
	if at < 0 || dot < at {
		return unit
	}
	return unit[:at+1] + unit[dot:]
}

func isDepDir(name string) bool {
	return strings.HasSuffix(name, ".wants") || strings.HasSuffix(name, ".requires") || strings.HasSuffix(name, ".upholds")
}

// Units returns the system (or user) units linked in *.wants/*.requires/*.upholds, sorted.
func (s *System) Units(user bool) []string {
	scope, _, _, _, _ := s.unitScope(user)
	entries, err := os.ReadDir(scope)
	if errors.Is(err, iofs.ErrNotExist) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	set := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || !isDepDir(e.Name()) {
			continue
		}
		subs, err := os.ReadDir(filepath.Join(scope, e.Name()))
		if err != nil {
			s.t.Fatal(err)
		}
		for _, sub := range subs {
			if sub.Type()&iofs.ModeSymlink != 0 {
				set[sub.Name()] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// SetUnits replaces the enabled system (or user) units in *.wants/*.requires/*.upholds,
// preserving unit files, masks and top-level links. Missing stub unit files are
// written under <Root>/usr/lib/systemd/{system,user} using the template name
// for instances so they never shadow test-written templates.
func (s *System) SetUnits(user bool, units ...string) {
	s.t.Helper()
	scope, wants, lib, targetPrefix, search := s.unitScope(user)
	if entries, err := os.ReadDir(scope); err == nil {
		for _, e := range entries {
			if !e.IsDir() || !isDepDir(e.Name()) {
				continue
			}
			depDir := filepath.Join(scope, e.Name())
			subs, err := os.ReadDir(depDir)
			if err != nil {
				s.t.Fatal(err)
			}
			for _, sub := range subs {
				if sub.Type()&iofs.ModeSymlink != 0 {
					if err := os.Remove(filepath.Join(depDir, sub.Name())); err != nil {
						s.t.Fatal(err)
					}
				}
			}
			_ = os.Remove(depDir)
		}
	} else if !errors.Is(err, iofs.ErrNotExist) {
		s.t.Fatal(err)
	}

	hasFile := func(name string) bool {
		for _, d := range search {
			if _, err := os.Lstat(filepath.Join(d, name)); err == nil {
				return true
			}
		}
		return false
	}
	for _, u := range units {
		tpl := fakeTemplate(u)
		if !hasFile(u) && !hasFile(tpl) {
			if err := os.MkdirAll(lib, 0o755); err != nil {
				s.t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lib, tpl), []byte("[Unit]\n"), 0o644); err != nil {
				s.t.Fatal(err)
			}
		}
		wantsDir := filepath.Join(scope, wants)
		if err := os.MkdirAll(wantsDir, 0o755); err != nil {
			s.t.Fatal(err)
		}
		link := filepath.Join(wantsDir, u)
		_ = os.Remove(link)
		if err := os.Symlink(targetPrefix+"/"+tpl, link); err != nil {
			s.t.Fatal(err)
		}
	}
}

// Groups returns the main user's groups, sorted.
func (s *System) Groups() []string { return sorted(s.lines("groups")) }

// SetGroups replaces the main user's groups.
func (s *System) SetGroups(groups ...string) { s.set("groups", groups) }

// SetPrimaryGroup sets the main user's primary group, which is otherwise
// named after the user.
func (s *System) SetPrimaryGroup(group string) { s.set("primary", []string{group}) }

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
