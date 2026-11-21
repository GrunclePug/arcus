# arcus

Install Arch Linux and manage it as a stack of small, swappable modules.

## Overview

arcus is a toolkit on top of stock Arch Linux. `arcus` is the day-to-day manager: modules bundle packages, dotfiles, system files, services and groups, profiles group the modules several machines share, and each machine's host file records which are on, so the setup can be reproduced anywhere. `arcus-install` runs from the Arch ISO and installs a clean Arch system with arcus already on it. Both ship as static Go binaries.

The project is in early development: until 1.0, commands and file formats may still change.

## Features

* **Layered modules**: modules stack in order, the later one wins a shared file, and any module can be turned off in any order with the files below it restored byte for byte.
* **Profiles**: a profile such as `laptop` lists the modules a kind of machine gets; the machine's host file picks profiles and adds its own modules on top.
* **Every resource kind is reversible**: turning a module off removes the packages, units, groups and files it brought, whether arcus installed them or took them over (packages it takes over are marked explicitly installed, like the ones it installs), but never ones another module still wants or that are protected. A package another installed package still needs stays, marked as a dependency, so pacman removes it with the last package that needs it.
* **Same result on any machine**: a machine set up by hand ends up like a fresh install of the same profile after `arcus machine`, `apply` and `clean`.
* **Own builds**: a module can carry PKGBUILD recipes; arcus builds them with makepkg as you, installs them with pacman, and rebuilds when a recipe changes. An own build that another pending one needs to build (its `depends`, `makedepends` and `checkdepends` as `makepkg --printsrcinfo` lists them, by name or through `provides`) is built and installed in an earlier batch, and one that a package depends on is installed with it or before; unrelated builds share a batch, a batch doesn't remove what one of its builds needs, and a dependency cycle is refused before anything changes.
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
arcus new shell            # create ~/.config/arcus/modules/shell/module.toml
arcus plan shell           # show what turning it on would do
arcus on shell             # turn it on and record it in this machine's host file
arcus ls                   # every profile and module, its status and stack rank
arcus off shell            # turn it off again
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

`arcus doctor` also reports what the modules don't account for: a machine module older than what `arcus machine` would write, units enabled against their vendor preset and groups that no module that's on lists (following `Also=` in the unit files), and, with aurlock, approvals no module lists. It only reports these; `-fix` never disables a unit, leaves a group or rewrites the machine module.

On the Arch ISO, `arcus-install` opens a TUI that writes an install plan and runs it, or saves it to `/root/plan.toml`. `arcus-install detect` shows the hardware modules a machine needs, `arcus-install check plan.toml` validates a plan against the machine without changing anything, and `arcus-install run plan.toml` installs it.

A module is a directory with a `module.toml`, a `home/` tree that is linked into `$HOME`, a `system/` tree that is copied into `/etc` and `/usr/local` as root, and a `pkg/<name>/PKGBUILD` recipe for each own build. Python bytecode caches (`__pycache__/`) in `home/` and `system/` are never deployed. The next `apply` removes those an earlier version deployed, and in `home/` also ones Python has rewritten since:

```toml
description = "Shared Wayland session pieces"
category  = "desktop"
requires  = ["base"]
dirs      = ["Pictures/Screenshots"]   # created if missing; removed on off only when empty
links     = { ".config/app/data" = "Sync/app/data" }   # symlinks in $HOME, both sides relative to it; removed on off only while unchanged
conflicts = []
groups    = ["video"]

[packages]
pacman = ["foot", "wmenu", "waybar"]
aur    = []
local  = []                 # own builds, from pkg/<name>/

[units]
system = []
user   = ["mako.service"]
now    = false

[appimages.t3code]          # one table per AppImage, downloaded into $HOME
url         = "https://github.com/pingdotgg/t3code/releases/download/v0.1.11/T3-Code-x86_64.AppImage"
sha256      = "8baa06a7bbaa3f729d69466fb211ed7006d6a6e4d800b9ecdacd831fd1686b14"
path        = "Applications/T3-Code.AppImage"   # relative to $HOME; default Applications/<name>.AppImage
self_update = true                               # the app updates its own file; default false
```

Arcus downloads an AppImage as your user into a temporary file beside its destination. It checks `sha256` before making the file executable and moving it into place. It never replaces a file that turned up or changed there during the download: an update swaps the new version in atomically (`renameat2` with `RENAME_EXCHANGE`) and swaps back a file that changed at the last moment, and on a kernel or filesystem that can't swap files it fails and leaves the old file. A failed or mismatched download stops the command and removes its temporary file. The next download or `arcus doctor -fix` removes temporary files left by an interrupted download. An update interrupted mid-swap can leave a `.<name>.<digits>.swap` file beside the destination, holding the new version or the old file; `arcus doctor` reports it for you to inspect, also once the AppImage is off, and never removes it. The `url` must be https, and the path can't be named `.<name>.<digits>.tmp` or `.<name>.<digits>.swap`, the names of these temporary files. No two modules that are on may declare the same AppImage name or path, an AppImage can't go where a module that is on deploys a home file, a link or a directory from `dirs`, and, as with home files, not below a directory in `$HOME` that is a symlink.

| At the path | `self_update = false` | `self_update = true` |
|-------------|-----------------------|----------------------|
| nothing | download it | download it |
| a file arcus didn't install, with the pinned `sha256` | take it over | take it over |
| a file arcus didn't install, any other | move it aside to `<path>.arcus-foreign`, then download | take it over as it is |
| the file arcus installed, unchanged, and the pin moved | download over it | leave it |
| the file arcus installed, changed since | move it aside, then download | leave it |

Turning the module off, or dropping the table, removes the file arcus recorded: a pinned one only while it is unchanged (a changed one stays and is forgotten), a self-updating one either way. A file below a directory that has become a symlink stays, and `arcus doctor -fix` removes no temporary files there: arcus doesn't follow the link. Directories created for it stay; list them in `dirs` to have them removed when empty. `arcus doctor` reports a declared AppImage that is missing, and a pinned one that doesn't match its pin; `apply` downloads it again. The state records each file's size, times and inode with its `sha256`, so a large AppImage is read again only once it changed.

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
