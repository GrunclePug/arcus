package layer

// Go's frozen syscall package omits SYS_SYNCFS on 386; Linux defines
// __NR_syncfs as 344 (arch/x86/entry/syscalls/syscall_32.tbl).
const sysSyncfs = 344
