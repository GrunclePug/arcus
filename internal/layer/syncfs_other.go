//go:build !amd64 && !386

package layer

import "syscall"

const sysSyncfs = syscall.SYS_SYNCFS
