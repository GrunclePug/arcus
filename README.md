# arcus

Install Arch Linux and manage it as a stack of small, swappable modules.

## Overview

arcus is a toolkit on top of stock Arch Linux. `arcus` is the day-to-day manager: modules bundle packages, dotfiles, system files, services and groups, profiles group the modules several machines share, and each machine's host file records which are on, so the setup can be reproduced anywhere. `arcus-install` runs from the Arch ISO and installs a clean Arch system with arcus already on it. Both ship as static Go binaries.

The project is in early development: until 1.0, commands and file formats may still change.

## Features

* **Layered modules**: modules stack in order, the later one wins a shared file, and any module can be turned off in any order with the files below it restored byte for byte.
* **Profiles**: a profile such as `laptop` lists the modules a kind of machine gets; the machine's host file picks profiles and adds its own modules on top.
* **Every resource kind is reversible**: turning a module off removes the packages, units, groups and files it brought, whether arcus installed them or took them over (packages it takes over are marked explicitly installed, like the ones it installs), but never ones another module still wants or that are protected. A package another installed package still needs stays, marked as a dependency, so pacman removes it with the last package that needs it. A unit another module's unit pulls in through `Also=` stays enabled, and any staying unit `systemctl disable` would take along through `Also=` is re-enabled in the same step (or refused at plan time if it can't be restored cleanly, or if disabling a bare template would wipe an unmanaged instance).
* **Same result on any machine**: a machine set up by hand ends up like a fresh install of the same profile after `arcus machine`, `apply` and `clean`.
* **Own builds**: a module can carry PKGBUILD recipes; arcus builds them with makepkg as you, installs them with pacman, and rebuilds when a recipe changes. An own build that another pending one needs to build (its `depends`, `makedepends` and `checkdepends` as `makepkg --printsrcinfo` lists them, by name or through `provides`) is built and installed in an earlier batch, and one that a package depends on is installed with it or before; unrelated builds share a batch, a batch doesn't remove what one of its builds needs, and a dependency cycle is refused before anything changes.
* **AppImages**: a module can pin a vendor AppImage by URL and sha256; arcus downloads it into `$HOME` as you, updates it when the pin moves, and leaves it alone once the app updates itself.
* **Conflicts in the plan**: an installed package that a package to install conflicts with is listed in the plan and removed right before its replacement goes in, unless a module wants it.
* **Clean on request**: `apply` never touches packages arcus didn't install; `arcus clean` removes the ones no module lists, after showing its plan.
* **Updates**: from a clone with `make install`, or `arcus update` for the latest release, checksum-verified.
* **Plan, then act**: every change is shown before it runs, including diffs of changed system files.
* **Least privilege**: arcus runs as you and asks sudo only for root steps; the root side refuses unless it recomputes exactly the steps you confirmed.
* **State history**: every command that changes the machine is recorded in a private git repository, and `arcus rollback` puts the host file back as of any entry and applies it.
* **Crash-safe**: a write-ahead ledger is fsynced before every file change, Ctrl-C rolls back cleanly, and `arcus doctor -fix` repairs what a power loss leaves behind.
* **Plain files**: modules, profiles and the host file are TOML with comments; state is readable JSON.
* **Config apart from state**: modules and profiles can live in a dotfiles repository; what a machine has on stays on the machine.
* **Static binaries**: pure Go, no CGO; `arcus` needs only the standard library and one TOML parser.

## Installation

### Requirements

* Arch Linux with sudo, and yay, paru or aurlock for AUR packages
* `base-devel` for own builds
* git for the state history (without it, arcus works and records nothing)
* To build from source: Go 1.27.3 or newer and Make

### Download

Each release on GitHub has both binaries, static and ready to run, plus `sha256sums.txt`. To install `arcus` on an existing system:

```sh
curl -LO https://github.com/grunclepug/arcus/releases/latest/download/arcus
sudo install -m 755 arcus /usr/local/bin/arcus
```

On the Arch ISO, download `arcus-install` and `arcus` into the same directory and run `./arcus-install`.

### Build

