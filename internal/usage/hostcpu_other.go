//go:build !(darwin && arm64)

package usage

// readHostCPU reads /proc/stat, which is where a Linux host keeps its CPU
// counters.
func readHostCPU(r Reader) (HostCPU, error) { return r.readProcStat() }
