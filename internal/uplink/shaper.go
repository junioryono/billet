package uplink

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Shaper drives CAKE on an interface through tc: egress on the interface's
// root, ingress on an IFB device its ingress is redirected to.
//
// IT TOUCHES ONLY WHAT IT CAN CALL ITS OWN. A qdisc the kernel assigned has
// handle 0:, root and children alike, and may be replaced; CAKE, an ingress
// qdisc and the ifb- device are billet's only when Owned says a run of the
// shaper recorded this interface (the record under /run/billet-uplink/, which a
// reboot empties along with them). Anything else is an operator's traffic
// policy, and the shaper refuses rather than replace it.
type Shaper struct {
	Iface string
	// Owned is whether billet's record names this interface, so the CAKE, the
	// ingress qdisc and the device on it are billet's to replace and remove.
	Owned bool
	// Run runs one command and returns its combined output; nil runs it. A test
	// records instead.
	Run func(ctx context.Context, argv ...string) (string, error)
}

// tools are the only executables the shaper runs.
var tools = []string{"tc", "ip", "modprobe"}

// IFB is the ingress device's name: "ifb-" and at most eleven characters of the
// interface, inside Linux's fifteen.
func (s *Shaper) IFB() string {
	name := s.Iface
	if len(name) > 11 {
		name = name[:11]
	}

	return "ifb-" + name
}

func (s *Shaper) run(ctx context.Context, argv ...string) (string, error) {
	if !slices.Contains(tools, argv[0]) {
		return "", fmt.Errorf("the shaper runs only %s, not %s", strings.Join(tools, ", "), argv[0])
	}

	if s.Run != nil {
		return s.Run(ctx, argv...)
	}

	//nolint:gosec // G204: argv[0] is tc, ip or modprobe (checked above) and every argument is built here; no shell
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}

	return string(out), nil
}

// state is what is on the interface now.
type state struct {
	// root is the root qdisc's kind, and kernel whether every egress qdisc on
	// the interface is one the kernel assigned (handle 0:).
	root   string
	kernel bool
	// ingress is an ingress qdisc, clsact one of the kind that also carries
	// ingress filters, and ifb whether the shaper's device exists.
	ingress, clsact, ifb bool
}

func (s *Shaper) inspect(ctx context.Context) (state, error) {
	out, err := s.run(ctx, "tc", "qdisc", "show", "dev", s.Iface)
	if err != nil {
		return state{}, err
	}

	st := state{kernel: true}

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "qdisc" {
			continue
		}

		switch fields[1] {
		case "ingress":
			st.ingress = true

			continue
		case "clsact":
			st.clsact = true

			continue
		}

		if fields[2] != "0:" {
			st.kernel = false
		}

		if slices.Contains(fields, "root") {
			st.root = fields[1]
		}
	}

	if out, err := s.run(ctx, "ip", "-o", "link", "show", "dev", s.IFB()); err == nil {
		st.ifb = strings.Contains(out, s.IFB())
	} else if !strings.Contains(out, "does not exist") {
		return state{}, err
	}

	return st, nil
}

// foreign refuses an interface carrying traffic policy the shaper did not make.
func (s *Shaper) foreign(st state) error {
	refuse := func(what string) error {
		return fmt.Errorf("%s carries %s, which billet did not install; not shaping over an operator's "+
			"traffic policy", s.Iface, what)
	}

	switch {
	case st.clsact:
		return refuse("a clsact qdisc")
	case st.root == "cake" && !s.Owned:
		return refuse("CAKE")
	case st.root != "cake" && !st.kernel:
		return refuse("a " + st.root + " root qdisc somebody configured")
	case st.ingress && !(s.Owned && st.ifb):
		return refuse("an ingress qdisc")
	case st.ifb && !s.Owned:
		return refuse("a device named " + s.IFB())
	}

	return nil
}

