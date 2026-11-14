package lease

import (
	"math"
	"strings"
	"testing"
)

// twoDestinations is a report with one named destination and traffic beyond it,
// split by time at a 2-second width so each rule has room to be broken.
func twoDestinations() JobDestinations {
	return JobDestinations{
		Destinations: []JobDestination{{Addr: "140.82.112.4", SentBytes: 150, ReceivedBytes: 7000, Connections: 3}},
		Other:        JobDestination{SentBytes: 10, ReceivedBytes: 900, Connections: 1},
		Openings: []DestinationOpening{
			{Ordinal: 0, Start: 1_794_650_000, SentBytes: 100, ReceivedBytes: 5000, Connections: 2},
			{Ordinal: 0, Start: 1_794_650_030, SentBytes: 50, ReceivedBytes: 2000, Connections: 1},
			{Ordinal: MaxJobDestinations, Start: 1_794_650_012, SentBytes: 10, ReceivedBytes: 900, Connections: 1},
		},
		OpeningSeconds: 2,
	}
}

// A SPLIT THE DESTINATIONS COULD HAVE GIVEN IS KEPT, and none is too: a report
// from a node before the openings has neither a width nor any opening.
func TestOpeningsThatSplitTheDestinationsAreValid(t *testing.T) {
	t.Parallel()

	if err := twoDestinations().Validate(); err != nil {
		t.Errorf("a split of the destinations was refused: %v", err)
	}
	if err := twoDestinations().WithoutOpenings().Validate(); err != nil {
		t.Errorf("destinations with no split were refused: %v", err)
	}
	// Less than the totals is a split of totals a node clamped, which it may be.
	d := twoDestinations()
	d.Openings = d.Openings[:1]
	if err := d.Validate(); err != nil {
		t.Errorf("openings adding up to less than their destination's totals were refused: %v", err)
	}
}

// EACH OPENING NO SPLIT OF THESE DESTINATIONS GIVES IS REFUSED, by its own reason.
func TestOpeningsNoSplitGivesAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*JobDestinations)
		want   string
	}{
		{"openings with no width", func(d *JobDestinations) { d.OpeningSeconds = 0 }, "no width"},
		{"a width that is not a power of two", func(d *JobDestinations) { d.OpeningSeconds = 3 }, "power of two"},
		{"a negative width", func(d *JobDestinations) { d.OpeningSeconds = -2 }, "power of two"},
		{"a width past the bound", func(d *JobDestinations) { d.OpeningSeconds = maxOpeningSeconds * 2 }, "power of two"},
		{"more openings than are kept", func(d *JobDestinations) {
			d.OpeningSeconds = 1
			d.Openings = make([]DestinationOpening, MaxDestinationOpenings+1)
			for i := range d.Openings {
				d.Openings[i] = DestinationOpening{Ordinal: 0, Start: int64(1_794_650_000 + i)}
			}
		}, "at most"},
		{"an ordinal naming no destination", func(d *JobDestinations) { d.Openings[2].Ordinal = 1 }, "names destination"},
		{"a negative ordinal", func(d *JobDestinations) { d.Openings[0].Ordinal = -1 }, "names destination"},
		{"a bucket off its width", func(d *JobDestinations) { d.Openings[0].Start++ }, "bucket"},
		{"a bucket before 1970", func(d *JobDestinations) { d.Openings[0].Start = 0 }, "bucket"},
		{"a negative count", func(d *JobDestinations) { d.Openings[1].SentBytes = -1 }, "negative"},
		{"two openings of one bucket", func(d *JobDestinations) { d.Openings[1].Start = d.Openings[0].Start }, "not after"},
		{"openings out of order", func(d *JobDestinations) {
			d.Openings[0], d.Openings[1] = d.Openings[1], d.Openings[0]
		}, "not after"},
		{"openings adding up past a destination's sent", func(d *JobDestinations) { d.Openings[1].SentBytes = 51 }, "more than its totals"},
		{"openings adding up past a destination's received", func(d *JobDestinations) { d.Openings[2].ReceivedBytes = 901 }, "more than its totals"},
		{"openings adding up past a destination's connections", func(d *JobDestinations) { d.Openings[0].Connections = 3 }, "more than its totals"},
	} {
		d := twoDestinations()
		tc.change(&d)
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want a refusal saying %q", tc.name, err, tc.want)
		}
	}
}

// A SUM PAST AN INT64 IS HELD AT ITS LARGEST, so openings whose counts each fit
// are still compared with their destination's totals, never wrapped below them.
func TestOpeningsThatWouldWrapAreStillRefused(t *testing.T) {
	t.Parallel()

	d := twoDestinations()
	d.Destinations[0].SentBytes = math.MaxInt64 - 1
	d.Openings[0].SentBytes = math.MaxInt64
	d.Openings[1].SentBytes = math.MaxInt64
	if err := d.Validate(); err == nil || !strings.Contains(err.Error(), "more than its totals") {
		t.Errorf("openings whose sum wraps past an int64 = %v, want refused as more than the totals", err)
	}
	if got := addCapped(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Errorf("addCapped past the largest int64 = %d", got)
	}
}
