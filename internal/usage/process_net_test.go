package usage

import (
	"os"
	"strings"
	"testing"
)

// softnetPID is the process carrying the VM's frames under softnet, in these
// tests, and softnetStart its start.
const (
	softnetPID   = 62980
	softnetStart = 16664085550000
)

// A PROCESS JOB'S NETWORK IS ITS DEVICE'S, from the host's side, so what the host
// sent on the VM's vmenet is what the guest received. A reading during which the
// VM's process, or the softnet carrying its link, was replaced is not kept, and
// the rest of the sample is.
func TestAProcessJobsNetworkIsItsDevicesWhileItsCarrierLasts(t *testing.T) {
	t.Parallel()

	tr := newTree(t)
	for name, v := range map[string]string{"rx_bytes": "125115", "tx_bytes": "20964202",
		"rx_packets": "1644", "tx_packets": "13931"} {
		tr.write("/sys/class/net/vmenet0/statistics/"+name, v+"\n")
	}

	for name, tc := range map[string]struct {
		carrier  int
		replaced int
		netOK    bool
	}{
		"the shared network's VM lasted":            {netOK: true},
		"the VM was replaced during the read":       {replaced: processTarget.PID},
		"the softnet lasted":                        {carrier: softnetPID, netOK: true},
		"the softnet was replaced during the read":  {carrier: softnetPID, replaced: softnetPID},
		"the VM under softnet was replaced instead": {carrier: softnetPID, replaced: processTarget.PID},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target := processTarget
			target.NetDevice, target.NetHostView = "vmenet0", true
			if tc.carrier != 0 {
				target.NetPID, target.NetPIDStart = tc.carrier, softnetStart
			}
			h := newProcessHost()
			// THE PROCESS IS REPLACED WHILE THE DEVICE IS READ, so a check made
			// before the read passes and only one made after it refuses.
			replaced := false
			r := Reader{Root: tr.root,
				ReadFile: func(name string) ([]byte, error) {
					if strings.Contains(name, "/sys/class/net/") && tc.replaced != 0 {
						replaced = true
					}

					return os.ReadFile(name)
				},
				Process: func(pid int) (ProcessCounters, error) {
					c, err := h.reader.Process(processTarget.PID)
					if pid == softnetPID {
						c.Start = softnetStart
					} else if pid != processTarget.PID {
						return ProcessCounters{}, os.ErrNotExist
					}
					if replaced && pid == tc.replaced {
						c.Start++
					}

					return c, err
				}}
			s := r.Read(target)
			if !s.CPUOK || !s.MemoryOK {
				t.Fatalf("the process's own groups were lost: %+v", s)
			}
			if s.NetOK != tc.netOK {
				t.Fatalf("network read %v, want %v", s.NetOK, tc.netOK)
			}
			if tc.netOK && (s.NetRx != 20964202 || s.NetTx != 125115 || s.NetRxPackets != 13931 ||
				s.NetTxPackets != 1644) {
				t.Errorf("the guest received %d bytes (%d packets) and sent %d (%d), want the host's sent "+
					"as received", s.NetRx, s.NetRxPackets, s.NetTx, s.NetTxPackets)
			}
		})
	}
}

// A PROCESS JOB WITH NO DEVICE IS NOT READ FOR ONE, and a device that cannot be
// read, or whose carrier's start is unknown, leaves the network unmeasured and
// nothing else.
func TestAProcessJobsMissingDeviceCostsItsNetworkAlone(t *testing.T) {
	t.Parallel()

	tr := newTree(t)
	tr.write("/sys/class/net/vmenet0/statistics/rx_bytes", "1\n")
	tr.write("/sys/class/net/vmenet0/statistics/tx_bytes", "1\n")
	tr.write("/sys/class/net/vmenet0/statistics/rx_packets", "1\n")
	tr.write("/sys/class/net/vmenet0/statistics/tx_packets", "1\n")
	h := newProcessHost()
	r := Reader{Root: tr.root, Process: func(pid int) (ProcessCounters, error) {
		c, err := h.reader.Process(processTarget.PID)
		if pid == softnetPID {
			c.Start = softnetStart
		}

		return c, err
	}}
	for name, target := range map[string]Target{
		"no device":                  {NetDevice: ""},
		"a device that is not there": {NetDevice: "vmenet7"},
		"a carrier with no start":    {NetDevice: "vmenet0", NetPID: softnetPID},
	} {
		tg := processTarget
		tg.NetDevice, tg.NetHostView, tg.NetPID, tg.NetPIDStart = target.NetDevice, true, target.NetPID,
			target.NetPIDStart
		if s := r.Read(tg); !s.CPUOK || s.NetOK {
			t.Errorf("a process job with %s read cpu %v, network %v; want cpu alone", name, s.CPUOK, s.NetOK)
		}
	}
}
