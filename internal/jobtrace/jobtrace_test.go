package jobtrace

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A TRACE READS BACK AS IT WAS WRITTEN, the job's display name included.
func TestATraceRoundTripsWithItsJobNames(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	trace := Trace{Arrivals: []Arrival{
		{At: at, Tier: "billet-2vcpu", Owner: "acme", Repository: "api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", Job: "build (ubuntu, 1.22)",
			RunID: 7, Duration: Duration(4 * time.Minute), Result: "failed"},
		{At: at.Add(time.Minute), Tier: "billet-4vcpu", Owner: "acme", Repository: "web",
			WorkflowRef: "acme/web/.github/workflows/ci.yml@refs/heads/main", RunID: 8,
			Duration: Duration(time.Minute)},
	}}
	trace.Normalize()

	var buf bytes.Buffer
	if err := trace.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	back, err := ReadTrace(&buf)
	if err != nil {
		t.Fatalf("ReadTrace: %v", err)
	}

	if !reflect.DeepEqual(back, trace) {
		t.Errorf("the trace read back as\n%+v\nwant\n%+v", back, trace)
	}
}

// goodFleet is a fleet every rule admits.
func goodFleet() Fleet {
	return Fleet{
		Hosts: []Host{
			{Name: "host-a", VCPU: 16, Memory: 64 * config.GiB, Site: "home", Provider: config.ProviderFirecracker},
			{Name: "mac", VCPU: 8, Memory: 32 * config.GiB, Provider: config.ProviderTart},
		},
		Tiers: []Tier{
			{Label: "billet-2vcpu", VCPU: 2, Memory: 8 * config.GiB},
			{Label: "billet-macos", VCPU: 4, Memory: 16 * config.GiB, Node: "mac"},
		},
		MaxVCPU: 24, MaxMemory: 96 * config.GiB,
		Placement: config.PlacementSpread, AdmissionOrder: config.AdmissionFill,
	}
}

// A FLEET READS BACK AS IT WAS WRITTEN, every field of it.
func TestAFleetRoundTrips(t *testing.T) {
	f := goodFleet()

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}

	back, err := ReadFleet(&buf)
	if err != nil {
		t.Fatalf("ReadFleet: %v", err)
	}

	if !reflect.DeepEqual(back, f) {
		t.Errorf("the fleet read back as\n%+v\nwant\n%+v", back, f)
	}
}

// A FLEET THAT DESCRIBES NO DEPLOYMENT IS REFUSED, each by what is wrong, and a
// field the format does not know is refused rather than dropped.
func TestAFleetThatDescribesNoDeploymentIsRefused(t *testing.T) {
	for _, c := range []struct {
		what   string
		change func(*Fleet)
		want   string
	}{
		{"no hosts", func(f *Fleet) { f.Hosts = nil }, "has no hosts"},
		{"a duplicate host", func(f *Fleet) { f.Hosts[1].Name = "host-a" }, "unique name"},
		{"a host with no memory", func(f *Fleet) { f.Hosts[0].Memory = 0 }, `host "host-a" needs positive`},
		{"a duplicate tier", func(f *Fleet) { f.Tiers[1].Label = "billet-2vcpu" }, "unique label"},
		{"a tier with no vcpu", func(f *Fleet) { f.Tiers[0].VCPU = 0 }, `tier "billet-2vcpu" needs positive`},
		{"a pin to an unlisted host", func(f *Fleet) { f.Tiers[1].Node = "gone" },
			`pinned to "gone", which the fleet does not list`},
		{"no ceiling", func(f *Fleet) { f.MaxMemory = 0 }, "positive deployment ceiling"},
		{"an unknown placement", func(f *Fleet) { f.Placement = "binpack" }, "server.placement"},
		{"an unknown order", func(f *Fleet) { f.AdmissionOrder = "fifo" }, "server.admission_order"},
		{"a duplicate target", func(f *Fleet) { f.Targets = []Target{{Name: "a"}, {Name: "a"}} }, "targets[1] needs a unique name"},
		{"a target with no name", func(f *Fleet) { f.Targets = []Target{{}} }, "targets[0] needs a unique name"},
		{"a negative weight", func(f *Fleet) { f.Targets = []Target{{Name: "a", Weight: -1}} }, `target "a" has weight -1`},
		{"too large a weight", func(f *Fleet) { f.Targets = []Target{{Name: "a", Weight: config.MaxTargetWeight + 1}} },
			`target "a" has weight`},
		{"a tier of an unlisted target", func(f *Fleet) { f.Tiers[0].Target = "gone" },
			`belongs to target "gone", which the fleet does not list`},
	} {
		f := goodFleet()
		c.change(&f)

		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate gave %v; want it refused saying %q", c.what, err, c.want)
		}
	}

	good := goodFleet()
	if err := good.Validate(); err != nil {
		t.Errorf("a good fleet was refused: %v", err)
	}

	if _, err := ReadFleet(strings.NewReader(`{"hosts": [], "price": 3}`)); err == nil ||
		!strings.Contains(err.Error(), `unknown field "price"`) {
		t.Errorf("a fleet with an unknown field gave %v; want it refused naming the field", err)
	}
}
