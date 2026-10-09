package uplink

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Shaper drives CAKE on an interface through tc: egress on the interface's
// root, ingress on an IFB device its ingress is redirected to.
type Shaper struct {
	Iface string
	// Run runs one command; nil runs it. A test records instead.
	Run func(ctx context.Context, argv ...string) error
}

// IFB is the ingress device's name: "ifb-" and at most eleven characters of the
// interface, inside Linux's fifteen.
func (s *Shaper) IFB() string {
	name := s.Iface
	if len(name) > 11 {
		name = name[:11]
	}

	return "ifb-" + name
}

func (s *Shaper) run(ctx context.Context, argv ...string) error {
	if s.Run != nil {
		return s.Run(ctx, argv...)
	}

	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}

	return nil
}

// Install sets both directions up from nothing, clearing first whatever a
// previous run left, so an interrupted start or a crash never stacks a second
// redirect on the first.
func (s *Shaper) Install(ctx context.Context, upMbit, downMbit float64) error {
	s.Clear(ctx)

	for _, argv := range [][]string{
		{"modprobe", "sch_cake"},
		{"modprobe", "ifb", "numifbs=0"},
		append([]string{"tc", "qdisc", "replace", "dev", s.Iface, "root"}, cake(upMbit, "dual-srchost")...),
		{"ip", "link", "add", s.IFB(), "type", "ifb"},
		{"ip", "link", "set", s.IFB(), "up"},
		append([]string{"tc", "qdisc", "replace", "dev", s.IFB(), "root"}, append(cake(downMbit, "dual-dsthost"), "ingress")...),
		{"tc", "qdisc", "add", "dev", s.Iface, "handle", "ffff:", "ingress"},
		{"tc", "filter", "add", "dev", s.Iface, "parent", "ffff:", "matchall",
			"action", "mirred", "egress", "redirect", "dev", s.IFB()},
	} {
		if err := s.run(ctx, argv...); err != nil {
			// A HALF-INSTALLED SHAPER IS REMOVED, never left: a redirect with no
			// device behind it would drop every packet the interface receives.
			s.Clear(context.WithoutCancel(ctx))

			return err
		}
	}

	return nil
}

// Set changes both directions' rates in place, which CAKE applies without
// dropping its queues.
func (s *Shaper) Set(ctx context.Context, upMbit, downMbit float64) error {
	if err := s.run(ctx, append([]string{"tc", "qdisc", "change", "dev", s.Iface, "root"},
		cake(upMbit, "dual-srchost")...)...); err != nil {
		return err
	}

	return s.run(ctx, append([]string{"tc", "qdisc", "change", "dev", s.IFB(), "root"},
		append(cake(downMbit, "dual-dsthost"), "ingress")...)...)
}

// Clear removes everything Install made. Each step is attempted whatever the
// last one did, and absence is not a failure.
func (s *Shaper) Clear(ctx context.Context) {
	for _, argv := range [][]string{
		{"tc", "qdisc", "del", "dev", s.Iface, "handle", "ffff:", "ingress"},
		{"tc", "qdisc", "del", "dev", s.Iface, "root"},
		{"ip", "link", "del", s.IFB()},
	} {
		_ = s.run(ctx, argv...) //nolint:errcheck // absent is the state Clear wants
	}
}

// cake is the qdisc's arguments at a rate. The rate is in kbit so a cut below
// a whole megabit is still applied. nat makes the host-fairness see the hosts
// behind this one's masquerade, which here is every guest.
func cake(mbit float64, fairness string) []string {
	return []string{"cake", "bandwidth", strconv.FormatInt(int64(mbit*1000), 10) + "kbit",
		"besteffort", "nat", fairness}
}
