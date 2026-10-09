package uplink

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeLink is one interface as tc and ip would report it, changed by the
// commands a Shaper runs, with every command recorded.
type fakeLink struct {
	iface, ifbName string
	root           string
	ingress, ifb   bool
	ran            []string
	fail           string
}

func newFakeLink(iface, root string) *fakeLink {
	return &fakeLink{iface: iface, ifbName: (&Shaper{Iface: iface}).IFB(), root: root}
}

func (f *fakeLink) run(_ context.Context, argv ...string) (string, error) {
	line := strings.Join(argv, " ")
	f.ran = append(f.ran, line)

	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		return "refused", errors.New("refused")
	}

	switch {
	case line == "tc qdisc show dev "+f.iface:
		out := "qdisc " + f.root + " 8001: root refcnt 2\n"
		if f.ingress {
			out += "qdisc ingress ffff: parent ffff:fff1 ----------------\n"
		}

		return out, nil
	case line == "ip -o link show dev "+f.ifbName:
		if !f.ifb {
			return `Device "` + f.ifbName + `" does not exist.`, errors.New("exit status 1")
		}

		return "9: " + f.ifbName + ": <BROADCAST,NOARP,UP,LOWER_UP>", nil
	case strings.HasPrefix(line, "tc qdisc replace dev "+f.iface+" root cake"):
		f.root = "cake"
	case line == "ip link add "+f.ifbName+" type ifb":
		f.ifb = true
	case line == "tc qdisc add dev "+f.iface+" handle ffff: ingress":
		f.ingress = true
	case line == "tc qdisc del dev "+f.iface+" handle ffff: ingress":
		f.ingress = false
	case line == "tc qdisc del dev "+f.iface+" root":
		f.root = "mq"
	case line == "ip link del "+f.ifbName:
		f.ifb = false
	}

	return "", nil
}

func (f *fakeLink) changed() []string {
	return slices.DeleteFunc(slices.Clone(f.ran), func(line string) bool {
		return strings.Contains(line, " show ") || strings.HasPrefix(line, "modprobe")
	})
}

// THE INGRESS REDIRECT IS ADDED LAST, after the device and qdisc it points at
// exist.
func TestInstallBuildsTheIngressPathBeforeRedirectingToIt(t *testing.T) {
	t.Parallel()

	f := newFakeLink("eno2np1", "mq")
	s := &Shaper{Iface: "eno2np1", Run: f.run}

	if err := s.Install(t.Context(), 400, 380); err != nil {
		t.Fatalf("install: %v", err)
	}

	index := func(prefix string) int {
		return slices.IndexFunc(f.ran, func(line string) bool { return strings.HasPrefix(line, prefix) })
	}

	device := index("ip link add ifb-eno2np1 type ifb")
	ingress := index("tc qdisc replace dev ifb-eno2np1 root cake bandwidth 380000kbit")
	redirect := index("tc filter add dev eno2np1 parent ffff: matchall action mirred egress redirect dev ifb-eno2np1")
	egress := index("tc qdisc replace dev eno2np1 root cake bandwidth 400000kbit")

	if device < 0 || ingress < device || redirect < ingress || egress < 0 || redirect != len(f.ran)-1 {
		t.Fatalf("commands ran in this order:\n%s", strings.Join(f.ran, "\n"))
	}

	if f.root != "cake" || !f.ingress || !f.ifb {
		t.Fatalf("after install the interface has root %s, ingress %v, ifb %v", f.root, f.ingress, f.ifb)
	}
}

// AN OPERATOR'S TRAFFIC POLICY IS REFUSED, NOT REPLACED: a root qdisc the kernel
// did not choose and billet did not install, or an ingress qdisc with no billet
// device behind it, and nothing on the interface is changed.
func TestAnOperatorsTrafficPolicyIsRefusedAndLeftAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		root    string
		ingress bool
		want    string
	}{
		{"a root qdisc billet did not install", "htb", false, "root qdisc is htb"},
		{"an ingress qdisc with no billet device", "fq_codel", true, "no ifb-eth0 device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeLink("eth0", tc.root)
			f.ingress = tc.ingress
			s := &Shaper{Iface: "eth0", Run: f.run}

			err := s.Install(t.Context(), 100, 100)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("install over it answered %v; want a refusal naming %q", err, tc.want)
			}

			if changed := f.changed(); len(changed) != 0 {
				t.Fatalf("a refused install still changed the interface:\n%s", strings.Join(changed, "\n"))
			}
		})
	}
}

// A HALF-BUILT SHAPER IS REMOVED: a redirect to a device that is not there would
// drop every packet the interface receives. The interface ends as it was found.
func TestAFailedInstallLeavesTheInterfaceAsItFoundIt(t *testing.T) {
	t.Parallel()

	f := newFakeLink("eno1", "mq")
	f.fail = "tc filter add"
	s := &Shaper{Iface: "eno1", Run: f.run}

	if err := s.Install(t.Context(), 100, 100); err == nil {
		t.Fatal("an install whose redirect failed reported success")
	}

	if f.root != "mq" || f.ingress || f.ifb {
		t.Fatalf("after a failed install the interface has root %s, ingress %v, ifb %v; want it as found",
			f.root, f.ingress, f.ifb)
	}
}

// CLEAR ANSWERS SUCCESS ONLY FOR AN INTERFACE IT LEFT CLEAN, and touches nothing
// on one that has nothing of billet's.
func TestClearReportsAFailedRemovalAndTouchesNothingNotItsOwn(t *testing.T) {
	t.Parallel()

	clean := newFakeLink("eno1", "fq_codel")
	if err := (&Shaper{Iface: "eno1", Run: clean.run}).Clear(t.Context()); err != nil {
		t.Fatalf("clearing an interface with nothing of billet's: %v", err)
	}

	if changed := clean.changed(); len(changed) != 0 {
		t.Fatalf("clearing an interface with nothing of billet's ran:\n%s", strings.Join(changed, "\n"))
	}

	shaped := newFakeLink("eno1", "cake")
	shaped.ingress, shaped.ifb = true, true
	shaped.fail = "ip link del"

	if err := (&Shaper{Iface: "eno1", Run: shaped.run}).Clear(t.Context()); err == nil {
		t.Fatal("a clear whose device removal failed reported success")
	}
}

// THE INGRESS DEVICE'S NAME FITS LINUX'S FIFTEEN CHARACTERS for any interface.
func TestTheIngressDeviceNameFits(t *testing.T) {
	t.Parallel()

	for _, iface := range []string{"eth0", "enp129s0f1np1", "eno1.100"} {
		if name := (&Shaper{Iface: iface}).IFB(); len(name) > 15 || !strings.HasPrefix(name, "ifb-") {
			t.Errorf("%s's ingress device is %q", iface, name)
		}
	}
}
