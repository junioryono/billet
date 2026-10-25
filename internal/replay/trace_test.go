package replay

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// A trace survives a round trip through its file format unchanged.
func TestATraceRoundTripsThroughItsFileFormat(t *testing.T) {
	t.Parallel()

	trace := LongTail(9, Params{Jobs: 50})

	var buf bytes.Buffer
	if err := trace.Write(&buf); err != nil {
		t.Fatal(err)
	}

	read, err := ReadTrace(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(read, trace) {
		t.Fatalf("the trace changed on the way through its file:\nwrote %+v\nread  %+v",
			trace.Arrivals[:2], read.Arrivals[:2])
	}
}

// A file the exporter writes is read: absolute times, Go durations, GitHub's own
// result, arrivals in any order.
func TestATraceFileIsNormalizedOnRead(t *testing.T) {
	t.Parallel()

	file := strings.Join([]string{
		`{"at":"2026-03-02T09:05:00Z","tier":"billet-4vcpu","repository":"api","workflow":"acme/api/.github/workflows/ci.yml@refs/heads/main","run_id":2,"duration":"245s","result":"succeeded"}`,
		``,
		`{"at":"2026-03-02T09:00:00Z","tier":"billet-2vcpu","repository":"web","workflow":"acme/web/.github/workflows/ci.yml@refs/heads/main","run_id":1,"duration":"1m"}`,
	}, "\n")

	trace, err := ReadTrace(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}

	if len(trace.Arrivals) != 2 {
		t.Fatalf("read %d arrivals, want 2", len(trace.Arrivals))
	}

	first := trace.Arrivals[0]
	if first.Seq != 1 || first.Repository != "web" || first.Owner != DefaultOwner || first.Result != ResultSucceeded {
		t.Errorf("the earlier arrival was not normalized first with defaults filled: %+v", first)
	}

	if got := time.Duration(trace.Arrivals[1].Duration); got != 245*time.Second {
		t.Errorf("duration read as %s, want 245s", got)
	}
}

// A trace the replay could not carry faithfully is refused, naming the line.
func TestAnUnreplayableTraceIsRefused(t *testing.T) {
	t.Parallel()

	for name, line := range map[string]string{
		"no duration":   `{"at":"2026-03-02T09:00:00Z","tier":"t","repository":"r","workflow":"w"}`,
		"no tier":       `{"at":"2026-03-02T09:00:00Z","repository":"r","workflow":"w","duration":"1m"}`,
		"no repository": `{"at":"2026-03-02T09:00:00Z","tier":"t","workflow":"w","duration":"1m"}`,
		"bad duration":  `{"at":"2026-03-02T09:00:00Z","tier":"t","repository":"r","workflow":"w","duration":"soon"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := ReadTrace(strings.NewReader(line)); err == nil {
				t.Fatalf("a trace with %s was accepted", name)
			}
		})
	}

	if _, err := ReadTrace(strings.NewReader("")); err == nil {
		t.Fatal("an empty trace was accepted")
	}
}

// THE SAME SEED IS THE SAME TRACE, and a different seed is a different one, for
// every generator. A comparison between two policies runs one trace twice; a
// generator that varied between calls would compare two samples.
func TestAGeneratorIsDeterministicPerSeed(t *testing.T) {
	t.Parallel()

	for name, gen := range map[string]func(uint64, Params) Trace{
		"morning burst":    MorningBurst,
		"monorepo fan-out": MonorepoFanOut,
		"long tail":        LongTail,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			a, b := gen(42, Params{Jobs: 100}), gen(42, Params{Jobs: 100})
			if !reflect.DeepEqual(a, b) {
				t.Fatal("two traces from one seed differ")
			}

			other := gen(43, Params{Jobs: 100})
			if reflect.DeepEqual(a, other) {
				t.Fatal("two seeds produced the same trace")
			}

			if err := a.Validate(); err != nil {
				t.Fatalf("the generated trace is not replayable: %v", err)
			}

			if len(a.Arrivals) != 100 {
				t.Fatalf("generated %d arrivals, want 100", len(a.Arrivals))
			}
		})
	}
}

// Each generator has the shape it is named for.
func TestGeneratorsHaveTheirShapes(t *testing.T) {
	t.Parallel()

	burst := MorningBurst(1, Params{Jobs: 1000})
	inFirstHour := 0

	for _, a := range burst.Arrivals {
		if a.At.Before(burst.Arrivals[0].At.Add(time.Hour)) {
			inFirstHour++
		}
	}

	if inFirstHour < 600 || inFirstHour > 800 {
		t.Errorf("morning burst put %d of 1000 jobs in the first hour, want about 700", inFirstHour)
	}

	fan := MonorepoFanOut(1, Params{Jobs: 300})
	runs := map[int64]int{}

	for _, a := range fan.Arrivals {
		runs[a.RunID]++

		if a.Repository != fan.Arrivals[0].Repository {
			t.Fatalf("a monorepo fan-out named a second repository %q", a.Repository)
		}
	}

	for run, n := range runs {
		if n < 2 {
			t.Errorf("run %d has %d job; a fan-out has many per run", run, n)
		}
	}

	tail := LongTail(1, Params{Jobs: 1000})
	long := 0

	for _, a := range tail.Arrivals {
		if time.Duration(a.Duration) >= 2*time.Hour {
			long++
		}
	}

	if long == 0 || long > 100 {
		t.Errorf("long tail has %d multi-hour jobs of 1000, want a few", long)
	}
}

// A fleet that does not declare a tier the trace uses is refused before
// anything is stood up.
func TestAFleetMustDeclareEveryTierTheTraceUses(t *testing.T) {
	t.Parallel()

	trace := MorningBurst(1, Params{Jobs: 10, Tiers: []string{"billet-96vcpu"}})

	fleet := Fleet{
		Hosts:     []Host{{Name: "a", VCPU: 8, Memory: 1 << 34}},
		Tiers:     []TierShape{{Label: "billet-2vcpu", VCPU: 2, Memory: 1 << 32}},
		MaxVCPU:   8,
		MaxMemory: 1 << 34,
	}

	err := fleet.validate(trace)
	if err == nil || !strings.Contains(err.Error(), "billet-96vcpu") {
		t.Fatalf("a fleet without the trace's tier was accepted, or the refusal did not name it: %v", err)
	}
}

// THE ADVERSARIAL GENERATORS HAVE THEIR SHAPES, and one seed is one trace.
func TestTheAdversarialGeneratorsHaveTheirShapes(t *testing.T) {
	t.Parallel()

	within := func(name string, trace Trace, d time.Duration) {
		t.Helper()

		first, last := trace.Arrivals[0].At, trace.Arrivals[len(trace.Arrivals)-1].At
		if last.Sub(first) > d {
			t.Errorf("%s spreads its jobs over %s; want them within %s", name, last.Sub(first), d)
		}
	}

	within("Burst", Burst(1, Params{Jobs: 50}), 10*time.Second)
	within("IdleBacklog", IdleBacklog(1, Params{Jobs: 50}), 30*time.Second)

	// THE QUIET TIER ARRIVES INSIDE THE BUSY ONE'S SPAN, and the busy one
	// offers more than the fleet holds.
	small := Fleet{
		Hosts: []Host{{Name: "only", VCPU: 16, Memory: 64 * config.GiB}},
		Tiers: []TierShape{{Label: "busy", VCPU: 2, Memory: 8 * config.GiB},
			{Label: "quiet", VCPU: 4, Memory: 16 * config.GiB}},
		MaxVCPU: 16, MaxMemory: 64 * config.GiB,
	}
	quiet := QuietBesideSaturated(1, QuietParams{Params: Params{Jobs: 200}, Fleet: small})
	counts := map[string]int{}

	var busyAsked float64

	var busyLast time.Time

	for _, a := range quiet.Arrivals {
		counts[a.Tier]++

		if a.Tier == "busy" {
			busyAsked += 2 * time.Duration(a.Duration).Seconds()
			busyLast = a.At
		}
	}

	for _, a := range quiet.Arrivals {
		if a.Tier == "quiet" && a.At.After(busyLast) {
			t.Errorf("a quiet job arrives at %s, after the busy stream's last at %s", a.At, busyLast)
		}
	}

	if counts["busy"] != 180 || counts["quiet"] != 20 {
		t.Errorf("QuietBesideSaturated put %v on its tiers; want 180 busy and 20 quiet", counts)
	}

	if load := busyAsked / (16 * busyLast.Sub(quiet.Arrivals[0].At).Seconds()); load < 1.2 {
		t.Errorf("the busy tier offers a load of %.2f of the room; want it saturated, about 1.5", load)
	}

	// THE WIDE MIX OFFERS ABOUT ITS LOAD: the vCPU-time its jobs ask for over
	// the room times the span they arrive in, near 0.85, and about one job in
	// twenty of the whole-host shape.
	fleet := WideFleet()
	wide := WideShapeMix(1, WideParams{Params: Params{Jobs: 2000}, Fleet: fleet})

	vcpuOf := map[string]int{}
	for _, s := range fleet.Tiers {
		vcpuOf[s.Label] = s.VCPU
	}

	asked, widest := 0.0, 0
	for _, a := range wide.Arrivals {
		asked += float64(vcpuOf[a.Tier]) * time.Duration(a.Duration).Seconds()
		if a.Tier == "billet-32vcpu" {
			widest++
		}
	}

	span := wide.Arrivals[len(wide.Arrivals)-1].At.Sub(wide.Arrivals[0].At).Seconds()
	if load := asked / (128 * span); load < 0.7 || load > 1.0 {
		t.Errorf("WideShapeMix offers a load of %.2f; want about 0.85", load)
	}

	if share := float64(widest) / 2000; share < 0.03 || share > 0.07 {
		t.Errorf("WideShapeMix made %.3f of its jobs whole-host; want about 0.05", share)
	}

	// A FLEET OF ONE TIER IS ALL WIDE.
	one := Fleet{Hosts: fleet.Hosts, Tiers: fleet.Tiers[4:], MaxVCPU: fleet.MaxVCPU, MaxMemory: fleet.MaxMemory}
	for _, a := range WideShapeMix(2, WideParams{Params: Params{Jobs: 20}, Fleet: one}).Arrivals {
		if a.Tier != "billet-32vcpu" {
			t.Errorf("a one-tier wide mix put a job on %q", a.Tier)
		}
	}

	for name, gen := range map[string]func(uint64) Trace{
		"Burst":       func(s uint64) Trace { return Burst(s, Params{Jobs: 20}) },
		"IdleBacklog": func(s uint64) Trace { return IdleBacklog(s, Params{Jobs: 20}) },
		"QuietBesideSaturated": func(s uint64) Trace {
			return QuietBesideSaturated(s, QuietParams{Params: Params{Jobs: 20}, Fleet: small})
		},
		"WideShapeMix": func(s uint64) Trace {
			return WideShapeMix(s, WideParams{Params: Params{Jobs: 20}, Fleet: fleet})
		},
	} {
		if !reflect.DeepEqual(gen(3), gen(3)) || reflect.DeepEqual(gen(3), gen(4)) {
			t.Errorf("%s is not one trace per seed", name)
		}
	}
}
