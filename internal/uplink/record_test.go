package uplink

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSys points the package at a /sys/class/net of these interfaces and their
// indexes, and at a record of its own. NOT PARALLEL: both are package variables.
func fakeSys(t *testing.T, interfaces map[string]string) {
	t.Helper()

	dir := t.TempDir()
	for name, index := range interfaces {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, name, "ifindex"), []byte(index+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	oldNet, oldState := sysNet, StateFile
	sysNet, StateFile = dir, filepath.Join(t.TempDir(), "interface")

	t.Cleanup(func() { sysNet, StateFile = oldNet, oldState })
}

// THE RECORDED INTERFACE IS FOUND BY ITS INDEX: a device that took its name
// since is somebody else's, one renamed since is still billet's, and one with
// the index nowhere is gone.
func TestTheRecordedInterfaceIsFoundByItsIndex(t *testing.T) {
	fakeSys(t, map[string]string{"eth0": "9", "wan0": "5"})

	current, gone, err := (Record{Iface: "eth0", Index: "5", IFB: "ifb-eth0"}).Resolve()
	if err != nil || gone || current != "wan0" {
		t.Fatalf("index 5, renamed to wan0 and its old name taken by index 9, resolved to %q (gone %v, %v)",
			current, gone, err)
	}

	if _, gone, err := (Record{Iface: "eth0", Index: "7"}).Resolve(); err != nil || !gone {
		t.Fatalf("an index no interface has resolved as gone %v (%v)", gone, err)
	}
}

// A LOOKUP THAT COULD NOT TELL IS NOT ABSENCE: an interface whose index cannot
// be read may be the recorded one, renamed mid-read, and treating it as gone
// would remove the device its redirect still uses. Everything is kept.
func TestAnUndecidedLookupKeepsTheDeviceAndTheRecord(t *testing.T) {
	fakeSys(t, map[string]string{"eth0": "9"})

	if err := os.Remove(filepath.Join(sysNet, "eth0", "ifindex")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := (Record{Iface: "wan0", Index: "5"}).Resolve(); err == nil {
		t.Fatal("an interface whose index could not be read was ruled out")
	}

	if err := writeRecord(Record{Iface: "wan0", Index: "5", IFB: "ifb-wan0"}); err != nil {
		t.Fatal(err)
	}

	f := newFakeLink("wan0", "cake")
	f.ifb = true

	if err := clearRecorded(t.Context(), f.run); err == nil {
		t.Fatal("a clear that could not find the interface reported success")
	}

	if !f.ifb || len(f.changed()) != 0 {
		t.Fatalf("an undecided clear changed something:\n%s", strings.Join(f.ran, "\n"))
	}

	if _, ok := ReadRecord(); !ok {
		t.Fatal("an undecided clear forgot the record")
	}
}

// A RENAMED INTERFACE IS CLEARED UNDER ITS NEW NAME, ITS DEVICE UNDER THE OLD,
// and the device that took the old name is not touched.
func TestClearingFollowsARenameAndLeavesTheNamesNewOwnerAlone(t *testing.T) {
	fakeSys(t, map[string]string{"eth0": "9", "wan0": "5"})

	if err := writeRecord(Record{Iface: "eth0", Index: "5", IFB: "ifb-eth0"}); err != nil {
		t.Fatal(err)
	}

	f := newFakeLink("wan0", "cake")
	f.ifbName, f.handle, f.ingress, f.ifb = "ifb-eth0", "8001:", true, true

	if err := clearRecorded(t.Context(), f.run); err != nil {
		t.Fatalf("clearing a renamed interface: %v", err)
	}

	if f.root != "mq" || f.ingress || f.ifb {
		t.Fatalf("after clearing, wan0 has root %s, ingress %v, ifb-eth0 %v", f.root, f.ingress, f.ifb)
	}

	if slices.ContainsFunc(f.ran, func(line string) bool { return strings.Contains(line, "dev eth0") }) {
		t.Fatalf("the device that took the old name was touched:\n%s", strings.Join(f.ran, "\n"))
	}

	if _, ok := ReadRecord(); ok {
		t.Fatal("the record survived a clear that succeeded")
	}
}

// A GONE INTERFACE LEAVES ONLY ITS DEVICE, which is removed, and nothing is
// asked of a name another device may hold.
func TestClearingAGoneInterfaceRemovesOnlyItsDevice(t *testing.T) {
	fakeSys(t, map[string]string{"eth0": "9"})

	if err := writeRecord(Record{Iface: "eth0", Index: "5", IFB: "ifb-eth0"}); err != nil {
		t.Fatal(err)
	}

	f := newFakeLink("eth0", "htb")
	f.handle, f.ifb = "1:", true

	if err := clearRecorded(t.Context(), f.run); err != nil {
		t.Fatalf("clearing a gone interface: %v", err)
	}

	if f.ifb || f.root != "htb" {
		t.Fatalf("after clearing: ifb-eth0 %v, eth0's root %s; want the device gone and eth0 untouched", f.ifb, f.root)
	}

	if slices.ContainsFunc(f.ran, func(line string) bool { return strings.HasPrefix(line, "tc ") }) {
		t.Fatalf("tc was asked about a name another device holds:\n%s", strings.Join(f.ran, "\n"))
	}
}
