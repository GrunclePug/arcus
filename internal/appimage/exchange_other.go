//go:build !386 && !amd64 && !arm64 && !riscv64

package appimage

// sysRenameat2 is unknown here; exchange then reports ENOSYS, and an
// update is refused.
const sysRenameat2 = -1
