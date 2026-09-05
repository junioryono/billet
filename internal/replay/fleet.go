package replay

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/simulated"
)

// The one organization, runner group and workflow allowlist every replayed tier
// is declared under. They are the scripted GitHub's answers to the runner-group
// policy check, which the real listener and the real node both make before a
// registration is minted; a trace's per-job repository and workflow ride on
// the messages and are not checked against them, exactly as GitHub does not.
const (
	DefaultOwner = "acme"
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
	// Boot is the modelled time between a mint and the runner taking a job. Zero
	// means DefaultBoot.
	Boot time.Duration
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

	labels := map[string]bool{}

	for i, t := range f.Tiers {
		if strings.TrimSpace(t.Label) == "" || labels[t.Label] {
			errs = append(errs, fmt.Errorf("replay: tiers[%d] needs a unique label, got %q", i, t.Label))
		}

		labels[t.Label] = true

		if t.VCPU <= 0 || t.Memory <= 0 {
			errs = append(errs, fmt.Errorf("replay: tier %q needs positive vcpu and memory", t.Label))
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

// limits is the allocator's ceiling, as cmd/billet builds it from a config.
func (f Fleet) limits() alloc.Limits {
	return alloc.Limits{MaxVCPU: f.MaxVCPU, MaxMemory: f.MaxMemory}
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
		})
	}

	return out
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
