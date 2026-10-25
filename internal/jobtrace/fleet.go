package jobtrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// Fleet is the deployment a trace ran on: the hosts that served it, the tiers
// it was assigned to, and the limits and policies the control plane applied.
//
// WHAT A DEPLOYMENT KNOWS OF ITSELF, NOTHING ELSE. The hosts are what each
// registered (its contribution, its backend and site), the tiers and limits
// what the configuration declared; nothing here is a price or a boot time,
// which the replay harness models and says it models.
type Fleet struct {
	Hosts []Host `json:"hosts"`
	Tiers []Tier `json:"tiers"`
	// MaxVCPU and MaxMemory are the deployment ceiling, server.max_vcpu and
	// server.max_memory.
	MaxVCPU   int             `json:"max_vcpu"`
	MaxMemory config.ByteSize `json:"max_memory_bytes"`
	// Placement and AdmissionOrder are server.placement and
	// server.admission_order; empty means the default, as it does in a config.
	Placement      config.PlacementPolicy `json:"placement,omitempty"`
	AdmissionOrder config.AdmissionOrder  `json:"admission_order,omitempty"`
}

// Host is one machine of the fleet.
type Host struct {
	Name   string          `json:"name"`
	VCPU   int             `json:"vcpu"`
	Memory config.ByteSize `json:"memory_bytes"`
	// Site is where the machine is, or empty for a fleet in one place.
	Site string `json:"site,omitempty"`
	// Provider is the backend it ran; empty is a host the harness simulates
	// directly.
	Provider config.ProviderKind `json:"provider,omitempty"`
}

// Tier is one runs-on label and what a job on it is charged.
type Tier struct {
	Label  string          `json:"label"`
	VCPU   int             `json:"vcpu"`
	Memory config.ByteSize `json:"memory_bytes"`
	// Node pins the tier to one host, as the tier's own `node` does; empty
	// places it anywhere.
	Node string `json:"node,omitempty"`
}

// Validate refuses a fleet document that describes no deployment: no hosts,
// a host or tier without a unique name and a positive shape, no ceiling, a
// policy billet does not implement, or a tier pinned to a host it does not
// list.
func (f *Fleet) Validate() error {
	var errs []error

	if len(f.Hosts) == 0 {
		errs = append(errs, errors.New("jobtrace: the fleet has no hosts"))
	}

	hosts := map[string]bool{}

	for i, h := range f.Hosts {
		if strings.TrimSpace(h.Name) == "" || hosts[h.Name] {
			errs = append(errs, fmt.Errorf("jobtrace: hosts[%d] needs a unique name, got %q", i, h.Name))
		}

		hosts[h.Name] = true

		if h.VCPU <= 0 || h.Memory <= 0 {
			errs = append(errs, fmt.Errorf("jobtrace: host %q needs positive vcpu and memory", h.Name))
		}
	}

	labels := map[string]bool{}

	for i, t := range f.Tiers {
		if strings.TrimSpace(t.Label) == "" || labels[t.Label] {
			errs = append(errs, fmt.Errorf("jobtrace: tiers[%d] needs a unique label, got %q", i, t.Label))
		}

		labels[t.Label] = true

		if t.VCPU <= 0 || t.Memory <= 0 {
			errs = append(errs, fmt.Errorf("jobtrace: tier %q needs positive vcpu and memory", t.Label))
		}

		if t.Node != "" && !hosts[t.Node] {
			errs = append(errs, fmt.Errorf("jobtrace: tier %q is pinned to %q, which the fleet does not list",
				t.Label, t.Node))
		}
	}

	if f.MaxVCPU <= 0 || f.MaxMemory <= 0 {
		errs = append(errs, errors.New("jobtrace: the fleet needs a positive deployment ceiling"))
	}

	if err := f.Placement.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("jobtrace: %w", err))
	}

	if err := f.AdmissionOrder.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("jobtrace: %w", err))
	}

	return errors.Join(errs...)
}

// Tier is the tier a label names, or false for one the fleet does not declare.
func (f *Fleet) Tier(label string) (Tier, bool) {
	i := slices.IndexFunc(f.Tiers, func(t Tier) bool { return t.Label == label })
	if i < 0 {
		return Tier{}, false
	}

	return f.Tiers[i], true
}

// ReadFleet reads a fleet document and validates it.
func ReadFleet(r io.Reader) (Fleet, error) {
	var f Fleet

	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	if err := dec.Decode(&f); err != nil {
		return Fleet{}, fmt.Errorf("jobtrace: read fleet: %w", err)
	}

	if err := f.Validate(); err != nil {
		return Fleet{}, err
	}

	return f, nil
}

// Write renders the fleet as one indented JSON document, the shape ReadFleet
// reads.
func (f *Fleet) Write(w io.Writer) error {
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("jobtrace: encode fleet: %w", err)
	}

	if _, err := fmt.Fprintf(w, "%s\n", body); err != nil {
		return fmt.Errorf("jobtrace: write fleet: %w", err)
	}

	return nil
}
