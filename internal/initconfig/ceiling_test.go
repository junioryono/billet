package initconfig

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// referenceHostMemory is what DetectHostCapacity actually reported on the
// reference host — the nominally-512GiB machine in
// docs/reference/reference-hardware.md.
// The awkward figure is the point: real memory is never a round number, and the
// rounding exists because of what this value renders as without it.
const referenceHostMemory = 523505880 * config.KiB

// THE RESERVATION SCALES, because a fixed one means different things at
// different sizes.
//
// Two vCPU is a quarter of an eight-thread laptop and 1.5% of a 128-thread
// server — and the server is the one carrying sixty guests, whose supervision,
// networking and storage clients are what the reservation is for.
//
// 120 is not an arbitrary expectation: it is the number the reference host's
// operator had already reached by hand, in an inventory written before this
// rule existed. It is the only corroboration available for a value nothing can
// measure — and the memory table below independently reproduces that same
// inventory's 480GiB from the host's nominal size.
func TestTheCeilingScalesWithTheMachine(t *testing.T) {
	if got := CeilingVCPU(128); got != 120 {
		t.Errorf("a 128-thread host generates a ceiling of %d, want 120", got)
	}
	if got := CeilingVCPU(64); got != 60 {
		t.Errorf("a 64-thread host generates a ceiling of %d, want 60", got)
	}

	// FAR ENOUGH OUT THAT A CAPPED RESERVATION SHOWS. A share capped at eight
	// threads matches every anchor through 128 and only diverges above it: 256
	// would keep 248 instead of 240. Nothing else here can see that.
	if got := CeilingVCPU(256); got != 240 {
		t.Errorf("a 256-thread host generates a ceiling of %d, want 240", got)
	}

	// AND OFF A MULTIPLE OF THE DIVISOR, because every other anchor here divides
	// by 16 exactly. Rounding the share UP instead of down — (detected+15)/16 —
	// matches all of them while withholding one more thread than it should, and
	// shows only where the division has a remainder.
	if got := CeilingVCPU(65); got != 61 {
		t.Errorf("a 65-thread host generates a ceiling of %d, want 61", got)
	}

	// AND INSIDE THE BAND WHERE THE SHARE HAS OVERTAKEN THE FLOOR BUT THE OTHER
	// ANCHORS CANNOT SEE IT. A rule that kept the flat floor until 64 threads
	// matches 4, 8, 16, 32, 64, 65, 128 and 256 — and withholds 2 instead of 3
	// from a 48-thread host, which only this says.
	if got := CeilingVCPU(48); got != 45 {
		t.Errorf("a 48-thread host generates a ceiling of %d, want 45", got)
	}

	// THE SMALL END, EXACTLY. Handing a two-thread host its whole machine — a
	// reservation that gives up rather than withholding one of two — passes
	// every anchor above, and withholds nothing from the kernel on the machine
	// least able to spare it.
	for detected, want := range map[int]int{1: 1, 2: 1, 3: 1} {
		if got := CeilingVCPU(detected); got != want {
			t.Errorf("a %d-thread host generates a ceiling of %d, want %d",
				detected, got, want)
		}
	}

	// AND THE FAR END, where a reservation capped at sixteen threads still
	// matches every anchor through 256 and first diverges here.
	if got := CeilingVCPU(512); got != 480 {
		t.Errorf("a 512-thread host generates a ceiling of %d, want 480", got)
	}
}

// AND BOTH DIRECTIONS: the floor still governs a small machine.
//
// A proportional rule alone would withhold nothing from a four-thread host
// (4/16 == 0), generating a ceiling equal to the whole machine — the exact
// overcommit the headroom exists to prevent. Tested because a change that drops
// the floor passes the scaling test above unchanged.
func TestTheCeilingKeepsTheFloorOnASmallMachine(t *testing.T) {
	for detected, want := range map[int]int{
		4:  2, // 4/16 == 0; only the floor withholds anything
		8:  6,
		16: 14, // 16/16 == 1, still under the floor
		32: 30, // 32/16 == 2, exactly the floor
	} {
		if got := CeilingVCPU(detected); got != want {
			t.Errorf("a %d-thread host generates a ceiling of %d, want %d", detected, got, want)
		}
	}
}

