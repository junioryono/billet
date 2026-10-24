//go:build darwin && arm64

package usage

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The libSystem calls a macOS host's CPU counters are read through, called the
// way golang.org/x/sys/unix calls libc on darwin: a trampoline per function
// (hostcpu_darwin_arm64.s) and the runtime's own syscall entry, so billet links
// no C and takes no dependency for them.
//
//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_task_self mach_task_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_processor_info host_processor_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_vm_deallocate vm_deallocate "/usr/lib/libSystem.B.dylib"

//nolint:revive // named as the assembly's trampolines are.
var (
	libc_mach_host_self_trampoline_addr      uintptr
	libc_mach_task_self_trampoline_addr      uintptr
	libc_host_processor_info_trampoline_addr uintptr
	libc_vm_deallocate_trampoline_addr       uintptr
)

//go:linkname syscallSyscall syscall.syscall
func syscallSyscall(fn, a1, a2, a3 uintptr) (r1, r2, err uintptr)

//go:linkname syscallSyscall6 syscall.syscall6
func syscallSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2, err uintptr)

const (
	// processorCPULoadInfo is PROCESSOR_CPU_LOAD_INFO from <mach/processor_info.h>.
	processorCPULoadInfo = 2
	// cpuStateMax is CPU_STATE_MAX, the words per processor, in the order
	// CPU_STATE_USER, CPU_STATE_SYSTEM, CPU_STATE_IDLE, CPU_STATE_NICE.
	cpuStateMax = 4
)

// readHostCPU reads a macOS host's CPU counters from the kernel's per-processor
// ticks, since macOS has no /proc/stat. A Reader rooted anywhere but / is a
// fixture tree and is read the Linux way.
//
// AN ORDINARY USER MAY READ THEM, which the node is: measured on macos-01 (macOS
// 27.0.1, M4 Pro, 18 cpus) on 2026-10-24, as the node's account, the ticks
// advanced 100 per cpu-second, kern.clockrate's stathz, and a read took 14 µs.
func readHostCPU(r Reader) (HostCPU, error) {
	if r.Root != "/" {
		return r.readProcStat()
	}

	clock, err := unix.SysctlClockinfo("kern.clockrate")
	if err != nil {
		return HostCPU{}, fmt.Errorf("usage: read kern.clockrate: %w", err)
	}
	if clock.Stathz <= 0 {
		return HostCPU{}, fmt.Errorf("usage: kern.clockrate's stathz is %d", clock.Stathz)
	}

	busy, total, cpus, err := machProcessorTicks()
	if err != nil {
		return HostCPU{}, err
	}

	perTick := int64(1_000_000 / clock.Stathz)

	return HostCPU{Busy: busy * perTick, Total: total * perTick, CPUs: cpus}, nil
}

// machHost is this task's send right to the host port, taken once and kept for
// the process's life.
//
// TAKEN ONCE BECAUSE EACH mach_host_self ADDS A REFERENCE to the right, which
// only a mach_port_deallocate gives back: one taken per sample would add a
// reference a second for as long as the node ran.
var machHost = sync.OnceValue(func() uintptr {
	host, _, _ := syscallSyscall(libc_mach_host_self_trampoline_addr, 0, 0, 0)

	return host
})

// machProcessorTicks sums every processor's busy (user, system and nice) and
// total ticks through host_processor_info, and frees what it was given. It
// answers busy, total and the processor count.
func machProcessorTicks() (int64, int64, int, error) {
	host := machHost()

	// THE KERNEL WRITES A POINTER INTO info, an array it mapped into this task,
	// so info is a pointer and never a uintptr turned back into one.
	var count, infoCount uint32
	var info *uint32
	kr, _, _ := syscallSyscall6(libc_host_processor_info_trampoline_addr, host, processorCPULoadInfo,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&infoCount)), 0)
	if int32(kr) != 0 {
		return 0, 0, 0, fmt.Errorf("usage: host_processor_info answered kern_return %d", int32(kr))
	}
	if info == nil {
		return 0, 0, 0, errors.New("usage: host_processor_info answered no array")
	}

	task, _, _ := syscallSyscall(libc_mach_task_self_trampoline_addr, 0, 0, 0)
	defer syscallSyscall(libc_vm_deallocate_trampoline_addr, task, uintptr(unsafe.Pointer(info)),
		uintptr(infoCount)*4)

	if count == 0 || infoCount < count*cpuStateMax {
		return 0, 0, 0, errors.New("usage: host_processor_info named no processors")
	}

	words := unsafe.Slice(info, infoCount)
	var busy, total int64
	for c := range int(count) {
		user, system := int64(words[c*cpuStateMax]), int64(words[c*cpuStateMax+1])
		idle, nice := int64(words[c*cpuStateMax+2]), int64(words[c*cpuStateMax+3])
		busy += user + system + nice
		total += user + system + nice + idle
	}

	return busy, total, int(count), nil
}
