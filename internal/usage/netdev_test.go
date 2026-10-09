package usage

import (
	"strings"
	"testing"
)

// refNetDev is /proc/<pid>/net/dev of a busybox container on Docker 29.4.3
// (kernel 6.12.76-linuxkit, 2026-10-08), read from the host's pid namespace
// after a download inside it, with a second network connected as eth1. The
// tunnel devices are the kernel's own, present in every namespace; gretap0
// and the longer names have no space before their colon.
const refNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
 tunl0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
gretap0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
ip6_vti0:       0       0    0    0    0     0          0         0        0       0    0    0    0     0       0          0
  eth0:    2182      18    0    0    0     0          0         0      693      10    0    0    0     0       0          0
  eth1:     282       3    0    0    0     0          0         0       42       1    0    0    0     0       0          0
`

// refContainerPID and refContainerStart are that container's init as the host
// saw it; refContainerStat is a stat line shaped like a busybox sleep's with
// that start time as field 22.
const (
	refContainerPID   = 4159321
	refContainerStart = 186542363
	refContainerStat  = "4159321 (sleep) S 4159299 4159321 4159321 0 -1 4194560 120 0 0 0 0 0 0 0 20 0 1 0 186542363 1658880 230 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0"
)

func TestOneInterfaceIsReadFromANetDevTable(t *testing.T) {
	for _, tc := range []struct {
		name, data, device string
		want               [4]int64
		err                string
	}{
		{"eth0 among several", refNetDev, "eth0", [4]int64{2182, 693, 18, 10}, ""},
		{"eth1 is its own line", refNetDev, "eth1", [4]int64{282, 42, 3, 1}, ""},
		{"a name with no space before its colon", "gretap0:5 6 0 0 0 0 0 0 7 8 0 0 0 0 0 0\n",
			"gretap0", [4]int64{5, 7, 6, 8}, ""},
		{"eth0 absent", strings.ReplaceAll(refNetDev, "eth0", "eth9"), "eth0", [4]int64{}, "no eth0"},
		{"a prefix is not the interface", "  eth00: 1 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n", "eth0",
			[4]int64{}, "no eth0"},
		{"eth0 twice", refNetDev + "  eth0: 1 1 0 0 0 0 0 0 1 1 0 0 0 0 0 0\n", "eth0", [4]int64{}, "twice"},
		{"a short line", "  eth0: 1 2 3\n", "eth0", [4]int64{}, "counters"},
		{"a counter that is not a number", "  eth0: x 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n", "eth0",
			[4]int64{}, "eth0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNetDev(tc.data, tc.device)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("parseNetDev = %v, %v; want an error saying %q", got, err, tc.err)
				}

				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("parseNetDev = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// referenceContainer lays the captured container out under a fixture root:
// its init's stat and its namespace's interface table.
func referenceContainer(t *testing.T) (tree, Target) {
	t.Helper()

	tr := newTree(t)
	tr.write("/proc/4159321/stat", refContainerStat+"\n")
	tr.write("/proc/4159321/net/dev", refNetDev)

	return tr, Target{CgroupDir: "/sys/fs/cgroup/system.slice/docker-3a8a.scope",
		PID: refContainerPID, PIDStart: refContainerStart, NetDevice: "eth0", NetNamespace: true}
}

// A CONTAINER'S eth0 IS ITS OWN VIEW: what it received is rx, unswapped, and
// no other interface in its namespace is added in.
func TestAContainersNetworkIsItsOwnEth0(t *testing.T) {
	tr, target := referenceContainer(t)

	s := Reader{Root: tr.root}.Read(target)
	if !s.NetOK || s.NetRx != 2182 || s.NetTx != 693 || s.NetRxPackets != 18 || s.NetTxPackets != 10 {
		t.Errorf("net = ok %v rx %d tx %d (packets %d/%d), want the container's eth0: rx 2182 tx 693 (18/10)",
			s.NetOK, s.NetRx, s.NetTx, s.NetRxPackets, s.NetTxPackets)
	}
}

// AND IT IS READ ONLY WHILE THE PID IS STILL THE CONTAINER'S: a pid whose start
// time no longer matches is another process in another namespace, a target with
// no start cannot be checked, and neither is read as this job's traffic.
func TestAContainersNetworkIsNotReadThroughAPidItCannotProve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(tr tree, target *Target)
	}{
		{"a pid reused by another process", func(tr tree, _ *Target) {
			tr.write("/proc/4159321/stat", strings.Replace(refContainerStat, " 186542363 ", " 186599999 ", 1)+"\n")
		}},
		{"a start that cannot be read", func(tr tree, _ *Target) { tr.remove("/proc/4159321/stat") }},
		{"a target with no start", func(_ tree, target *Target) { target.PIDStart = 0 }},
		{"a target with no pid", func(_ tree, target *Target) { target.PID = 0 }},
		{"eth0 absent", func(tr tree, _ *Target) {
			tr.write("/proc/4159321/net/dev", strings.ReplaceAll(refNetDev, "eth0", "eth9"))
		}},
		{"no table", func(tr tree, _ *Target) { tr.remove("/proc/4159321/net/dev") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, target := referenceContainer(t)
			tc.stage(tr, &target)

			if s := (Reader{Root: tr.root}).Read(target); s.NetOK || s.NetRx != 0 || s.NetTx != 0 {
				t.Errorf("net = ok %v rx %d tx %d, want unmeasured", s.NetOK, s.NetRx, s.NetTx)
			}
		})
	}
}
