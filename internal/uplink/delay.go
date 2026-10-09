package uplink

import (
	"math"
	"slices"
	"time"
)

// baselineRise is how slowly a reflector's baseline follows a round trip above
// it: one part in this many per sample. At two samples a second a queue that
// lasts a few seconds barely moves it, while a route that really did get longer
// is followed within minutes. A round trip below the baseline replaces it at
// once, because an idle path is the fastest one ever seen.
const baselineRise = 1000

// Delays turns each round's round trips into how far the line's queue has
// risen, judged against each reflector's own idle baseline.
type Delays struct {
	baseline map[string]time.Duration
}

// Observe records one round. answered maps a reflector to its round trip;
// every reflector asked and not in answered went unanswered, and a lost reply
// counts as an unbounded delay, because a full queue drops as well as delays.
// The delay is the median over the reflectors that have a baseline, so one
// slow or vanished reflector cannot move it; known is false until at least one
// has.
func (d *Delays) Observe(asked []string, answered map[string]time.Duration) (time.Duration, bool) {
	if d.baseline == nil {
		d.baseline = make(map[string]time.Duration)
	}

	var risen []time.Duration

	for _, reflector := range asked {
		rtt, ok := answered[reflector]
		base, known := d.baseline[reflector]

		switch {
		case ok && (!known || rtt < base):
			d.baseline[reflector] = rtt
		case ok:
			d.baseline[reflector] = base + (rtt-base)/baselineRise
		}

		if !known {
			continue
		}

		if !ok {
			risen = append(risen, time.Duration(math.MaxInt64))

			continue
		}

		risen = append(risen, max(0, rtt-base))
	}

	if len(risen) == 0 {
		return 0, false
	}

	slices.Sort(risen)

	// THE LOWER MEDIAN, so a delay is declared only when a strict majority of
	// reflectors see it: with an even count, half of them unreachable is not a
	// full queue.
	return risen[(len(risen)-1)/2], true
}
