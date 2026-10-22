#!/bin/sh
# Try the installer by hand, in a QEMU window, the way it runs on a real
# machine: boots the Arch ISO with a blank 32 GiB disk and starts the
# arcus-install TUI on its console. With BOOT=1, boots the disk installed
# last time instead. The disk and the UEFI variables stay in
# ~/.cache/arcus/vm/try until the next install. Needs qemu-ui-gtk for the
# window. Run it through 'make vm-try', which builds bin/ first.
set -eu

[ -e /usr/lib/qemu/ui-gtk.so ] || {
	echo "try.sh: QEMU's window needs the qemu-ui-gtk package" >&2
	exit 1
}
here=$(cd "$(dirname "$0")" && pwd)
iso=$("$here/iso.sh")
boot=${iso%.iso}
try=${XDG_CACHE_HOME:-$HOME/.cache}/arcus/vm/try
ovmf=/usr/share/edk2/x64

set -- -enable-kvm -cpu host -m 4096 -smp 4 -display gtk \
	-drive if=pflash,format=raw,readonly=on,file="$ovmf/OVMF_CODE.4m.fd" \
	-drive if=pflash,format=raw,file="$try/vars.fd" \
	-drive file="$try/disk.qcow2",if=virtio,format=qcow2

if [ "${BOOT:-}" = 1 ]; then
	[ -f "$try/disk.qcow2" ] || { echo "try.sh: nothing installed yet; run it without BOOT=1 first" >&2; exit 1; }
	exec qemu-system-x86_64 "$@" -netdev user,id=net0 -device virtio-net-pci,netdev=net0
fi

rm -rf "$try"
mkdir -p "$try/stage"
cp "$ovmf/OVMF_VARS.4m.fd" "$try/vars.fd"
qemu-img create -q -f qcow2 "$try/disk.qcow2" 32G
cp "$here/../../bin/arcus-install" "$here/../../bin/arcus" "$try/stage/"
cat > "$try/stage/run.sh" <<'GUEST'
#!/bin/bash
cd /root
for f in arcus-install arcus; do curl -sf -o "$f" "tftp://10.0.2.2/$f"; done
chmod +x arcus-install arcus
./arcus-install
GUEST
exec qemu-system-x86_64 "$@" \
	-kernel "$boot/arch/boot/x86_64/vmlinuz-linux" -initrd "$boot/arch/boot/x86_64/initramfs-linux.img" \
	-append "$(cat "$boot/options") script=tftp://10.0.2.2/run.sh" \
	-drive file="$iso",if=virtio,format=raw,readonly=on \
	-netdev user,id=net0,tftp="$try/stage" -device virtio-net-pci,netdev=net0
