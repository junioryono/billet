package usage

import "testing"

// A PROCESS JOB'S NETWORK IS ITS DEVICE'S, from the host's side, so what the host
// sent on the VM's vmenet is what the guest received; a reading after which the
// process no longer started when it did is not kept, and the rest of the sample
// is.
func TestAProcessJobsNetworkIsItsDevicesWhileItsProcessLasts(t *testing.T) {
	t.Parallel()

	tr := newTree(t)
	for name, v := range map[string]string{"rx_bytes": "125115", "tx_bytes": "20964202",
		"rx_packets": "1644", "tx_packets": "13931"} {
		tr.write("/sys/class/net/vmenet0/statistics/"+name, v+"\n")
	}
	target := processTarget
	target.NetDevice, target.NetHostView = "vmenet0", true

	for name, tc := range map[string]struct {
		restarted bool
		netOK     bool
	}{
		"the process lasted":                 {netOK: true},
		"the process changed after the read": {restarted: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newProcessHost()
			calls := 0
			r := Reader{Root: tr.root, Process: func(pid int) (ProcessCounters, error) {
				calls++
				c, err := h.reader.Process(pid)
				if tc.restarted && calls > 1 {
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
// read leaves the network unmeasured and nothing else.
func TestAProcessJobsMissingDeviceCostsItsNetworkAlone(t *testing.T) {
	t.Parallel()

	tr := newTree(t)
	h := newProcessHost()
	r := Reader{Root: tr.root, Process: h.reader.Process}
	for _, dev := range []string{"", "vmenet7"} {
		target := processTarget
		target.NetDevice, target.NetHostView = dev, true
		if s := r.Read(target); !s.CPUOK || s.NetOK {
			t.Errorf("a process job with device %q read cpu %v, network %v; want cpu alone", dev, s.CPUOK, s.NetOK)
		}
	}
}
