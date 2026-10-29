# arcus

Install Arch Linux and manage it as a stack of small, swappable modules.

## Overview

arcus is a toolkit on top of stock Arch Linux. `arcus` is the day-to-day manager: modules bundle packages, dotfiles, system files, services and groups, profiles group the modules several machines share, and each machine's host file records which are on, so the setup can be reproduced anywhere. `arcus-install` runs from the Arch ISO and installs a clean Arch system with arcus already on it. Both ship as static Go binaries.

The project is in early development: until 1.0, commands and file formats may still change.

## Features

* **Layered modules**: modules stack in order, the later one wins a shared file, and any module can be turned off in any order with the files below it restored byte for byte.
* **Profiles**: a profile such as `laptop` lists the modules a kind of machine gets; the machine's host file picks profiles and adds its own modules on top.
* **Every resource kind is reversible**: turning a module off removes the packages, units, groups and files it brought, whether arcus installed them or took them over, but never ones another module still wants or that are protected. A package another installed package still needs stays, marked as a dependency, so pacman removes it with the last package that needs it.
* **Same result on any machine**: a machine set up by hand ends up like a fresh install of the same profile after `arcus machine`, `apply` and `clean`.
* **Own builds**: a module can carry PKGBUILD recipes; arcus builds them with makepkg as you, installs them with pacman, and rebuilds when a recipe changes.
* **Conflicts in the plan**: an installed package that a package to install conflicts with is listed in the plan and removed right before its replacement goes in, unless a module wants it.
* **Clean on request**: `apply` never touches packages arcus didn't install; `arcus clean` removes the ones no module lists, after showing its plan.
* **Updates**: from a clone with `make install`, or `arcus update` for the latest release, checksum-verified.
* **Plan, then act**: every change is shown before it runs, including diffs of changed system files.
* **Least privilege**: arcus runs as you and asks sudo only for root steps; the root side refuses unless it recomputes exactly the steps you confirmed.
* **Crash-safe**: a write-ahead ledger is fsynced before every file change, Ctrl-C rolls back cleanly, and `arcus doctor -fix` repairs what a power loss leaves behind.
* **Plain files**: modules, profiles and the host file are TOML with comments; state is readable JSON.
* **Config apart from state**: modules and profiles can live in a dotfiles repository; what a machine has on stays on the machine.
* **Static binaries**: pure Go, no CGO; `arcus` needs only the standard library and one TOML parser.

## Installation

### Requirements

* Arch Linux with sudo, and yay, paru or aurlock for AUR packages
* `base-devel` for own builds
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
arcus doctor               # check state, ledgers and deployed files; list packages no module lists
arcus clean                # remove packages no module lists (shows its plan, asks first)
arcus rebuild dwm-arcus    # rebuild an own build whose recipe didn't change
arcus machine              # write this machine's module: kernel, bootloader, filesystems, swap
arcus update               # replace arcus with the latest release
```

On the Arch ISO, `arcus-install` opens a TUI that writes an install plan and runs it, or saves it to `/root/plan.toml`. `arcus-install detect` shows the hardware modules a machine needs, `arcus-install check plan.toml` validates a plan against the machine without changing anything, and `arcus-install run plan.toml` installs it.

A module is a directory with a `module.toml`, a `home/` tree that is linked into `$HOME`, a `system/` tree that is copied into `/etc` and `/usr/local` as root, and a `pkg/<name>/PKGBUILD` recipe for each own build:

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
```

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