// THE CEILING IS RENDERED INTO THE FILE THE OPERATOR REVIEWS.
//
// ByteSize prints only the unit that divides it exactly, so an unrounded
// ceiling reaches the config as `519311576KiB` — a number that cannot be
// compared at a glance to the `8GiB` and `16GiB` tiers directly beneath it, in
// the one field whose entire purpose is to be read and raised.
//
// Exact expectations, because a suffix check alone is satisfied by a function
// that returns a constant 1GiB and ignores the machine entirely.
func TestTheMemoryCeilingRendersInGiB(t *testing.T) {
	for name, tc := range map[string]struct {
		detected, want config.ByteSize
	}{
		"the reference host as measured": {referenceHostMemory, 468 * config.GiB},
		"the reference host as sold":     {512 * config.GiB, 480 * config.GiB},
		"an awkward 32GiB":               {32*config.GiB - 7*config.MiB, 27 * config.GiB},
		"a round 64GiB":                  {64 * config.GiB, 60 * config.GiB},
		"the smallest that can boot":     {2 * config.GiB, 1 * config.GiB},
		"where the half-cap gives way":   {8 * config.GiB, 4 * config.GiB},
	} {
		t.Run(name, func(t *testing.T) {
			got := CeilingMemory(tc.detected)
			if got != tc.want {
				t.Errorf("CeilingMemory(%s) = %s, want %s", tc.detected, got, tc.want)
			}
			if !strings.HasSuffix(got.String(), "GiB") {
				t.Errorf("the ceiling renders as %q, which is not reviewable beside the tiers",
					got)
			}
			if got > tc.detected {
				t.Errorf("the ceiling %s exceeds the %s detected", got, tc.detected)
			}
		})
	}
}

// THE CURVE IS MONOTONIC, which is the property the small end kept breaking.
//
// Two earlier versions each failed it in their own direction: returning the
// whole amount below the reservation withheld nothing from the kernel, and
// refusing outright broke billet init's commitment to write a config that loads
// on any size of machine. Between them the rule was not even monotonic — 4GiB
// was accepted while a LARGER 4.5GiB host was refused, because its remainder
// rounded away to nothing.
//
// More memory must never buy less ceiling, and no ceiling may exceed its
// machine. Swept across the range rather than asserted at one boundary, because
// the next change to this rule will have its own boundary and this test should
// not have to know where it is.
//
// THIS SAMPLES THE PROPERTY, IT DOES NOT PROVE IT. The step is 128MiB, so a dip
// that recovers between two samples passes — and the reservation's second
// crossover sits at 64GiB+16 bytes, which a 128MiB stride steps straight over.
// The tests below cover the swap points byte by byte and pin an exact value
// inside each range; this covers the long spans between them, out to a size no
// host billet targets exceeds, because a sweep that stops early cannot see a
// rule that misbehaves past its last sample.
func TestMoreMemoryNeverBuysLessCeiling(t *testing.T) {
	var previous config.ByteSize

	for mem := config.ByteSize(0); mem <= config.TiB; mem += 128 * config.MiB {
		got := CeilingMemory(mem)
		if got < previous {
			t.Fatalf("CeilingMemory(%s) = %s, less than the %s a smaller host got",
				mem, got, previous)
		}
		if got > mem {
			t.Fatalf("CeilingMemory(%s) = %s, more than the machine has", mem, got)
		}

		previous = got
	}
}

