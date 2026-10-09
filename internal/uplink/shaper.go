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
// IT TOUCHES ONLY WHAT IT CAN CALL ITS OWN: a root qdisc that is the kernel's
// default or CAKE, and an ingress qdisc only beside the IFB device it names.
// Anything else on the interface is an operator's traffic policy, and the
// shaper refuses rather than replace it.
type Shaper struct {
	Iface string
	// Run runs one command and returns its combined output; nil runs it. A test
	// records instead.
	Run func(ctx context.Context, argv ...string) (string, error)
}

// defaultRoots are the root qdiscs a Linux interface has when nobody chose
// one, which the shaper may replace.
var defaultRoots = []string{"mq", "pfifo_fast", "fq_codel", "fq", "noqueue", "pfifo"}

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
	root    string
	ingress bool
	ifb     bool
}

func (s *Shaper) inspect(ctx context.Context) (state, error) {
	out, err := s.run(ctx, "tc", "qdisc", "show", "dev", s.Iface)
	if err != nil {
		return state{}, err
	}

	var st state

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "qdisc" {
			continue
		}

		switch {
		case slices.Contains(fields, "root"):
			st.root = fields[1]
		case fields[1] == "ingress":
			st.ingress = true
		}
	}

	if out, err := s.run(ctx, "ip", "-o", "link", "show", "dev", s.IFB()); err == nil {
		st.ifb = strings.Contains(out, s.IFB())
	} else if !strings.Contains(out, "does not exist") {
		return state{}, err
	}

	return st, nil
}

// owned refuses an interface carrying traffic policy the shaper did not make.
func (st state) owned(iface, ifb string) error {
	if st.root != "" && st.root != "cake" && !slices.Contains(defaultRoots, st.root) {
		return fmt.Errorf("%s's root qdisc is %s, which billet did not install; not shaping over an "+
			"operator's traffic policy", iface, st.root)
	}

	if st.ingress && !st.ifb {
		return fmt.Errorf("%s has an ingress qdisc and no %s device, so it is not billet's; not shaping "+
			"over an operator's traffic policy", iface, ifb)
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

	return st.owned(s.Iface, s.IFB())
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
// no shaping of billet's.
func (s *Shaper) Clear(ctx context.Context) error {
	st, err := s.inspect(ctx)
	if err != nil {
		return err
	}

	var errs []error

	if st.ingress && st.ifb {
		_, err := s.run(ctx, "tc", "qdisc", "del", "dev", s.Iface, "handle", "ffff:", "ingress")
		errs = append(errs, err)
	}

	if st.root == "cake" {
		_, err := s.run(ctx, "tc", "qdisc", "del", "dev", s.Iface, "root")
		errs = append(errs, err)
	}

	if st.ifb {
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
