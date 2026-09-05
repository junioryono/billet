package replay

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
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