```sh
git clone https://github.com/grunclepug/arcus.git
cd arcus
make build
sudo make install   # optional; installs arcus only, arcus-install runs from the ISO
```

To update, pull and build as yourself, then install: `git pull && make build && sudo make install`. Building under sudo would leave a root-owned `bin/`.

Without a clone, `arcus update` replaces the installed binary with the latest release after checking it against the release's `sha256sums.txt`; `arcus doctor` notes when a newer release is out.

## Usage

```sh
arcus new editor           # create ~/.config/arcus/modules/editor/module.toml
arcus new -unlisted mine   # a module of what this machine has that no module that's on lists
arcus plan editor          # show what turning it on would do
arcus on editor            # turn it on and record it in this machine's host file
arcus ls                   # every profile and module, its status and stack rank
arcus off editor           # turn it off again
arcus apply                # make the system match the host file
arcus doctor               # check state, ledgers and deployed files; list what no module declares
arcus clean                # remove packages no module lists (shows its plan, asks first)
arcus rebuild dwm-arcus    # rebuild an own build whose recipe didn't change
arcus machine              # write this machine's module: kernel, bootloader, filesystems, swap
arcus update               # replace arcus with the latest release
arcus history              # the latest entries of the state history
arcus rollback 3f2a91c     # put the host file back as of an entry and apply it
```

When a module that's on changes, `apply` redeploys it and the modules above it, and keeps the directories their files live in, so programs watching them keep working. If a redeploy is cut short in a way that leaves its bookkeeping behind (a crash or power loss), `plan` and `apply` stop until you run `arcus doctor -fix`, then `arcus apply`.

To start from an Arch system you already have, write its machine module and turn it on, then let arcus list the rest:

```sh
arcus machine && arcus on machine
arcus new -unlisted mine   # explicitly installed packages, enabled units and groups no module lists
$EDITOR ~/.config/arcus/modules/mine/module.toml   # review it; delete what you don't want kept
arcus on mine              # takes over what is already there
arcus clean -n             # shows what's left: dependencies nothing needs any more
```

`-unlisted` lists packages installed explicitly (from the repositories in `pacman`, the rest in `aur`), enabled system and user units (including stock system defaults such as `getty@tty1.service`, `remote-fs.target` and `systemd-userdbd.socket`, and template instances such as `wg-quick@wg0.service`) and groups, leaving out protected packages and whatever a module that's on lists. A unit that another listed unit enables through `Also=` isn't listed again, and names a module can't hold are left out and shown. User units cover only `$XDG_CONFIG_HOME/systemd/user`, not system-wide package links in `/etc/systemd/user`. It never touches files in `/etc` or `$HOME`. Turning the module off removes everything it lists (including `getty@tty1.service` if left in it), so split it into real modules as you go. A package in `aur` that isn't on the AUR, or a unit you wrote yourself, works on this machine but needs its recipe or unit file in the module before it works on another.

`arcus doctor` also reports what the modules don't account for: a machine module older than what `arcus machine` would write, enabled system units in `/etc/systemd/system` and user units in `$XDG_CONFIG_HOME/systemd/user` and groups that no module that's on lists (following `Also=` in the unit files), and, with aurlock, approvals no module lists. It only reports these; `-fix` never disables a unit, leaves a group or rewrites the machine module. On machines upgraded from an older arcus whose `base` module predates this check, add the system defaults you want kept (`getty@tty1.service`, `remote-fs.target`, `systemd-userdbd.socket`) to `base`. Any user unit a module previously adopted through a global `/etc/systemd/user` link is enabled once in `$XDG_CONFIG_HOME/systemd/user` on the next `apply`.

On the Arch ISO, `arcus-install` opens a TUI that writes an install plan and runs it, or saves it to `/root/plan.toml`. Without a dotfiles repository, it writes the starter config in [examples/](examples/) (`config.toml`, `profiles/` and `modules/`, including a `desktop` profile with Sway — log in on tty1 and run `sway`) to `~/.config/arcus`. `arcus-install detect` shows the hardware modules a machine needs, `arcus-install check plan.toml` validates a plan against the machine without changing anything, and `arcus-install run plan.toml` installs it.

