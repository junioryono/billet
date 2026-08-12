package config

import (
	"fmt"
	"math"
	"runtime"
)

// DetectHostCapacity reports what this machine has, for a node that did not say
// what it will contribute.
//
// A DEFAULT, NOT A POLICY. The node's own config wins when it sets max_vcpu or
// max_memory, because the operator of that machine is the one who knows what
// else runs on it. This is what billet assumes when nobody has said.
//
// THERE IS NO FALLBACK VALUE ON AN UNSUPPORTED PLATFORM, deliberately. Each
// platform supplies detectTotalMemory in its own file, so a GOOS billet has not
// been taught about fails to COMPILE rather than registering a node that
// contributes zero — or worse, contributes a number nobody chose. A node whose
// contribution is unknown is how one mis-sized machine silently absorbs a fleet.
func DetectHostCapacity() (int, ByteSize, error) {
	total, err := detectTotalMemory()
	if err != nil {
		return 0, 0, fmt.Errorf("config: detect host memory: %w", err)
	}

	memory, err := memoryToByteSize(total)
	if err != nil {
		return 0, 0, err
	}

	// NumCPU is what the Go runtime may schedule on, which is the honest answer
	// for a bare-metal node. Three qualifications, because none of them is
	// visible from the call: on Linux it is the affinity mask read at process
	// START, so narrowing a running billet's affinity does not change it; on
	// Darwin it is hw.ncpu; and on neither does it follow a cgroup CPU quota, so
	// a process limited to 2 of 64 still reports 64.
	//
	// That last one only matters if billet itself is containerised, and a node is
	// the thing running containers rather than a thing inside one. An operator who
	// does containerise it sets node.max_vcpu, which is what that key is for.
	return runtime.NumCPU(), memory, nil
}

// memoryToByteSize converts a reading into the signed type the rest of billet
// uses, refusing anything it cannot represent.
//
// A READING IS UNSIGNED AND ByteSize IS NOT. Anything above MaxInt64 converts to
// a NEGATIVE size, and a negative one does not fail loudly — it is a ceiling
// that every comparison passes, which silently disables the capacity check that
// stops billet overcommitting a machine. bytesize.go refuses the same thing when
// parsing a config value, for the same reason; a syscall is no more trustworthy
// than a config file about a number this load-bearing.
func memoryToByteSize(bytes uint64) (ByteSize, error) {
	if bytes > math.MaxInt64 {
		return 0, fmt.Errorf("config: this host reports %d bytes of memory, which billet cannot "+
			"represent; set node.max_memory to what it should contribute", bytes)
	}

	return ByteSize(bytes), nil
}