// Check reports whether this host can be shaped: the kernel has CAKE and IFB,
// and nothing on the interface belongs to somebody else. It changes nothing
// but loading the two modules.
func (s *Shaper) Check(ctx context.Context) error {
	for _, module := range [][]string{{"modprobe", "sch_cake"}, {"modprobe", "ifb", "numifbs=0"}} {
		if _, err := s.run(ctx, module...); err != nil {
			return fmt.Errorf("this kernel cannot shape: %w", err)
		}
	}

	st, err := s.inspect(ctx)
	if err != nil {
		return err
	}

	return s.foreign(st)
}

// Install sets both directions up, after Check and after clearing what a
// previous run of its own left, so a crash never stacks a second redirect on
// the first.
func (s *Shaper) Install(ctx context.Context, upMbit, downMbit float64) error {
	if err := s.Check(ctx); err != nil {
		return err
	}

	if err := s.Clear(ctx); err != nil {
		return err
	}

	// FROM HERE WHAT IS ON THE INTERFACE IS BILLET'S, so a failure below clears
	// what this install made.
	s.Owned = true

	for _, argv := range [][]string{
		append([]string{"tc", "qdisc", "replace", "dev", s.Iface, "root"}, cake(upMbit, "dual-srchost")...),
		{"ip", "link", "add", s.IFB(), "type", "ifb"},
		{"ip", "link", "set", s.IFB(), "up"},
		append([]string{"tc", "qdisc", "replace", "dev", s.IFB(), "root"}, append(cake(downMbit, "dual-dsthost"), "ingress")...),
		{"tc", "qdisc", "add", "dev", s.Iface, "handle", "ffff:", "ingress"},
		{"tc", "filter", "add", "dev", s.Iface, "parent", "ffff:", "matchall",
			"action", "mirred", "egress", "redirect", "dev", s.IFB()},
	} {
		if _, err := s.run(ctx, argv...); err != nil {
			// A HALF-INSTALLED SHAPER IS REMOVED, never left: a redirect with no
			// device behind it would drop every packet the interface receives.
			return errors.Join(err, s.Clear(context.WithoutCancel(ctx)))
		}
	}

	return nil
}

// Set changes both directions' rates in place, which CAKE applies without
// dropping its queues.
func (s *Shaper) Set(ctx context.Context, upMbit, downMbit float64) error {
	if _, err := s.run(ctx, append([]string{"tc", "qdisc", "change", "dev", s.Iface, "root"},
		cake(upMbit, "dual-srchost")...)...); err != nil {
		return err
	}

	_, err := s.run(ctx, append([]string{"tc", "qdisc", "change", "dev", s.IFB(), "root"},
		append(cake(downMbit, "dual-dsthost"), "ingress")...)...)

	return err
}

// Clear removes what the shaper installed and nothing else, and reports every
// removal that failed: an answer of success is a promise the interface carries
// no shaping of billet's. Without the record it removes nothing.
func (s *Shaper) Clear(ctx context.Context) error {
	if !s.Owned {
		return nil
	}

	st, err := s.inspect(ctx)
	if err != nil {
		return err
	}

	var errs []error

	// THE REDIRECT GOES FIRST, AND THE DEVICE ONLY ONCE IT HAS GONE: a redirect
	// left pointing at no device drops everything the interface receives, and a
	// retry still finds the redirect because the device is still there.
	redirected := st.ingress
	if st.ingress {
		_, err := s.run(ctx, "tc", "qdisc", "del", "dev", s.Iface, "handle", "ffff:", "ingress")
		errs = append(errs, err)
		redirected = err != nil
	}

	if st.root == "cake" {
		_, err := s.run(ctx, "tc", "qdisc", "del", "dev", s.Iface, "root")
		errs = append(errs, err)
	}

	if st.ifb && !redirected {
		_, err := s.run(ctx, "ip", "link", "del", s.IFB())
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// cake is the qdisc's arguments at a rate. The rate is in kbit so a cut below
// a whole megabit is still applied. nat makes the host-fairness see the hosts
// behind this one's masquerade, which here is every guest.
func cake(mbit float64, fairness string) []string {
	return []string{"cake", "bandwidth", strconv.FormatInt(int64(mbit*1000), 10) + "kbit",
		"besteffort", "nat", fairness}
}