See [examples/](examples/) for a working starter configuration laid out like `~/.config/arcus` that you can read or copy (`config.toml`, `profiles/` and `modules/`). A module is a directory with a `module.toml`, a `home/` tree that is linked into `$HOME`, a `system/` tree that is copied into `/etc` and `/usr/local` as root, and a `pkg/<name>/PKGBUILD` recipe for each own build. Python bytecode caches (`__pycache__/`) in `home/` and `system/` are never deployed. The next `apply` removes those an earlier version deployed, and in `home/` also ones Python has rewritten since:

```toml
description = "Shared Wayland session pieces"
category  = "desktop"
requires  = ["base"]
dirs      = ["Pictures/Screenshots"]   # created if missing; removed on off only when empty
links     = { ".config/app/data" = "Sync/app/data" }   # symlinks in $HOME, both sides relative to it; removed on off only while unchanged; can't overlap a module's home/ files (replacing a staying module's deployed files with a link takes two applies)
conflicts = []
groups    = ["video"]

[packages]
pacman = ["foot", "wmenu", "waybar"]
aur    = []
local  = []                 # own builds, from pkg/<name>/

[units]
system = []                 # concrete units or instances (e.g. "getty@tty1.service"), never bare templates ("getty@.service")
user   = ["mako.service"]   # managed in $XDG_CONFIG_HOME/systemd/user; off leaves package links in /etc/systemd/user alone (now = true still stops the running unit)
now    = false

[appimages.editor]          # one table per AppImage, downloaded into $HOME
url    = "https://example.org/releases/1.2.0/Editor-x86_64.AppImage"
sha256 = "<the release's sha256>"
path   = "Applications/Editor.AppImage"   # relative to $HOME; default Applications/<name>.AppImage
```

An AppImage is downloaded as your user into `<path>.arcus-tmp` beside its destination, checked against `sha256`, made executable, and put in place without replacing anything that appeared there meanwhile. Keep `path` free of the version: an app that updates itself under a new name would otherwise be downloaded again. arcus acts on the file only while it is exactly what arcus installed:

| At the path | `apply` |
|-------------|---------|
| nothing | downloads the pin |
| the pinned file, which arcus didn't install or which isn't executable | takes it over and makes it executable |
| what arcus installed, and the pin moved | downloads the new pin over it |
| what arcus installed, changed since (the app updated itself, or you replaced it) | leaves it alone, and says so whenever it checks the file's content; remove it to get the pin |
| any other file, or a directory | refuses: move it away, then apply again |

Turning the module off, or dropping the table, removes the file while it is what arcus installed; a changed one stays, is named, and is forgotten. arcus never deletes a `<path>.arcus-tmp` left by an interrupted download: the next run refuses until you remove it. As with home files, an AppImage can't go below a directory in `$HOME` that is a symlink, nor inside arcus's own data; nor where two modules that are on both put one, nor at or below a home file or home link of a module that is on; a module's home file turning into an AppImage at the same path takes two applies (remove the file, apply, then add the AppImage).

A profile is a file that lists modules in stacking order, and the host file `~/.local/state/arcus/host.toml` picks profiles and adds the machine's own modules:

```toml
# ~/.config/arcus/profiles/laptop.toml
modules = ["base", "machine", "shell", "desktop", "wl-base", "stream", "battery"]

# ~/.local/state/arcus/host.toml
profiles = ["laptop"]
modules  = ["aurora"]
```

`arcus on` and `arcus off` take a module or a profile, edit those two lists and leave the rest of the file as you wrote it. The host file is state: it stays on the machine, so turning modules on and off never changes a shared dotfiles repository. Before 0.2.0 it was `~/.config/arcus/hosts/<hostname>.toml`; until it is moved, arcus refuses to run and prints the command that moves it.

