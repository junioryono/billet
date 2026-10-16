package uplink

import (
	"slices"
	"time"
)

// baselineRise is how slowly a reflector's baseline follows a round trip above
// it: one part in this many per sample. At two samples a second a queue that
// lasts a few seconds barely moves it, while a route that really did get longer
// is followed within minutes. A round trip below the baseline replaces it at
// once, because an idle path is the fastest one ever seen.
const baselineRise = 1000

// silentAfter is how many rounds in a row a reflector may go unanswered before
// it stops being counted, until it answers again: ten seconds, cake-autorate's
// global ping response timeout. Until then every round it misses is a lost
// probe, so a line that drops everything is still seen dropping; after it, a
// reflector that has stopped answering ICMP for good is not read as a line
// losing a third of its packets forever, and a host whose reflectors have all
// gone quiet has no measurement rather than an invented one.
const silentAfter = 20

// Probe is one counted reflector's part in a round.
type Probe struct {
	Reflector string
	Lost      bool
}

// Sample is what one round of probes says about the line.
type Sample struct {
	// Delay is how far the round trip rose above its baseline, and DelayKnown
	// whether it was measured: a strict majority of the counted reflectors
	// answered. An unanswered probe is a loss, never a delay, so a round most
	// of the reflectors missed has no delay at all.
	Delay      time.Duration
	DelayKnown bool
	// Probes are the reflectors this round counted (those with a baseline that
	// have not gone silent) and which of them went unanswered. Empty is a round
	// that measured nothing.
	Probes []Probe
}

// Lost is how many of the round's counted probes went unanswered.
func (s Sample) Lost() int {
	lost := 0

	for _, p := range s.Probes {
		if p.Lost {
			lost++
		}
	}

	return lost
}

// Delays turns each round's round trips into how far the line's queue has
// risen, judged against each reflector's own idle baseline, and which probes
// the line lost.
type Delays struct {
	baseline map[string]time.Duration
	silent   map[string]int
}

// Observe records one round. answered maps a reflector to its round trip;
// every reflector asked and not in answered went unanswered. A reflector is
// counted once it has a baseline, and stops being counted after silentAfter
// unanswered rounds until it answers again.
func (d *Delays) Observe(asked []string, answered map[string]time.Duration) Sample {
	if d.baseline == nil {
		d.baseline = make(map[string]time.Duration)
		d.silent = make(map[string]int)
	}

	var (
		s     Sample
		risen []time.Duration
	)

	for _, reflector := range asked {
		rtt, ok := answered[reflector]
		base, known := d.baseline[reflector]
		silent := d.silent[reflector]

		switch {
		case ok && (!known || rtt < base):
			d.baseline[reflector] = rtt
		case ok:
			d.baseline[reflector] = base + (rtt-base)/baselineRise
		}

		if ok {
			d.silent[reflector] = 0
		} else {
			d.silent[reflector] = silent + 1
		}

		if !known || (!ok && silent >= silentAfter) {
			continue
		}

		s.Probes = append(s.Probes, Probe{Reflector: reflector, Lost: !ok})

		if ok {
			risen = append(risen, max(0, rtt-base))
		}
	}

	// A DELAY IS DECLARED ONLY WHEN A STRICT MAJORITY OF THE COUNTED REFLECTORS
	// SEE IT: the delay is the rise that many of them reach, so one slow or
	// vanished reflector cannot move it, and with fewer answers than a majority
	// there is no delay to report.
	need := len(s.Probes)/2 + 1
	if len(risen) < need {
		return s
	}

	slices.Sort(risen)
	slices.Reverse(risen)

	s.Delay, s.DelayKnown = risen[need-1], true

	return s
}
