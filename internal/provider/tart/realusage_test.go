package tart

import (
	"os"
	"testing"

	"github.com/junioryono/billet/internal/usage"
)

// A REAL VM IS FOUND BY ITS DISK AND READ BY ITS PROCESS. Run on a Mac with a
// billet VM running, naming it: BILLET_TART_USAGE_VM=billet-<lease>. It reads
// and changes nothing.
func TestARealVMIsFoundAndMeasured(t *testing.T) {
	name := os.Getenv("BILLET_TART_USAGE_VM")
	if name == "" {
		t.Skip("BILLET_TART_USAGE_VM names no running VM")
	}
	p, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	target, err := p.UsageTarget(t.Context(), name)
	if err != nil {
		t.Fatalf("UsageTarget: %v", err)
	}
	if !target.Process || target.PID <= 0 || target.PIDStart == 0 {
		t.Fatalf("target %+v is not a proved process", target)
	}
	s := usage.Reader{Root: "/"}.Read(usage.Target{PID: target.PID, PIDStart: target.PIDStart, Process: true})
	if !s.CPUOK || !s.MemoryOK || !s.IOOK || !s.ProcessEnergyOK {
		t.Fatalf("the VM's process read %+v, want cpu, memory, io and energy", s)
	}
	if s.CPUUsage <= 0 || s.MemoryPeak < 1<<30 || s.ProcessEnergy <= 0 {
		t.Errorf("cpu %dµs, memory peak %d bytes, energy %dµJ: a running guest has all three",
			s.CPUUsage, s.MemoryPeak, s.ProcessEnergy)
	}
	t.Logf("pid %d: cpu %.1fs (user %.1fs, system %.1fs), footprint %.2f GB (peak %.2f GB), "+
		"disk read %.2f GB written %.2f GB, energy %.1f J", target.PID, float64(s.CPUUsage)/1e6,
		float64(s.CPUUser)/1e6, float64(s.CPUSys)/1e6, float64(s.MemoryCurrent)/1e9,
		float64(s.MemoryPeak)/1e9, float64(s.DiskRead)/1e9, float64(s.DiskWrite)/1e9,
		float64(s.ProcessEnergy)/1e6)
}
