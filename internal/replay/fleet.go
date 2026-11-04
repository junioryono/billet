package replay

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/jobtrace"
	"github.com/junioryono/billet/internal/provider/simulated"
)

// The one organization, runner group and workflow allowlist every replayed tier
// is declared under. They are the scripted GitHub's answers to the runner-group
// policy check, which the real listener and the real node both make before a
// registration is minted; a trace's per-job repository and workflow ride on
// the messages and are not checked against them, exactly as GitHub does not.
const (
	DefaultOwner = jobtrace.DefaultOwner
	RunnerGroup  = "billet-replay"
	Workflow     = "acme/replay/.github/workflows/ci.yml@refs/heads/main"

	// DefaultBoot is how long a minted runner takes to register with GitHub and
	// be handed a job, when a fleet does not say. It is the harness's model of
	// something billet does not decide, and the report names it as such.
	DefaultBoot = 30 * time.Second
)

// Host is one simulated machine in the fleet.
type Host struct {
	Name   string
	VCPU   int
	Memory config.ByteSize
	// Site is where the machine is, or empty for a fleet in one place.
	Site string
	// RatePerHour is what running this machine costs per hour, in dollars. The
	// harness's input, like the trace: an owned box's rate is not a fact billet
	// records. Zero means unpriced.
	RatePerHour float64
}

// TierShape is one runs-on label and what a job on it is charged.
type TierShape struct {
	Label  string
	VCPU   int
	Memory config.ByteSize
	// Node pins the tier to one host, as a tier's `node` does; empty places it
	// anywhere.
	Node string
	// Target is the target the tier belongs to, one Fleet.Targets names; empty
	// in a fleet that declares none.
	Target string
	// Reserved is the tier's `reserved` floor.
	Reserved int
}

// TargetShape is one GitHub target of the fleet.
type TargetShape struct {
	Name string
	// Weight is the target's `weight`; zero is none written.
	Weight int
}

// Fleet is the deployment a trace is replayed against.
type Fleet struct {
	Hosts []Host
	Tiers []TierShape
	// MaxVCPU and MaxMemory are the deployment ceiling, server.max_vcpu and
	// server.max_memory.
	MaxVCPU   int
	MaxMemory config.ByteSize
	// Placement is server.placement; empty means pack, as it does in a config.
	Placement config.PlacementPolicy
	// AdmissionOrder is server.admission_order; empty means fair, as it does in
	// a config.
	AdmissionOrder config.AdmissionOrder
	// Targets are the targets the tiers belong to, the first of them the
	// `github` block and so named config.DefaultTargetName, as in a config. Empty
	// is one target nothing says anything about.
	Targets []TargetShape
	// Boot is the modelled time between a mint and the runner taking a job. Zero
	// means DefaultBoot.
	Boot time.Duration
}

// ownerOf is the GitHub organization a target of the fleet serves.
func ownerOf(target string) string {
	if target == config.DefaultTargetName {
		return DefaultOwner
	}

	return target
}

// validate refuses a fleet the trace cannot be replayed against.
func (f Fleet) validate(trace Trace) error {
	var errs []error

	if len(f.Hosts) == 0 {
		errs = append(errs, errors.New("replay: the fleet has no hosts"))
	}

	seen := map[string]bool{}

	for i, h := range f.Hosts {
		if strings.TrimSpace(h.Name) == "" || seen[h.Name] {
			errs = append(errs, fmt.Errorf("replay: hosts[%d] needs a unique name, got %q", i, h.Name))
		}

		seen[h.Name] = true

		if h.VCPU <= 0 || h.Memory <= 0 {
			errs = append(errs, fmt.Errorf("replay: host %q needs positive vcpu and memory", h.Name))
		}
	}

	if f.MaxVCPU <= 0 || f.MaxMemory <= 0 {
		errs = append(errs, errors.New("replay: the fleet needs a positive deployment ceiling"))
	}

	if err := f.Placement.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("replay: %w", err))
	}

	if err := f.AdmissionOrder.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("replay: %w", err))
	}

	targets := map[string]bool{}

	for i, target := range f.Targets {
		if (i == 0) != (target.Name == config.DefaultTargetName) || targets[target.Name] {
			errs = append(errs, fmt.Errorf("replay: targets[%d] is named %q; the first and only the first target "+
				"is %q, and each has a unique name", i, target.Name, config.DefaultTargetName))
		}

		targets[target.Name] = true
	}

	labels := map[string]bool{}

	for i, t := range f.Tiers {
		// EVERY TIER OF A DECLARED SET OF TARGETS NAMES ONE, as a config requires
		// of several; the server cannot resolve an empty one among them.
		if len(f.Targets) > 0 && !targets[t.Target] || len(f.Targets) == 0 && t.Target != "" {
			errs = append(errs, fmt.Errorf("replay: tier %q belongs to target %q, which the fleet does not declare",
				t.Label, t.Target))
		}

		if strings.TrimSpace(t.Label) == "" || labels[t.Label] {
			errs = append(errs, fmt.Errorf("replay: tiers[%d] needs a unique label, got %q", i, t.Label))
		}

		labels[t.Label] = true

		if t.VCPU <= 0 || t.Memory <= 0 {
			errs = append(errs, fmt.Errorf("replay: tier %q needs positive vcpu and memory", t.Label))
		}

		if t.Node != "" && !seen[t.Node] {
			errs = append(errs, fmt.Errorf("replay: tier %q is pinned to %q, which the fleet does not declare",
				t.Label, t.Node))
		}
	}

	for _, label := range trace.Tiers() {
		if !labels[label] {
			errs = append(errs, fmt.Errorf("replay: the trace uses tier %q, which the fleet does not declare", label))
		}
	}

	return errors.Join(errs...)
}

