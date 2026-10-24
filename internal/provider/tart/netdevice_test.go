package tart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// bridgeTable is `ifconfig bridge100 addr` on the reference Mac with a VM on
// tart's shared network (vmenet0) and one under softnet (vmenet1), macOS
// 27.0.1, 2026-10-24.
const bridgeTable = "7a:4c:3:da:6:3 Vlan1 vmenet1 1180 flags=0<>\n" +
	"86:46:87:ef:d0:52 Vlan1 vmenet0 1197 flags=0<>\n"

// unitLine is how `lsof -F n` names a userspace_ethernet socket of unit n.
func unitLine(n string) string { return "n[ctl com.apple.userspace_ethernet id 14 unit " + n + "]" }

// sharedVM and softVM are the VMs of bridgeTable, as `ps -axo pid=,ppid=,args=`
// and `lsof` see them: the shared network's VM process holds its own socket,
// and under softnet the socket is the softnet process's that the VM's `tart run`
// started.
const (
	sharedMAC = "86:46:87:ef:d0:52"
	softMAC   = "7a:4c:03:da:06:03"
	tartRun   = "/opt/homebrew/Cellar/tart/2.37.0/libexec/tart.app/Contents/MacOS/tart run " + usageVM +
		" --no-graphics --net-softnet --net-softnet-block=@host"
	softnetRun = "/opt/homebrew/bin/softnet --vm-fd 0 --vm-mac-address " + softMAC + " --block @host"
)

// network is what a host says about a VM's link: the VM's configured address,
// each process's sockets as lsof lists them, the process table and each
// bridge's address table (each a line of shell, as lsof's answers are).
type network struct {
	mac     string
	sockets map[string]string
	ps      string
	tables  map[string]string
	// owners is `ps -axo uid=,args=`; empty is a host whose only VM is this one,
	// the node account's.
	owners string
}

