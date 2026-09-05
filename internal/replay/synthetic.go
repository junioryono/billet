package replay

import (
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Params shape a synthetic trace. Zero values take the defaults below.
type Params struct {
	// Jobs is how many arrivals to generate.
	Jobs int
	// Tiers are the labels jobs are spread over, first the most common.
	Tiers []string
	// Repositories are the repositories jobs belong to, first the most common.
	Repositories []string
	// Start is the trace's first instant. A Monday at nine, by default.
	Start time.Time
}

// DefaultStart is a Monday morning, so a trace reads like a working day.
var DefaultStart = time.Date(2026, time.January, 5, 9, 0, 0, 0, time.UTC)

func (p Params) withDefaults() Params {
	if p.Jobs <= 0 {
		p.Jobs = 200
	}

	if len(p.Tiers) == 0 {
		p.Tiers = []string{"billet-2vcpu", "billet-4vcpu"}
	}

	if len(p.Repositories) == 0 {
		p.Repositories = []string{"web", "api", "infra"}
	}

	if p.Start.IsZero() {
		p.Start = DefaultStart
	}

	return p
}

// generator is one seeded source of arrivals. Same seed, same trace: the
// generators exist so two policies can be compared on one workload, and a
// comparison against a different sample is not a comparison.
type generator struct {
	r *rand.Rand
	p Params
}

func newGenerator(seed uint64, p Params) *generator {
	return &generator{
		// A SEEDED GENERATOR ON PURPOSE. The property a comparison needs is that
		// one seed is one trace, which a cryptographic source cannot give.
		r: rand.New(rand.NewPCG(seed, seed*0x9E3779B97F4A7C15+1)), //nolint:gosec // math/rand/v2 is required: the trace must be reproducible from its seed
		p: p.withDefaults(),
	}
}

// pick chooses from a list with a bias to the front: the first entry is chosen
// about twice as often as the second, and so on.
func (g *generator) pick(items []string) string {
	weights := make([]float64, len(items))
	total := 0.0

	for i := range items {
		weights[i] = 1 / float64(i+1)
		total += weights[i]
	}

	x := g.r.Float64() * total

	for i, w := range weights {
		x -= w
		if x <= 0 {
			return items[i]
		}
	}

	return items[len(items)-1]
}

// lognormal draws a duration around a median with the spread a job mix has:
// most near the median, a tail several times longer.
func (g *generator) lognormal(median time.Duration, sigma float64) time.Duration {
	d := float64(median) * math.Exp(sigma*g.r.NormFloat64())

	return time.Duration(d).Round(time.Second)
}

// arrival builds one job at an instant.
func (g *generator) arrival(at time.Time, tier, repo string, runID int64, d time.Duration) Arrival {
	if d < time.Second {
		d = time.Second
	}

	return Arrival{
		At:          at,
		Tier:        tier,
		Owner:       DefaultOwner,
		Repository:  repo,
		WorkflowRef: fmt.Sprintf("%s/%s/.github/workflows/ci.yml@refs/heads/main", DefaultOwner, repo),
		RunID:       runID,
		Duration:    Duration(d),
		Result:      ResultSucceeded,
	}
}

// MorningBurst is a working day: seventy percent of the jobs land in the first
// hour as everyone pushes at once, the rest trickle over the next seven.
func MorningBurst(seed uint64, p Params) Trace {
	g := newGenerator(seed, p)

	var t Trace

	burst := g.p.Jobs * 7 / 10
	at := g.p.Start

	for i := range g.p.Jobs {
		if i < burst {
			// Exponential gaps whose mean fills the hour with the burst.
			at = at.Add(time.Duration(g.r.ExpFloat64() * float64(time.Hour) / float64(burst)))
		} else {
			at = g.p.Start.Add(time.Hour + time.Duration(g.r.Float64()*float64(7*time.Hour)))
		}

		t.Arrivals = append(t.Arrivals, g.arrival(at, g.pick(g.p.Tiers), g.pick(g.p.Repositories),
			int64(1000+i/3), g.lognormal(6*time.Minute, 0.8)))
	}

	t.Normalize()

	return t
}

// MonorepoFanOut is one repository whose every run fans out into many jobs that
// arrive within seconds of each other, a few runs an hour.
func MonorepoFanOut(seed uint64, p Params) Trace {
	g := newGenerator(seed, p)

	var t Trace

	repo := g.p.Repositories[0]
	at := g.p.Start
	runID := int64(5000)

	for len(t.Arrivals) < g.p.Jobs {
		fan := 20 + g.r.IntN(41)
		if remaining := g.p.Jobs - len(t.Arrivals); fan > remaining {
			fan = remaining
		}

		runID++

		for range fan {
			jobAt := at.Add(time.Duration(g.r.Float64() * float64(10*time.Second)))
			t.Arrivals = append(t.Arrivals, g.arrival(jobAt, g.pick(g.p.Tiers), repo, runID,
				g.lognormal(4*time.Minute, 0.5)))
		}

		at = at.Add(time.Duration(g.r.ExpFloat64() * float64(20*time.Minute)))
	}

	t.Normalize()

	return t
}

// LongTail is a steady trickle of short jobs beside a few that run for hours,
// which is the shape that shows whether a placement policy strands capacity
// behind something that will not finish.
func LongTail(seed uint64, p Params) Trace {
	g := newGenerator(seed, p)

	var t Trace

	at := g.p.Start

	for i := range g.p.Jobs {
		at = at.Add(time.Duration(g.r.ExpFloat64() * float64(90*time.Second)))

		d := g.lognormal(2*time.Minute, 0.6)
		if g.r.IntN(40) == 0 {
			d = 2*time.Hour + time.Duration(g.r.Float64()*float64(4*time.Hour))
		}

		t.Arrivals = append(t.Arrivals, g.arrival(at, g.pick(g.p.Tiers), g.pick(g.p.Repositories),
			int64(9000+i), d))
	}

	t.Normalize()

	return t
}