// A HOST TOO SMALL TO SPEND ANYTHING IS REFUSED THROUGH THE ONE VALIDATION PATH.
//
// The reservation is capped at half the machine so a small host still gets a
// budget, but below about 2GiB that half does not reach a whole GiB and the
// ceiling rounds to zero. config.Parse then refuses it with its own diagnostic
// — billet does not carry a second validator for this.
func TestAHostTooSmallToBudgetIsRefused(t *testing.T) {
	for _, mem := range []config.ByteSize{config.MiB, 512 * config.MiB, config.GiB} {
		if got := CeilingMemory(mem); got != 0 {
			t.Errorf("CeilingMemory(%s) = %s, want 0 — a sub-GiB budget is not a budget",
				mem, got)
		}
	}

	_, _, err := Generate(Params{
		Org: "acme", Provider: config.ProviderDocker, VCPU: 4, Memory: config.GiB,
		RunnerGroup: "billet-trial",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
	})
	if err == nil {
		t.Fatal("a machine with no spendable memory generated a config")
	}
	if !strings.Contains(err.Error(), "server.max_memory must be positive") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// A READING THAT NEVER HAPPENED IS NOT A SMALL MACHINE.
//
// Subtracting a reservation from zero or a negative detection used to return 1
// vCPU — a ceiling ABOVE what was detected. That does not fail, it overcommits,
// and the config self-validates because a positive ceiling looks fine.
func TestANonReadingIsRefusedRatherThanFloored(t *testing.T) {
	for _, vcpu := range []int{0, -1, -1000} {
		if got := CeilingVCPU(vcpu); got != 0 {
			t.Errorf("CeilingVCPU(%d) = %d, want 0 — that is more than was detected", vcpu, got)
		}
	}

	for _, mem := range []config.ByteSize{0, -config.GiB} {
		if got := CeilingMemory(mem); got != 0 {
			t.Errorf("CeilingMemory(%s) = %s, want 0", mem, got)
		}
	}

	// And Generate refuses it by name, rather than rendering a config whose
	// ceiling exceeds the machine it claims to describe.
	_, _, err := Generate(Params{
		Org: "acme", Provider: config.ProviderDocker, VCPU: 0, Memory: 32 * config.GiB,
		RunnerGroup: "billet-trial",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
	})
	if err == nil {
		t.Fatal("a zero-vCPU reading generated a config")
	}
	if !strings.Contains(err.Error(), "server.max_vcpu must be positive") {
		t.Errorf("the refusal does not name the problem: %v", err)
	}
}

// EACH TERM MUST GOVERN ITS OWN RANGE, and monotonicity alone does not say so.
//
// The previous version of this test walked byte-wide windows around the three
// points where the terms swap and asserted only that the curve did not dip.
// That constrains almost nothing: an implementation that used the half-cap up to
// 16GiB, the flat floor up to 128GiB and the share above that passes every
// window, the exact-value table, and the sweep — while generating a wrong
// ceiling across two wide bands of real machine sizes.
//
// Worse, the byte-level switches are INVISIBLE at the points they occur, because
// the ceiling is rounded to a whole GiB: 64GiB+15, the last size the flat floor
// governs, and 64GiB+16, the first the share does, both yield 60GiB.
// A window around a switch therefore proves nothing about the switch. What pins
// the rule is an exact value from INSIDE each range, far enough from the
// boundary that a moved boundary changes the answer — 10GiB, 65GiB and 80GiB all
// differ from the counterexample above.
//
// These were derived from the rule and checked against an independent
// implementation of it, not read back from a passing run.
func TestEachTermGovernsItsOwnRange(t *testing.T) {
	for name, tc := range map[string]struct {
		detected, want config.ByteSize
	}{
		// The half-machine cap: below 8GiB it is smaller than the flat floor.
		"half the machine, at 4GiB": {4 * config.GiB, 2 * config.GiB},
		"half the machine, at 6GiB": {6 * config.GiB, 3 * config.GiB},

		// The flat floor, from 8GiB until the share overtakes it at 64GiB+16.
		// 9GiB and 48GiB sit just inside each end of that range: a boundary
		// moved outward to 10GiB or inward to 48GiB passes every other anchor
		// here and is caught only by these two.
		"the flat floor, just inside its lower end": {9 * config.GiB, 5 * config.GiB},
		"the flat floor, at 10GiB":                  {10 * config.GiB, 6 * config.GiB},
		"the flat floor, at 16GiB":                  {16 * config.GiB, 12 * config.GiB},
		"the flat floor, just inside its upper end": {48 * config.GiB, 44 * config.GiB},
		// The last size whose ceiling still DIFFERS from a share that started
		// early. A boundary moved down to 60GiB is invisible at 48, 64 and 65GiB
		// — all three round to the same answer either way — and shows only here.
		"the flat floor, where an early share would show": {61*config.GiB - 1, 56 * config.GiB},
		"the flat floor, at 64GiB":                        {64 * config.GiB, 60 * config.GiB},

		// The proportional share, from 64GiB+16 up. The 1TiB anchor is what
		// catches a rule that quietly stops growing: clamping every result above
		// 512GiB satisfies monotonicity, the sweep and every anchor below it.
		"the share, just above the floor": {65 * config.GiB, 60 * config.GiB},
		"the share, at 80GiB":             {80 * config.GiB, 75 * config.GiB},
		"the share, at 512GiB":            {512 * config.GiB, 480 * config.GiB},
		"the share, at 1TiB":              {config.TiB, 960 * config.GiB},

		// Where the rounding itself decides: one byte below 2GiB the spendable
		// half is 1GiB-1 and rounds away entirely.
		"one byte short of a spendable GiB": {2*config.GiB - 2, 0},
		"exactly one spendable GiB":         {2*config.GiB - 1, config.GiB},
	} {
		t.Run(name, func(t *testing.T) {
			if got := CeilingMemory(tc.detected); got != tc.want {
				t.Errorf("CeilingMemory(%d B) = %s, want %s", tc.detected, got, tc.want)
			}
		})
	}
}

// AND NO ONE-BYTE DIP WHERE THE TERMS SWAP.
//
// A rule assembled from min and max is exactly the shape that goes non-monotonic
// where its terms trade places, and no stride can catch a dip that recovers in a
// byte. The windows are wide because integer division moves the real switch off
// the algebraic point by up to a divisor: floor(x/16) first exceeds 4GiB at
// 64GiB+16, not at 64GiB.
func TestTheReservationDoesNotDipWhereTheTermsSwap(t *testing.T) {
	for name, at := range map[string]config.ByteSize{
		"where a budget first rounds to a whole GiB": 2 * config.GiB,
		"where the half-cap yields to the floor":     8 * config.GiB,
		"where the floor yields to the share":        headroomDivisor * HeadroomMemory,
	} {
		t.Run(name, func(t *testing.T) {
			for offset := config.ByteSize(-64); offset < 64; offset++ {
				lower, upper := at+offset, at+offset+1

				low, high := CeilingMemory(lower), CeilingMemory(upper)
				if high < low {
					t.Errorf("CeilingMemory(%d B) = %s but CeilingMemory(%d B) = %s — "+
						"one more byte bought less ceiling", lower, low, upper, high)
				}
				if high > upper {
					t.Errorf("CeilingMemory(%d B) = %s, more than the machine has", upper, high)
				}
			}
		})
	}
}

// A CATALOGUE MUST FIT ITS OWN BUDGET, ALL OF IT AT ONCE.
//
// Every tier is its own scale set, and a listener escrows capacity before it
// advertises — one backed discovery slot per tier, because a scale set
// advertising zero is never discovered. So the floor is one job of every
// generated tier simultaneously, and testing each candidate against the bare
// ceiling is not the same question.
//
// The case that shipped: a host measured at 10 vCPU / 19GiB generated an 8GiB
// and a 16GiB tier. Both were individually legal; together they needed 24GiB.
// The larger tier's discovery slot took the memory, both tiers advertised zero,
// and jobs queued forever against a control plane that reported itself healthy.
func TestTheGeneratedCatalogueFitsItsOwnCeiling(t *testing.T) {
	for name, tc := range map[string]struct {
		vcpu   int
		memory config.ByteSize
		want   []string
	}{
		// The host from the walk. 8+16 = 24GiB against 19GiB, so the 4vcpu
		// tier is dropped rather than generated and left undiscoverable.
		"the host this was found on": {10, 19 * config.GiB, []string{"billet-2vcpu"}},
		// Exactly enough for both: 8+16 = 24GiB.
		"exactly enough for two": {6, 24 * config.GiB, []string{"billet-2vcpu", "billet-4vcpu"}},
		// One GiB short of both.
		"one GiB short of two": {6, 23 * config.GiB, []string{"billet-2vcpu"}},
		// A real server: 8+16+32 = 56GiB, well inside.
		"a server": {120, 468 * config.GiB,
			[]string{"billet-2vcpu", "billet-4vcpu", "billet-8vcpu"}},
		// vCPU binds before memory: 2+4=6 fits, +8 would be 14 > 12.
		"vcpu binds first": {12, 512 * config.GiB, []string{"billet-2vcpu", "billet-4vcpu"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := tiers(tc.vcpu, tc.memory)

			var (
				labels     []string
				usedVCPU   int
				usedMemory config.ByteSize
			)

			for _, tr := range got {
				labels = append(labels, tr.label)
				usedVCPU += tr.vcpu
				usedMemory += tr.memory
			}

			if strings.Join(labels, ",") != strings.Join(tc.want, ",") {
				t.Errorf("tiers(%d, %s) = %v, want %v", tc.vcpu, tc.memory, labels, tc.want)
			}

			// The property, independent of the exact ladder: one job of every
			// tier at once must fit, or some tier can never be discovered.
			if usedVCPU > tc.vcpu || usedMemory > tc.memory {
				t.Errorf("the catalogue needs %d vCPU and %s to make every tier "+
					"discoverable, but the ceiling is %d vCPU and %s",
					usedVCPU, usedMemory, tc.vcpu, tc.memory)
			}
		})
	}
}

// AND A MACHINE TOO SMALL FOR ANY SHAPE STILL GETS ONE TIER.
//
// A config with no tiers loads and then schedules nothing, which is the failure
// the fallback exists to prevent — and it must itself fit the ceiling.
func TestATinyHostStillGetsOneServableTier(t *testing.T) {
	got := tiers(2, 3*config.GiB)

	if len(got) != 1 {
		t.Fatalf("a tiny host generated %d tiers, want exactly 1: %v", len(got), got)
	}
	if got[0].vcpu > 2 || got[0].memory > 3*config.GiB {
		t.Errorf("the fallback tier %d vCPU / %s does not fit its own 2 vCPU / 3GiB ceiling",
			got[0].vcpu, got[0].memory)
	}
}
