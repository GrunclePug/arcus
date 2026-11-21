package appimage

import (
	"syscall"
	"unsafe"
)

const renameExchange = 2 // RENAME_EXCHANGE from linux/fs.h

// exchange atomically swaps the files at a and b with renameat2. It returns
// ENOSYS where the call or its number is unavailable.
func exchange(a, b string) error {
	nr := sysRenameat2 // a variable, so the "unknown" -1 needn't convert as a constant
	if nr < 0 {
		return syscall.ENOSYS
	}
	pa, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	pb, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(uintptr(nr),
		uintptr(atFDCWD), uintptr(unsafe.Pointer(pa)),
		uintptr(atFDCWD), uintptr(unsafe.Pointer(pb)),
		renameExchange, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

var atFDCWD = -100 // AT_FDCWD; a variable so the negative value converts to uintptr