// withNetwork gives h's VM and host the network n describes.
func (h usageHost) withNetwork(t *testing.T, n network) {
	t.Helper()

	config := `{"version":1,"os":"linux","macAddress":"` + n.mac + `"}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(h.disk), "config.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("write the VM's configuration: %v", err)
	}
	bin := t.TempDir()
	write := func(name, script string) string {
		path := filepath.Join(bin, name)
		if err := forkSafeWriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write the %s shim: %v", name, err)
		}

		return path
	}

	var lsof strings.Builder
	lsof.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	for pid, answer := range n.sockets {
		lsof.WriteString("  \"-nP +w -a -p " + pid + " -F n\") printf 'p" + pid + "\\nf3\\nn/dev/null\\n'; " +
			answer + " ;;\n")
	}
	lsof.WriteString("  *\"-F n\"*) exit 1 ;;\nesac\nexec " + h.p.lsof + " \"$@\"\n")
	h.p.lsof = write("lsof", lsof.String())

	owners := n.owners
	if owners == "" {
		owners = fmt.Sprintf("%d %s\n", os.Getuid(), vmService)
	}
	h.p.ps = write("ps", "#!/bin/sh\ncase \"$*\" in\n"+
		"  '-axo pid=,ppid=,args=') printf '%s' '"+n.ps+"' ;;\n"+
		"  '-axo uid=,args=') printf '%s' '"+owners+"' ;;\n"+
		"  *) exit 2 ;;\nesac\n")

	var ifconfig strings.Builder
	ifconfig.WriteString("#!/bin/sh\ncase \"$1 $2\" in\n")
	var bridges []string
	for bridge, answer := range n.tables {
		bridges = append(bridges, bridge)
		ifconfig.WriteString("  \"" + bridge + " addr\") " + answer + " ;;\n")
	}
	ifconfig.WriteString("  *) echo \"unexpected: $*\" >&2; exit 2 ;;\nesac\n")
	h.p.ifconfig = write("ifconfig", ifconfig.String())
	h.p.bridges = func() ([]string, error) { return bridges, nil }
}

// printed is the shell that prints s.
func printed(s string) string { return "printf '%s\\n' '" + s + "'" }

// table is bridge100 holding bridgeTable, and an empty bridge101.
var table = map[string]string{"bridge100": "printf '%s' '" + bridgeTable + "'", "bridge101": "true"}

// psWith is a process table holding the given rows, each "pid ppid args".
func psWith(rows ...string) string {
	return "    1     0 /sbin/launchd\n61958     1 /usr/local/bin/billet node\n" + strings.Join(rows, "\n") + "\n"
}

// newNetworkHost is a usage host whose VM process 74207 has been proved.
func newNetworkHost(t *testing.T) usageHost {
	t.Helper()

	h := newUsageHost(t, "echo 74207")
	h.paths[74207], h.starts[74207] = vmService, 16664085551217
	h.starts[62980], h.starts[62990] = 16664085550000, 16664085550001

	return h
}

// A VM'S NETWORK IS THE VMENET THE BRIDGE LEARNED ITS ADDRESS ON, carried by the
// process its link's socket proves: the VM's own on tart's shared network, and
// the softnet its `tart run` started under softnet; counted from the host's
// side, beside the process found as ever.
func TestAVMsNetworkIsTheVMNetItsLinksSocketNames(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		n       network
		dev     string
		carrier int
		start   uint64
	}{
		"the shared network": {dev: "vmenet0", n: network{mac: sharedMAC,
			tables: table, ps: psWith(), sockets: map[string]string{"74207": printed(unitLine("1"))}}},
		// AS ON macos-01: a socket's unit 4 beside vmenet2, since units and interface
		// numbers are allocated apart.
		"a socket whose unit is not its interface's number": {dev: "vmenet0", n: network{mac: sharedMAC,
			tables: table, ps: psWith(), sockets: map[string]string{"74207": printed(unitLine("4"))}}},
		"softnet": {dev: "vmenet1", carrier: 62980, start: 16664085550000, n: network{mac: softMAC, tables: table,
			ps:      psWith("62978 61958 "+tartRun, "62980 62978 "+softnetRun),
			sockets: map[string]string{"74207": "true", "62980": printed(unitLine("2"))}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			h.withNetwork(t, tc.n)
			target, err := h.p.UsageTarget(t.Context(), usageVM)
			if err != nil {
				t.Fatalf("UsageTarget: %v", err)
			}
			want := provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true, NetDevice: tc.dev,
				NetHostView: true, NetPID: tc.carrier, NetPIDStart: tc.start}
			if target != want {
				t.Errorf("the VM is %+v, want %+v", target, want)
			}
		})
	}
}

// A DEVICE THAT CANNOT BE PROVED IS NOT GUESSED, and costs the network alone:
// whenever the socket and the bridge disagree, or either cannot be read, or the
// softnet carrying the VM is not proved its own, the VM is measured without its
// traffic.
func TestAVMsDeviceThatCannotBeProvedCostsItsNetworkAlone(t *testing.T) {
	t.Parallel()

	shared := func(n network) network {
		if n.mac == "" {
			n.mac = sharedMAC
		}
		if n.tables == nil {
			n.tables = table
		}
		if n.sockets == nil {
			n.sockets = map[string]string{"74207": printed(unitLine("1"))}
		}
		if n.ps == "" {
			n.ps = psWith()
		}

		return n
	}
	soft := func(rows ...string) network {
		return network{mac: softMAC, tables: table, ps: psWith(rows...),
			sockets: map[string]string{"74207": "true", "62980": printed(unitLine("2")),
				"62990": printed(unitLine("2"))}}
	}
	for name, n := range map[string]network{
		"no bridge learned the address": shared(network{mac: "02:00:00:00:00:01"}),
		"two members hold the address": shared(network{tables: map[string]string{
			"bridge100": table["bridge100"], "bridge101": printed(sharedMAC + " Vlan1 vmenet4 30 flags=0<>"),
		}}),
		"the address is on the host's own port": shared(network{tables: map[string]string{
			"bridge100": printed(sharedMAC + " Vlan1 en0 30 flags=0<>"),
		}}),
		"a table line of another shape": shared(network{tables: map[string]string{
			"bridge100": printed("sorry"),
		}}),
		"ifconfig failed": shared(network{tables: map[string]string{
			"bridge100": "echo 'ifconfig: interface bridge100 does not exist' >&2; exit 1",
		}}),
		"the configuration names no address": shared(network{mac: "none"}),
		"the socket's unit cannot be read": shared(network{
			sockets: map[string]string{"74207": printed("n[ctl com.apple.userspace_ethernet id 14 unit x]")},
		}),
		// WITH ITS OWN SOFTNET RUNNING TOO, so a process holding two is refused,
		// not passed over for the softnet.
		"the VM process holds two sockets": {mac: softMAC, tables: table,
			ps: psWith("62978 61958 "+tartRun, "62980 62978 "+softnetRun),
			sockets: map[string]string{"74207": printed(unitLine("1")) + "; " + printed(unitLine("2")),
				"62980": printed(unitLine("3"))}},
		"lsof warned": shared(network{
			sockets: map[string]string{"74207": printed(unitLine("1")) + "; echo 'lsof: WARNING: x' >&2"},
		}),
		"no softnet is running":                  soft(),
		"the softnet is another VM's":            soft("62978 61958 "+strings.Replace(tartRun, usageVM, "billet-other", 1), "62980 62978 "+softnetRun),
		"the softnet was told another address":   soft("62978 61958 "+tartRun, "62980 62978 "+strings.Replace(softnetRun, softMAC, sharedMAC, 1)),
		"the softnet was told no address":        soft("62978 61958 "+tartRun, "62980 62978 /opt/homebrew/bin/softnet --vm-fd 0"),
		"two softnets carry the VM":              soft("62978 61958 "+tartRun, "62980 62978 "+softnetRun, "62990 62978 "+softnetRun),
		"the softnet holds no socket":            {mac: softMAC, tables: table, ps: psWith("62978 61958 "+tartRun, "62980 62978 "+softnetRun), sockets: map[string]string{"74207": "true", "62980": "true"}},
		"a softnet whose parent is not tart run": soft("62978 61958 /bin/sh -c run "+usageVM, "62980 62978 "+softnetRun),
		"the softnet's start cannot be read": {mac: softMAC, tables: table,
			ps:      psWith("62978 61958 "+tartRun, "62970 62978 "+softnetRun),
			sockets: map[string]string{"74207": "true", "62970": printed(unitLine("2"))}},
		"ps answered a line billet cannot read": soft("x y z"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			h.withNetwork(t, n)
			target, err := h.p.UsageTarget(t.Context(), usageVM)
			if err != nil {
				t.Fatalf("UsageTarget refused the VM over its network: %v", err)
			}
			want := provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true}
			if target != want {
				t.Errorf("the VM is %+v, want %+v, with no device", target, want)
			}
		})
	}
}

// ONLY A BRIDGE'S NAME REACHES IFCONFIG: a name of any other shape is refused
// before anything runs.
func TestOnlyABridgesNameReachesIfconfig(t *testing.T) {
	t.Parallel()

	h := newNetworkHost(t)
	h.withNetwork(t, network{mac: sharedMAC, tables: table, ps: psWith()})
	for _, name := range []string{"-a", "bridge", "bridge1x", "en0", "bridge12345"} {
		if _, err := h.p.bridgeMembers(t.Context(), name, nil); err == nil ||
			!strings.Contains(err.Error(), "not a bridge's name") {
			t.Errorf("bridge %q answered %v, want it refused by name", name, err)
		}
	}
}

// A SOFTNET REPLACED WHILE ITS LINK IS FOUND IS NOT THE VM'S, whenever the pid
// changes hands: during the read of its socket, which its start read on either
// side catches, or between the first listing and the first start, which only
// proving it the VM's softnet again after the socket catches, since every start
// read after that is the successor's, or after it was proved again, which its
// last start read catches. The VM's network is left unmeasured
// rather than tied to whatever took the pid.
func TestASoftnetReplacedWhileItsLinkIsFoundIsNotTheVMs(t *testing.T) {
	t.Parallel()

	for _, when := range []string{"while its socket is read", "before its start is first read",
		"after it is proved again"} {
		t.Run(when, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			replaced := filepath.Join(t.TempDir(), "replaced")
			socket := printed(unitLine("2"))
			if when == "while its socket is read" {
				socket += "; : > " + replaced
			}
			h.withNetwork(t, network{mac: softMAC, tables: table,
				ps:      psWith("62978 61958 "+tartRun, "62980 62978 "+softnetRun),
				sockets: map[string]string{"74207": "true", "62980": socket}})
			// THE SUCCESSOR IS ANOTHER VM'S SOFTNET, told its own address.
			successor := psWith("62978 61958 "+tartRun, "62979 61958 "+strings.Replace(tartRun, usageVM,
				"billet-other", 1), "62980 62979 "+strings.Replace(softnetRun, softMAC, sharedMAC, 1))
			ps := filepath.Join(t.TempDir(), "ps")
			script := "#!/bin/sh\nif [ -e " + replaced + " ]; then printf '%s' '" + successor + "'; else " +
				"printf '%s' '" + psWith("62978 61958 "+tartRun, "62980 62978 "+softnetRun) + "'; fi\n"
			if err := forkSafeWriteFile(ps, []byte(script), 0o755); err != nil {
				t.Fatalf("write the ps shim: %v", err)
			}
			h.p.ps = ps
			starts := h.p.processStart
			reads := 0
			h.p.processStart = func(pid int) (uint64, error) {
				if pid != 62980 {
					return starts(pid)
				}
				reads++
				if when == "before its start is first read" || when == "after it is proved again" && reads == 2 {
					if err := os.WriteFile(replaced, nil, 0o600); err != nil {
						t.Errorf("mark the softnet replaced: %v", err)
					}
				}
				if _, err := os.Stat(replaced); err == nil {
					return 16664085559999, nil
				}

				return starts(pid)
			}

			target, err := h.p.UsageTarget(t.Context(), usageVM)
			if err != nil {
				t.Fatalf("UsageTarget: %v", err)
			}
			if want := (provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true}); target != want {
				t.Errorf("the VM is %+v, want %+v, with no device", target, want)
			}
		})
	}
}