### Configuration

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| | `-y` | off | Don't ask for confirmation |
| | `-n` | off | Only show the plan |
| | `-f` | off | `off`: also turn off modules that require it; `off`, `apply`: move files edited since deployment aside to `<path>.arcus-foreign` |
| | `-fix` | off | `doctor`: apply the safe repairs |
| | `-unlisted` | off | `new`: list what this machine has that no module that's on lists |
| | `-yes` | off | `arcus-install run`: don't ask to type the device name |
| | `-version` | | Print the version (both binaries) |
| `XDG_CONFIG_HOME` | | `~/.config` | Holds `arcus/config.toml`, `arcus/profiles/` and `arcus/modules/` |
| `XDG_STATE_HOME` | | `~/.local/state` | Holds the host file (`arcus/host.toml`) and the user half's state and ledgers in `arcus/` |
| `XDG_CACHE_HOME` | | `~/.cache` | Holds own builds, their packages and sources in `arcus/` |
| `aur_helper` in `config.toml` | | `yay` | AUR helper: `yay`, `paru` or `aurlock`. The installer builds yay or paru from the AUR; aurlock has to come from a module's own build. |
| `protected` in `config.toml` | | base, base-devel, linux, linux-firmware, sudo, pacman, systemd, the AUR helper | Extra packages arcus never removes |
| `module_dirs` in `config.toml` | | none | Extra directories of user modules |

## State and history

arcus records the machine's state in a bare git repository at `~/.local/state/arcus/history`, readable only by you: the host file, both halves' state and ledgers, and the machine module's `module.toml`, as arcus read them. It records what arcus decided and did, never the contents of the files modules deploy. The system half is recorded as it was when the entry was made.

Each command that changes the machine (`on`, `off`, `apply`, `rebuild`, `rollback`, `clean`, `machine`, `mv` of a module something selects or records, and `doctor -fix` when it repairs something) adds an entry once it is past its confirmation, named after the command line, with ` (failed)` when it failed. Plans, dry runs and declined confirmations add nothing. A change made in between, such as a hand edit of the host file or a run without git, becomes an entry of its own, `unrecorded changes`, before the next command's; the first entry on a machine with state is `history starts`.

When `~/.config/arcus` is in a git repository, as with a dotfiles repository, each entry also notes that repository's commit, and whether the config directory had uncommitted changes. Only that repository is noted, not other `module_dirs`. Recording and rollback never check out a revision of it or edit it; git may run the helpers you configured for it, as your own `git status` would.

`arcus history [count]` lists the latest entries, with the dotfiles commit and a `*` when it had changes. `arcus rollback <entry>` puts the host file's profiles and modules back as they were after that entry, keeping the rest of the file, and then does what `apply` does, with the same plan, confirmation and flags; to undo the latest command, roll back to the entry before it. It doesn't change the modules: when their repository has moved on since the entry, it says so. If a killed arcus left a lock in the history, the next commands warn and `arcus doctor -fix` removes it. To drop the history, remove its directory: `rm -rf ~/.local/state/arcus/history`.

## Development

* `make run ARGS=...`: run `arcus` locally as version `dev`.
* `make test`: run the test suite; it never touches the real system.
* `make fmt`: format the code.
* `make lint`: `go vet`, staticcheck, and a check that `arcus` doesn't depend on the TUI packages.
* `make build`: build `bin/arcus` and `bin/arcus-install`.
* `make release`: build both and write `bin/sha256sums.txt`. Pushing a `v*` tag that matches `VERSION` in `config.mk` makes CI publish them as a GitHub release.
* `make vm-test`: boot the Arch ISO in QEMU and run `arcus-install` there (needs `qemu-base` and `edk2-ovmf`; slow).
* `make vm-try`: run the installer TUI by hand in a QEMU window; `make vm-try BOOT=1` boots what it installed (needs `qemu-ui-gtk`).
* `make clean`: remove `bin/`.

## Contributing

Fork the repository and open a pull request.

## License

MIT. See [LICENSE](LICENSE).

## Author

Chad Humphries | [Website](https://grunclepug.com/) | [GitHub](https://github.com/GrunclePug) | [More projects](https://github.com/GrunclePug?tab=repositories)
