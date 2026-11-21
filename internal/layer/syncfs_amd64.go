package layer

// Go's frozen syscall package omits SYS_SYNCFS on amd64; Linux defines
// __NR_syncfs as 306 (arch/x86/entry/syscalls/syscall_64.tbl).
const sysSyncfs = 306
