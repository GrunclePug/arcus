# arcus - install Arch Linux and manage it as layered modules.
# See LICENSE file for copyright and license details.

include config.mk

export CGO_ENABLED = 0

SRC = $(shell find cmd internal -type f 2>/dev/null) go.mod go.sum config.mk  # includes embedded files
BIN = bin/arcus bin/arcus-install

# Module paths of the Charm TUI stack. Only arcus-install may link them.
CHARM = ^(charm\.land|github\.com/charmbracelet)/

all: build

build: ${BIN}

# Real file targets, so `sudo make install` after `make` only copies and
# doesn't rebuild as root.
bin/%: ${SRC}
	@mkdir -p bin
	${GO} build ${GOFLAGS} -ldflags "${LDFLAGS}" -o $@ ./cmd/$*

# The assets of a GitHub release: both binaries and their checksums.
release: ${BIN}
	cd bin && sha256sum arcus arcus-install > sha256sums.txt

run:
	${GO} run ./cmd/arcus ${ARGS}

test:
	${GO} test -v ./...

fmt:
	${GO} fmt ./...

lint:
	${GO} vet ./...
	${GO} tool staticcheck ./...
	@if ${GO} list -deps ./cmd/arcus | grep -E '${CHARM}'; then \
		echo "lint: cmd/arcus must not depend on the TUI packages" >&2; exit 1; \
	fi

# Boots the Arch ISO in QEMU and runs arcus-install there.
# Needs qemu-base and edk2-ovmf; the ISO is cached in ~/.cache/arcus/vm.
# SCENARIOS picks them (default: check install luks bios; also: dot, matrix).
vm-test: build
	test/vm/smoke.sh ${SCENARIOS}

# Runs the installer TUI by hand in a QEMU window (BOOT=1: boots what it
# installed). Needs qemu-ui-gtk.
vm-try: build
	BOOT=${BOOT} test/vm/try.sh

clean:
	rm -rf bin

# Only the manager is installed; arcus-install runs from the Arch ISO.
install: bin/arcus
	@mkdir -p ${DESTDIR}${PREFIX}/bin
	cp -f bin/arcus ${DESTDIR}${PREFIX}/bin
	@chmod 755 ${DESTDIR}${PREFIX}/bin/arcus

uninstall:
	rm -f ${DESTDIR}${PREFIX}/bin/arcus

.PHONY: all build release run test fmt lint vm-test vm-try clean install uninstall