// boot is the modelled registration time.
func (f Fleet) boot() time.Duration {
	if f.Boot > 0 {
		return f.Boot
	}

	return DefaultBoot
}

// tiers renders the fleet's shapes as the catalogue alloc.New and the plane
// read, on the simulated backend.
//
// BUILT IN CODE, NOT PARSED FROM YAML, because config.Load refuses the simulated
// kind anywhere a file names it; alloc.New re-applies every per-tier rule Load
// would, so the catalogue is still validated the way a deployment's is.
//
// THE COMMAND OUTLASTS EVERY JOB ON THE TIER. The simulated backend reports an
// instance stopped once its command's duration elapses, and in billet's model a
// job ends when GitHub says it did, not when the runner process does; a command
// shorter than a job would model a runner dying under it. So the command is the
// tier's longest job plus the boot model plus an hour, and the completion's
// destroy is what removes every instance.
func (f Fleet) tiers(trace Trace) []config.Tier {
	out := make([]config.Tier, 0, len(f.Tiers))

	for _, shape := range f.Tiers {
		out = append(out, config.Tier{
			Label:       shape.Label,
			Provider:    config.ProviderSimulated,
			VCPU:        shape.VCPU,
			Memory:      shape.Memory,
			Image:       "simulated",
			RunnerGroup: RunnerGroup,
			GuestOS:     config.GuestLinux,
			Trust:       config.WorkloadTrusted,
			Workflows:   []string{Workflow},
			Command:     simulated.RunFor(trace.LongestDuration(shape.Label) + f.boot() + time.Hour),
			Node:        shape.Node,
			Target:      shape.Target,
			Reserved:    shape.Reserved,
		})
	}

	return out
}

// FleetFromDocument is the fleet a trace's document describes (billet jobs
// export writes one), ready to replay against: every host simulated, every
// tier on the simulated backend at its charged shape. It returns the hosts it
// left out, by name, for the replay's record to say.
//
// A REMOTE BACKEND IS LEFT OUT. The simulated backend runs work on a host it
// holds, so an EC2 or CodeBuild node, whose capacity is a catalogue of shapes
// bought per job, cannot be modelled as one, and a replay that pretended to
// would measure a fleet the deployment never had; the jobs it served are
// replayed on the hosts that remain. A tier pinned to a host left out cannot
// be replayed at all, and is refused by name.
func FleetFromDocument(doc jobtrace.Fleet) (Fleet, []string, error) {
	if err := doc.Validate(); err != nil {
		return Fleet{}, nil, err
	}

	f := Fleet{
		MaxVCPU: doc.MaxVCPU, MaxMemory: doc.MaxMemory, Placement: doc.Placement,
		AdmissionOrder: doc.AdmissionOrder,
	}

	for _, t := range doc.Targets {
		f.Targets = append(f.Targets, TargetShape{Name: t.Name, Weight: t.Weight})
	}

	var dropped []string

	for _, h := range doc.Hosts {
		if h.Provider != "" && !h.Provider.RunsOnHost() {
			dropped = append(dropped, fmt.Sprintf("%s (%s)", h.Name, h.Provider))

			continue
		}

		f.Hosts = append(f.Hosts, Host{Name: h.Name, VCPU: h.VCPU, Memory: h.Memory, Site: h.Site})
	}

	var errs []error

	for _, t := range doc.Tiers {
		if t.Node != "" && !slices.ContainsFunc(f.Hosts, func(h Host) bool { return h.Name == t.Node }) {
			errs = append(errs, fmt.Errorf("replay: tier %q is pinned to %q, a remote host the simulated backend "+
				"cannot model", t.Label, t.Node))

			continue
		}

		f.Tiers = append(f.Tiers, TierShape{Label: t.Label, VCPU: t.VCPU, Memory: t.Memory, Node: t.Node,
			Target: t.Target})
	}

	if err := errors.Join(errs...); err != nil {
		return Fleet{}, nil, err
	}

	if len(f.Hosts) == 0 {
		return Fleet{}, nil, fmt.Errorf("replay: every host of the fleet is remote (%v), and the simulated "+
			"backend cannot model one", dropped)
	}

	return f, dropped, nil
}

// cost prices one record, and says which rate did it.
//
// THE LEDGER'S PRICE FIRST. A positive recorded price is the rate the shape was
// charged at when the lease was escrowed, which is the number a cost report has
// to use. A host-backed lease records none, and there the fleet's per-host rate
// is the harness's input, applied to the charged vCPU-hours as a share of the
// host; it is labelled so a reader never mistakes it for a fact billet recorded.
func (f Fleet) cost(rec *Record) (float64, string) {
	hours := time.Duration(rec.RunDuration).Hours()
	if hours <= 0 {
		return 0, ""
	}

	if rec.PriceMicrosPerHour > 0 {
		return float64(rec.PriceMicrosPerHour) / 1e6 * hours, "ledger"
	}

	h, ok := f.host(rec.Node)
	if !ok || h.RatePerHour <= 0 || h.VCPU <= 0 {
		return 0, ""
	}

	return h.RatePerHour * hours * float64(rec.VCPU) / float64(h.VCPU), "fleet-rate"
}

// shape is what a job on a tier is charged, or false for a label the fleet does
// not declare.
func (f Fleet) shape(label string) (TierShape, bool) {
	i := slices.IndexFunc(f.Tiers, func(t TierShape) bool { return t.Label == label })
	if i < 0 {
		return TierShape{}, false
	}

	return f.Tiers[i], true
}

// host is a machine by name.
func (f Fleet) host(name string) (Host, bool) {
	i := slices.IndexFunc(f.Hosts, func(h Host) bool { return h.Name == name })
	if i < 0 {
		return Host{}, false
	}

	return f.Hosts[i], true
}
