#!/bin/sh
# VM tests: boot the official Arch ISO in QEMU with a blank disk, run
# arcus-install there, and check the result. Run them through
# 'make vm-test', which builds bin/ first.
#
#   test/vm/smoke.sh [scenario...]   default: check install luks bios
#
# check    check accepts a plan and refuses the live medium (UEFI and BIOS)
# install  UEFI + btrfs: install, boot, check through the guest agent
# luks     UEFI + ext4 on LUKS: install, boot (unlocked by a credential), check
# bios     BIOS + xfs + a swap partition + linux-lts: install, boot, check
# dot      the laptop profile of github.com/grunclepug/dot: install, then
#          check it in a chroot (downloads several GB)
# matrix   every firmware, filesystem and encryption combination: install,
#          boot, check (12 installs; MATRIX_FW=uefi or bios runs half)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
bin=$here/../../bin
iso=$("$here/iso.sh")
cache=${XDG_CACHE_HOME:-$HOME/.cache}/arcus/vm
ovmf=/usr/share/edk2/x64
logs=$cache/logs
mkdir -p "$logs"
# On disk, not in /tmp: the installed disks take a few GiB each.
work=$(mktemp -d "$cache/run.XXXXXX")
trap 'rm -rf "$work"' EXIT
(cd "$here/../.." && go build -o "$work/qga" ./test/vm/qga)

boot=${iso%.iso} # the ISO's kernel, initramfs and boot options (iso.sh)

# firmware NAME uefi|bios: QEMU options for the firmware; UEFI keeps its
# variables (GRUB's boot entry) in NAME's stage between boots.
firmware() {
	[ "$2" = uefi ] || return 0
	[ -f "$work/$1/vars.fd" ] || cp "$ovmf/OVMF_VARS.4m.fd" "$work/$1/vars.fd"
	echo "-drive if=pflash,format=raw,readonly=on,file=$ovmf/OVMF_CODE.4m.fd -drive if=pflash,format=raw,file=$work/$1/vars.fd"
}

