package uplink

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// recorder records each command a Shaper runs and fails the one named.
type recorder struct {
	ran  []string
	fail string
}

func (r *recorder) run(_ context.Context, argv ...string) error {
	line := strings.Join(argv, " ")
	r.ran = append(r.ran, line)

	if r.fail != "" && strings.Contains(line, r.fail) {
		return errors.New("refused")
	}

	return nil
}

// THE INGRESS REDIRECT IS ADDED LAST, after the device and qdisc it points at
// exist, and an install clears what a crashed run left before it starts.
func TestInstallBuildsTheIngressPathBeforeRedirectingToIt(t *testing.T) {
	t.Parallel()

	r := &recorder{}
	s := &Shaper{Iface: "eno2np1", Run: r.run}

	if err := s.Install(t.Context(), 400, 380); err != nil {
		t.Fatalf("install: %v", err)
	}

	index := func(prefix string) int {
		return slices.IndexFunc(r.ran, func(line string) bool { return strings.HasPrefix(line, prefix) })
	}

	cleared := index("tc qdisc del dev eno2np1 handle ffff: ingress")
	device := index("ip link add ifb-eno2np1 type ifb")
	ingress := index("tc qdisc replace dev ifb-eno2np1 root cake bandwidth 380000kbit")
	redirect := index("tc filter add dev eno2np1 parent ffff: matchall action mirred egress redirect dev ifb-eno2np1")
	egress := index("tc qdisc replace dev eno2np1 root cake bandwidth 400000kbit")

	if cleared != 0 || device < 0 || ingress < device || redirect < ingress || egress < 0 ||
		redirect != len(r.ran)-1 {
		t.Fatalf("commands ran in this order:\n%s", strings.Join(r.ran, "\n"))
	}
}

// A HALF-BUILT SHAPER IS REMOVED: a redirect to a device that is not there would
// drop every packet the interface receives.
func TestAFailedInstallRemovesWhatItBuilt(t *testing.T) {
	t.Parallel()

	r := &recorder{fail: "tc filter add"}
	s := &Shaper{Iface: "eno1", Run: r.run}

	if err := s.Install(t.Context(), 100, 100); err == nil {
		t.Fatal("an install whose redirect failed reported success")
	}

	tail := r.ran[len(r.ran)-3:]
	if !strings.HasPrefix(tail[0], "tc qdisc del dev eno1 handle ffff: ingress") ||
		!strings.HasPrefix(tail[1], "tc qdisc del dev eno1 root") || tail[2] != "ip link del ifb-eno1" {
		t.Fatalf("after the failure ran:\n%s", strings.Join(r.ran, "\n"))
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
