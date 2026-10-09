package usage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
		{"a zero start on both sides", func(tr tree, target *Target) {
			tr.write("/proc/4159321/stat", strings.Replace(refContainerStat, " 186542363 ", " 0 ", 1)+"\n")
			target.PIDStart = 0
		}},
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

// AND THE CHECK COMES AFTER THE READ: a pid that changes hands while its table
// is being read is caught. The table is a pipe whose writer replaces the
// stat's start before it lets the read finish, so a start checked before the
// read would still match.
func TestAPidReusedDuringTheNetworkReadIsNotTheContainers(t *testing.T) {
	tr, target := referenceContainer(t)
	table := filepath.Join(tr.root, "proc", "4159321", "net", "dev")
	tr.remove("/proc/4159321/net/dev")
	if err := syscall.Mkfifo(table, 0o600); err != nil {
		t.Fatalf("make the table a pipe: %v", err)
	}

	// opened is closed once the writer's open returns, which a FIFO allows only
	// when a reader has opened the other end; the read cannot reach its EOF
	// before the writer closes, which comes after.
	opened := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(table, os.O_WRONLY, 0)
		if err != nil {
			done <- err

			return
		}
		close(opened)
		reused := strings.Replace(refContainerStat, " 186542363 ", " 186599999 ", 1) + "\n"
		werr := os.WriteFile(filepath.Join(tr.root, "proc", "4159321", "stat"), []byte(reused), 0o600)
		_, err = f.WriteString(refNetDev)
		done <- errors.Join(werr, err, f.Close())
	}()

	s := Reader{Root: tr.root}.Read(target)

	select {
	case <-opened:
	default:
		// THE READER NEVER OPENED THE TABLE. A read end held open until the
		// writer is done lets its open return whenever it gets there.
		t.Error("the reader returned without opening the namespace's table")
		release, err := os.OpenFile(table, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("open the table to release the writer: %v", err)
		}
		defer func() { _ = release.Close() }()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stage the reuse: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the writer never finished")
	}
	if s.NetOK || s.NetRx != 0 {
		t.Errorf("net = ok %v rx %d after the pid changed hands during the read, want unmeasured", s.NetOK, s.NetRx)
	}
}
