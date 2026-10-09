package uplink

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeLink is one interface as tc and ip would report it, changed by the
// commands a Shaper runs, with every command recorded. A root the kernel
// assigned has handle 0:; one somebody configured has another.
type fakeLink struct {
	iface, ifbName string
	root, handle   string
	ingress        bool
	clsact         bool
	ifb            bool
	ran            []string
	fail           string
}

func newFakeLink(iface, root string) *fakeLink {
	return &fakeLink{iface: iface, ifbName: (&Shaper{Iface: iface}).IFB(), root: root, handle: "0:"}
}

func (f *fakeLink) run(_ context.Context, argv ...string) (string, error) {
	line := strings.Join(argv, " ")
	f.ran = append(f.ran, line)

	if f.fail != "" && strings.HasPrefix(line, f.fail) {
		return "refused", errors.New("refused")
	}

	switch {
	case line == "tc qdisc show dev "+f.iface:
		out := "qdisc " + f.root + " " + f.handle + " root refcnt 2\n"
		if f.ingress {
			out += "qdisc ingress ffff: parent ffff:fff1 ----------------\n"
		}

		if f.clsact {
			out += "qdisc clsact ffff: parent ffff:fff1\n"
		}

		return out, nil
	case line == "ip -o link show dev "+f.ifbName:
		if !f.ifb {
			return `Device "` + f.ifbName + `" does not exist.`, errors.New("exit status 1")
		}

		return "9: " + f.ifbName + ": <BROADCAST,NOARP,UP,LOWER_UP>", nil
	case strings.HasPrefix(line, "tc qdisc replace dev "+f.iface+" root cake"):
		f.root, f.handle = "cake", "8001:"
	case line == "ip link add "+f.ifbName+" type ifb":
		f.ifb = true
	case line == "tc qdisc add dev "+f.iface+" handle ffff: ingress":
		f.ingress = true
	case line == "tc qdisc del dev "+f.iface+" handle ffff: ingress":
		f.ingress = false
	case line == "tc qdisc del dev "+f.iface+" root":
		f.root, f.handle = "mq", "0:"
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
// exist, on an interface carrying only what the kernel assigned.
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

// AN OPERATOR'S TRAFFIC POLICY IS REFUSED, NOT REPLACED, and nothing on the
// interface is changed: a root somebody configured, even of a default kind;
// CAKE billet has no record of installing; an ingress or clsact qdisc; a device
// named like billet's with no record behind it.
func TestAnOperatorsTrafficPolicyIsRefusedAndLeftAlone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		setup func(f *fakeLink)
		want  string
	}{
		{"a root qdisc billet did not install", func(f *fakeLink) { f.root, f.handle = "htb", "1:" }, "htb root qdisc"},
		{"fq_codel somebody configured", func(f *fakeLink) { f.root, f.handle = "fq_codel", "8002:" }, "fq_codel root"},
		{"CAKE with no record of billet's", func(f *fakeLink) { f.root, f.handle = "cake", "8001:" }, "carries CAKE"},
		{"an ingress qdisc", func(f *fakeLink) { f.ingress = true }, "an ingress qdisc"},
		{"a clsact qdisc", func(f *fakeLink) { f.clsact = true }, "a clsact qdisc"},
		{"a device named like billet's", func(f *fakeLink) { f.ifb = true }, "a device named ifb-eth0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeLink("eth0", "mq")
			tc.setup(f)
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

// WHAT BILLET'S RECORD SAYS IS BILLET'S IS REPLACED: a crashed run's CAKE,
// redirect and device are cleared and installed afresh.
func TestARecordedRunsLeftoversAreReplaced(t *testing.T) {
	t.Parallel()

	f := newFakeLink("eno1", "cake")
	f.handle, f.ingress, f.ifb = "8001:", true, true
	s := &Shaper{Iface: "eno1", Owned: true, Run: f.run}

	if err := s.Install(t.Context(), 100, 100); err != nil {
		t.Fatalf("installing over a recorded run's leftovers: %v", err)
	}

	if !slices.Contains(f.ran, "ip link del ifb-eno1") {
		t.Fatalf("the leftovers were not cleared first:\n%s", strings.Join(f.ran, "\n"))
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

// CLEAR TOUCHES NOTHING WITHOUT THE RECORD, AND A REDIRECT IT COULD NOT REMOVE
// KEEPS ITS DEVICE, so the retry finds both and finishes.
func TestClearKeepsTheDeviceWhileTheRedirectRemains(t *testing.T) {
	t.Parallel()

	stranger := newFakeLink("eno1", "cake")
	stranger.handle, stranger.ingress, stranger.ifb = "8001:", true, true

	if err := (&Shaper{Iface: "eno1", Run: stranger.run}).Clear(t.Context()); err != nil {
		t.Fatalf("clearing with no record: %v", err)
	}

	if changed := stranger.changed(); len(changed) != 0 {
		t.Fatalf("clearing with no record changed the interface:\n%s", strings.Join(changed, "\n"))
	}

	f := newFakeLink("eno1", "cake")
	f.handle, f.ingress, f.ifb = "8001:", true, true
	f.fail = "tc qdisc del dev eno1 handle ffff: ingress"
	s := &Shaper{Iface: "eno1", Owned: true, Run: f.run}

	if err := s.Clear(t.Context()); err == nil {
		t.Fatal("a clear whose redirect removal failed reported success")
	}

	if !f.ingress || !f.ifb {
		t.Fatalf("after the redirect's removal failed: ingress %v, device %v; the device must stay with it",
			f.ingress, f.ifb)
	}

	f.fail = ""

	if err := s.Clear(t.Context()); err != nil {
		t.Fatalf("the retry: %v", err)
	}

	if f.root != "mq" || f.ingress || f.ifb {
		t.Fatalf("after the retry the interface has root %s, ingress %v, ifb %v", f.root, f.ingress, f.ifb)
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