# vm NAME uefi|bios SIZE COMMANDS: boots the ISO with a blank disk of SIZE
# as /dev/vda and the ISO itself as /dev/vdb (OVMF doesn't find it on an
# IDE CD drive). archiso runs the script= URL after login on tty1; that
# script fetches the files staged here over QEMU's TFTP server, runs
# COMMANDS in them, then powers off. The serial port, with the kernel
# console and the script's output, ends up in $logs/NAME.log.
vm() {
	name=$1 fw=$2 size=$3 commands=$4
	stage=$work/$name
	mkdir -p "$stage"
	cp "$bin/arcus-install" "$bin/arcus" "$here"/plans/*.toml "$stage/"
	files=$(cd "$stage" && echo *)
	cat > "$stage/run.sh" <<GUEST
#!/bin/bash
exec >/dev/ttyS0 2>&1
mkdir -p /tmp/arcus && cd /tmp/arcus
for f in $files; do curl -sf -o "\$f" "tftp://10.0.2.2/\$f" || echo "arcus-vm: cannot fetch \$f"; done
chmod +x arcus-install arcus
$commands
echo "arcus-vm: done"
systemctl poweroff
GUEST
	qemu-img create -q -f qcow2 "$stage/disk.qcow2" "$size"
	echo "== $name: booting the ISO ($fw), log in $logs/$name.log"
	# shellcheck disable=SC2046 # firmware's options are meant to split
	timeout 5400 qemu-system-x86_64 -enable-kvm -cpu host -m 4096 -smp 4 -display none -no-reboot \
		-kernel "$boot/arch/boot/x86_64/vmlinuz-linux" -initrd "$boot/arch/boot/x86_64/initramfs-linux.img" \
		-append "$(cat "$boot/options") script=tftp://10.0.2.2/run.sh console=ttyS0" \
		-drive file="$stage/disk.qcow2",if=virtio,format=qcow2 \
		-drive file="$iso",if=virtio,format=raw,readonly=on \
		-netdev user,id=net0,tftp="$stage" -device virtio-net-pci,netdev=net0 \
		-serial file:"$logs/$name.log" $(firmware "$name" "$fw") </dev/null ||
		echo "smoke: qemu exited with $?" >> "$logs/$name.log"
}

# installed NAME uefi|bios [QEMU OPTION...]: boots the disk NAME installed
# to, without the ISO, and leaves it running for guest commands (its serial
# port is logged in $logs/NAME-boot.log). stop shuts it down.
installed() {
	name=$1 fw=$2
	shift 2
	echo "== $name: booting the installed system ($fw), log in $logs/$name-boot.log"
	# shellcheck disable=SC2046
	qemu-system-x86_64 -enable-kvm -cpu host -m 2048 -smp 2 -display none -no-reboot \
		-drive file="$work/$name/disk.qcow2",if=virtio,format=qcow2 \
		-netdev user,id=net0 -device virtio-net-pci,netdev=net0 \
		-chardev socket,id=qga0,path="$work/$name/qga.sock",server=on,wait=off \
		-device virtio-serial -device virtserialport,chardev=qga0,name=org.qemu.guest_agent.0 \
		-serial file:"$logs/$name-boot.log" $(firmware "$name" "$fw") "$@" </dev/null &
	qemu=$!
}

stop() {
	"$work/qga" -socket "$work/$1/qga.sock" -wait 10s 'systemctl poweroff' >/dev/null 2>&1 || true
	for _ in $(seq 60); do kill -0 "$qemu" 2>/dev/null || return 0; sleep 1; done
	kill "$qemu"
}

fails=0
pass() { echo "ok    $1"; }
fail() {
	echo "FAIL  $1"
	fails=$((fails + 1))
}

# expect NAME TEXT...: every TEXT must appear in the serial log of NAME.
expect() {
	name=$1
	shift
	for text in "$@"; do
		if grep -aqF -- "$text" "$logs/$name.log"; then pass "$name: $text"; else fail "$name: $text"; fi
	done
}

# guest NAME DESCRIPTION COMMAND: runs COMMAND as root in the installed
# system NAME through the guest agent; it must exit 0.
guest() {
	if out=$("$work/qga" -socket "$work/$1/qga.sock" "$3" 2>&1); then
		pass "$1: $2"
	else
		fail "$1: $2"
		echo "$out" | sed 's/^/      /'
	fi
}

# booted NAME: the checks every installed system must pass.
booted() {
	guest "$1" "boots to a running system" 'systemctl is-system-running --wait | grep -qx running || { systemctl --failed; exit 1; }'
	guest "$1" "host file exists" 'test -f /home/tester/.local/state/arcus/host.toml'
	guest "$1" "arcus doctor is clean" 'runuser -u tester -- arcus doctor'
	guest "$1" "arcus plan is empty" 'runuser -u tester -- arcus plan | grep -qx "Nothing to do."'
	guest "$1" "the machine module matches the system" 'runuser -u tester -- arcus machine | grep -q "up to date"'
	guest "$1" "tester can sudo" 'id -nG tester | grep -qw wheel && test -f /etc/sudoers.d/10-wheel && ! test -e /etc/sudoers.d/99-arcus-install'
}

scenario_check() {
	checks='
./arcus-install detect
./arcus-install check PLAN && echo "arcus-vm: plan accepted"
./arcus-install check live-medium.toml || echo "arcus-vm: live medium refused"
'
	vm check-uefi uefi 24G "$(echo "$checks" | sed 's/PLAN/uefi-btrfs.toml/')"
	expect check-uefi "disk      /dev/vda, 24 GiB, wiped" "system    UEFI, linux, btrfs root" \
		"modules   machine base btrfs-snapshots vm-qemu" "arcus-vm: plan accepted" \
		"/dev/vdb is the live medium" "arcus-vm: live medium refused" "arcus-vm: done" \
		"vm         QEMU" "modules    vm-qemu"
	vm check-bios bios 24G "$(echo "$checks" | sed 's/PLAN/bios-xfs.toml/')"
	expect check-bios "system    BIOS, linux-lts, xfs root" "modules   machine base vm-qemu" \
		"arcus-vm: plan accepted" "arcus-vm: live medium refused" "arcus-vm: done"
}

scenario_install() {
	vm install uefi 24G './arcus-install run -yes uefi-btrfs.toml && echo "arcus-vm: installed"'
	expect install "arcus-vm: installed"
	grep -aq "arcus-vm: installed" "$logs/install.log" || return 0
	installed install uefi
	booted install
	guest install "swap on zram" 'swapon --show=NAME --noheadings | grep -q zram'
	guest install "snapper has a root config" 'snapper -c root list'
	stop install
}

scenario_luks() {
	vm luks uefi 24G 'printf "arcusvm\narcusvm\n" | ./arcus-install run -yes uefi-ext4-luks.toml && echo "arcus-vm: installed"'
	expect luks "arcus-vm: installed"
	grep -aq "arcus-vm: installed" "$logs/luks.log" || return 0
	# systemd-cryptsetup reads the cryptsetup.passphrase credential, which
	# QEMU hands to systemd through SMBIOS; GRUB and the initramfs are the
	# real ones.
	installed luks uefi -smbios type=11,value=io.systemd.credential:cryptsetup.passphrase=arcusvm
	booted luks
	guest luks "swap on zram" 'swapon --show=NAME --noheadings | grep -q zram'
	guest luks "root is on LUKS" 'test "$(findmnt -no SOURCE /)" = /dev/mapper/root && cryptsetup status root | grep -q "type:    LUKS2"'
	stop luks
}

scenario_bios() {
	vm bios bios 24G './arcus-install run -yes bios-xfs.toml && echo "arcus-vm: installed"'
	expect bios "arcus-vm: installed"
	grep -aq "arcus-vm: installed" "$logs/bios.log" || return 0
	installed bios bios
	booted bios
	guest bios "booted BIOS, not UEFI" '! test -d /sys/firmware/efi'
	guest bios "linux-lts kernel" 'uname -r | grep -q lts'
	guest bios "xfs root, ext4 /boot" 'test "$(findmnt -no FSTYPE /)" = xfs && test "$(findmnt -no FSTYPE /boot)" = ext4'
	guest bios "swap partition" 'swapon --show=TYPE --noheadings | grep -qx partition'
	stop bios
}

scenario_dot() {
	in_chroot='arch-chroot /mnt runuser -u tester -- env -i HOME=/home/tester PATH=/usr/local/bin:/usr/bin LANG=C.UTF-8 arcus'
	vm dot uefi 48G "./arcus-install run -yes dot.toml && echo \"arcus-vm: installed\"
mount /dev/vda2 /mnt && mount /dev/vda1 /mnt/boot
$in_chroot plan
$in_chroot doctor && echo \"arcus-vm: doctor clean\""
	expect dot "arcus-vm: installed" "Nothing to do." "arcus-vm: doctor clean"
}

# combination NAME uefi|bios FILESYSTEM yes|no: installs a plan for the
# combination, boots it and checks that it is what the plan asked for.
combination() {
	name=$1 fw=$2 fs=$3 enc=$4
	encrypt=false modules='"base", "vm-qemu"'
	[ "$enc" = yes ] && encrypt=true
	[ "$fs" = btrfs ] && modules='"base", "btrfs-snapshots", "vm-qemu"'
	mkdir -p "$work/$name"
	sed -e "s/^filesystem = .*/filesystem = \"$fs\"/" -e "s/^encrypt    = .*/encrypt    = $encrypt/" \
		-e "s/^modules = .*/modules = [$modules]/" "$here/plans/uefi-ext4-luks.toml" > "$work/$name/combination.toml"
	vm "$name" "$fw" 24G 'printf "arcusvm\narcusvm\n" | ./arcus-install run -yes combination.toml && echo "arcus-vm: installed"'
	expect "$name" "arcus-vm: installed"
	grep -aq "arcus-vm: installed" "$logs/$name.log" || return 0
	installed "$name" "$fw" -smbios type=11,value=io.systemd.credential:cryptsetup.passphrase=arcusvm
	booted "$name"
	if [ "$fw" = uefi ]; then
		guest "$name" "booted UEFI" 'test -d /sys/firmware/efi'
	else
		guest "$name" "booted BIOS" '! test -d /sys/firmware/efi'
	fi
	guest "$name" "$fs root" "test \"\$(findmnt -no FSTYPE /)\" = $fs"
	if [ "$enc" = yes ]; then
		guest "$name" "root is on LUKS" 'findmnt -no SOURCE / | grep -q "^/dev/mapper/root"'
	else
		guest "$name" "root is not encrypted" '! test -e /dev/mapper/root'
	fi
	guest "$name" "swap on zram" 'swapon --show=NAME --noheadings | grep -q zram'
	stop "$name"
	rm -f "$work/$name/disk.qcow2"
}

scenario_matrix() {
	for fw in ${MATRIX_FW:-uefi bios}; do
		for fs in btrfs ext4 xfs; do
			for enc in no yes; do
				combination "matrix-$fw-$fs-$enc" "$fw" "$fs" "$enc"
			done
		done
	done
}

for s in ${*:-check install luks bios}; do
	"scenario_$s"
done
echo "smoke: $fails failure(s)"
[ "$fails" -eq 0 ]
