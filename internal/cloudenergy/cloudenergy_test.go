package cloudenergy

import (
	"strings"
	"testing"
	"time"
)

// THE MODEL IS CLOUD CARBON FOOTPRINT'S, at its own AWS figures: a vCPU draws its
// minimum at 0% and its maximum at 100%, and a job is estimated at 50%.
func TestTheModelIsTheCitedOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ u, want float64 }{{0, 0.74}, {0.5, 2.12}, {1, 3.5}} {
		if got := WattsPerVCPU(tc.u); !near(got, tc.want) {
			t.Errorf("WattsPerVCPU(%v) = %v, want %v", tc.u, got, tc.want)
		}
	}

	// Two vCPUs for ten minutes at 2.12 W each: 2,544 J.
	e, ok := Of(2, 10*time.Minute)
	if !ok {
		t.Fatal("Of(2, 10m) gave no estimate")
	}
	if e.Microjoules != 2_544_000_000 || e.VCPU != 2 || e.Duration != 10*time.Minute ||
		e.Utilization != DefaultUtilization {
		t.Errorf("Of(2, 10m) = %+v, want 2,544 J for 2 vCPU over 10m at 50%%", e)
	}
}

// A SHAPE OR A DURATION NOT RECORDED IS NO ESTIMATE, never one of zero, and
// neither is one past anything a cloud sells or GitHub runs.
func TestNoEstimateFromWhatWasNotRecorded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		vcpu int64
		d    time.Duration
	}{
		{"no vCPUs", 0, time.Minute},
		{"negative vCPUs", -1, time.Minute},
		{"more vCPUs than any shape", maxVCPU + 1, time.Minute},
		{"no duration", 2, 0},
		{"a duration that runs backwards", 2, -time.Second},
		{"a duration past a year", 2, maxDuration + time.Second},
	} {
		if e, ok := Of(tc.vcpu, tc.d); ok {
			t.Errorf("%s: Of(%d, %s) = %+v, want no estimate", tc.name, tc.vcpu, tc.d, e)
		}
	}

	// The bounds themselves are estimated, and fit.
	e, ok := Of(maxVCPU, maxDuration)
	if !ok || e.Microjoules <= 0 {
		t.Errorf("Of at the bounds = %+v, %v; want a positive estimate", e, ok)
	}

	// A duration too short to come to a microjoule has no estimate, and the
	// shortest that does has one, never zero.
	if e, ok := Of(1, time.Nanosecond); ok {
		t.Errorf("Of(1, 1ns) = %+v, want no estimate of what rounds to nothing", e)
	}
	if e, ok := Of(1, time.Millisecond); !ok || e.Microjoules != 2120 {
		t.Errorf("Of(1, 1ms) = %+v, %v; want 2,120 µJ", e, ok)
	}
}

// A DURATION UNDER A SECOND KEEPS ITS MILLISECONDS in the method, so it never
// reads as 0s.
func TestTheMethodNeverRoundsADurationToNothing(t *testing.T) {
	t.Parallel()

	e, _ := Of(1, 1500*time.Microsecond)
	if got := e.Method(); !strings.Contains(got, "1 vCPU for 2ms") {
		t.Errorf("Method() = %q, want the duration in milliseconds", got)
	}
}

// THE METHOD NEVER READS AS A MEASUREMENT: it says so first, then what the model
// was given and whose figures it used.
func TestTheMethodSaysItIsAnEstimate(t *testing.T) {
	t.Parallel()

	e, _ := Of(4, 90*time.Second)
	got := e.Method()
	for _, want := range []string{"estimated, not measured", "4 vCPU for 1m30s", "assumed 50% CPU",
		"0.74 to 3.50 W per vCPU", "Cloud Carbon Footprint's AWS averages"} {
		if !strings.Contains(got, want) {
			t.Errorf("Method() = %q, missing %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "estimated, not measured") {
		t.Errorf("Method() = %q, want it to begin by saying it is not measured", got)
	}
}

func near(a, b float64) bool {
	const epsilon = 1e-9
	return a-b < epsilon && b-a < epsilon
}
