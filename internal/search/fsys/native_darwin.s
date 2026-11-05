// libSystem trampolines for the darwin directory reader (amd64 and arm64
// share this syntax). Addresses are taken in sys_darwin.go and invoked through
// syscall.syscall6 so the runtime handles errno and scheduler hand-off.

#include "textflag.h"

TEXT libc_openat_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_openat(SB)
GLOBL	·libc_openat_trampoline_addr(SB), RODATA, $8
DATA	·libc_openat_trampoline_addr(SB)/8, $libc_openat_trampoline<>(SB)

TEXT libc_getdirentries64_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getdirentries64(SB)
GLOBL	·libc_getdirentries64_trampoline_addr(SB), RODATA, $8
DATA	·libc_getdirentries64_trampoline_addr(SB)/8, $libc_getdirentries64_trampoline<>(SB)

TEXT libc_getattrlistbulk_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getattrlistbulk(SB)
GLOBL	·libc_getattrlistbulk_trampoline_addr(SB), RODATA, $8
DATA	·libc_getattrlistbulk_trampoline_addr(SB)/8, $libc_getattrlistbulk_trampoline<>(SB)

TEXT libc_fstatat_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_fstatat(SB)
GLOBL	·libc_fstatat_trampoline_addr(SB), RODATA, $8
DATA	·libc_fstatat_trampoline_addr(SB)/8, $libc_fstatat_trampoline<>(SB)
