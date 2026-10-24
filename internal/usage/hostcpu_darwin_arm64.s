// The libSystem trampolines hostcpu_darwin.go calls through.

#include "textflag.h"

TEXT libc_mach_host_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·libc_mach_host_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_host_self_trampoline_addr(SB)/8, $libc_mach_host_self_trampoline<>(SB)

TEXT libc_mach_task_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_task_self(SB)
GLOBL	·libc_mach_task_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_task_self_trampoline_addr(SB)/8, $libc_mach_task_self_trampoline<>(SB)

TEXT libc_host_processor_info_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_processor_info(SB)
GLOBL	·libc_host_processor_info_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_processor_info_trampoline_addr(SB)/8, $libc_host_processor_info_trampoline<>(SB)

TEXT libc_vm_deallocate_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_vm_deallocate(SB)
GLOBL	·libc_vm_deallocate_trampoline_addr(SB), RODATA, $8
DATA	·libc_vm_deallocate_trampoline_addr(SB)/8, $libc_vm_deallocate_trampoline<>(SB)
