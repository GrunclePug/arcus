#!/bin/sh
# Downloads the current Arch ISO into the VM cache, verified against the
# release's sha256sums.txt, and prints its path. A verified copy is reused.
# Next to it, <iso name>/ gets the ISO's kernel, initramfs and boot options
# (options), for QEMU's direct kernel boot: the boot menus can't take extra
# options unattended.
set -eu

mirror=${ARCUS_VM_MIRROR:-https://geo.mirror.pkgbuild.com/iso/latest}
cache=${XDG_CACHE_HOME:-$HOME/.cache}/arcus/vm
mkdir -p "$cache"
cd "$cache"

curl -fsSL -o sha256sums.txt "$mirror/sha256sums.txt"
name=$(grep -o 'archlinux-[0-9.]*-x86_64\.iso' sha256sums.txt | head -n 1)
[ -n "$name" ] || { echo "iso.sh: no ISO in $mirror/sha256sums.txt" >&2; exit 1; }

if ! grep " $name\$" sha256sums.txt | sha256sum -c --status 2>/dev/null; then
	echo "iso.sh: downloading $name" >&2
	curl -fL --progress-bar -o "$name.part" "$mirror/$name"
	mv "$name.part" "$name"
	grep " $name\$" sha256sums.txt | sha256sum -c --quiet
fi
boot=$cache/${name%.iso}
if [ ! -s "$boot/options" ]; then
	mkdir -p "$boot"
	entry=$(bsdtar -tf "$name" | grep -m 1 '^loader/entries/.*linux\.conf$')
	bsdtar -xf "$name" -C "$boot" arch/boot/x86_64/vmlinuz-linux arch/boot/x86_64/initramfs-linux.img "$entry"
	sed -n 's/^options //p' "$boot/$entry" > "$boot/options"
fi
echo "$cache/$name"
