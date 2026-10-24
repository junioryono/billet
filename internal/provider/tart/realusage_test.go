package tart

import (
	"os"
	"strings"
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
	// THE DEPLOYMENT IS THE VM'S OWN: this proves finding and reading a real VM,
	// and the refusal of someone else's is proved above against fixtures.
	probe, err := New(testOwner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	owner, err := probe.ownerOf(name)
	if err != nil {
		t.Fatalf("read %s's ownership marker: %v", name, err)
	}
	p, err := New(owner)
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
	if !strings.HasPrefix(target.NetDevice, "vmenet") || !target.NetHostView {
		t.Fatalf("target %+v names no VM network device from the host's side", target)
	}
	if target.NetPID <= 0 || target.NetPIDStart == 0 {
		t.Fatalf("target %+v names no proved process carrying the VM's link", target)
	}
	s := usage.Reader{Root: "/"}.Read(usage.Target{PID: target.PID, PIDStart: target.PIDStart, Process: true,
		NetDevice: target.NetDevice, NetHostView: target.NetHostView, NetPID: target.NetPID,
		NetPIDStart: target.NetPIDStart})
	if !s.CPUOK || !s.MemoryOK || !s.IOOK || !s.ProcessEnergyOK || !s.NetOK {
		t.Fatalf("the VM's process read %+v, want cpu, memory, io, energy and network", s)
	}
	// A RUNNING RUNNER HAS TALKED TO GITHUB both ways.
	if s.NetRx <= 0 || s.NetTx <= 0 || s.NetRxPackets <= 0 || s.NetTxPackets <= 0 {
		t.Errorf("%s: the guest received %d bytes (%d packets) and sent %d (%d); a runner does both",
			target.NetDevice, s.NetRx, s.NetRxPackets, s.NetTx, s.NetTxPackets)
	}
	t.Logf("%s: guest received %.1f MB (%d packets), sent %.1f MB (%d packets)", target.NetDevice,
		float64(s.NetRx)/1e6, s.NetRxPackets, float64(s.NetTx)/1e6, s.NetTxPackets)
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
