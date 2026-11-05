package replay

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/config"
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

// Burst is one push queuing every job at once: all of them within ten seconds
// of the start, across the tiers, most a few minutes long.
func Burst(seed uint64, p Params) Trace {
	g := newGenerator(seed, p)

	var t Trace

	for i := range g.p.Jobs {
		at := g.p.Start.Add(time.Duration(g.r.Float64() * float64(10*time.Second)))
		t.Arrivals = append(t.Arrivals, g.arrival(at, g.pick(g.p.Tiers), g.pick(g.p.Repositories),
			int64(20000+i/10), g.lognormal(5*time.Minute, 0.5)))
	}

	t.Normalize()

	return t
}

// IdleBacklog is a backlog larger than the fleet arriving within half a minute
// at an idle fleet, the shape of a fleet coming back after an outage: the
// queue is drained by what the hosts can hold, and the tail is how long the
// last of it waits.
func IdleBacklog(seed uint64, p Params) Trace {
	g := newGenerator(seed, p)

	var t Trace

	for i := range g.p.Jobs {
		at := g.p.Start.Add(time.Duration(g.r.Float64() * float64(30*time.Second)))
		t.Arrivals = append(t.Arrivals, g.arrival(at, g.pick(g.p.Tiers), g.pick(g.p.Repositories),
			int64(30000+i), g.lognormal(10*time.Minute, 0.4)))
	}

	t.Normalize()

	return t
}

// QuietParams shape QuietBesideSaturated.
type QuietParams struct {
	Params
	// Fleet is what the busy tier saturates; its tiers' first is the busy one
	// and its last the quiet one.
	Fleet Fleet
	// Saturation is the share of the fleet's vCPU room the busy tier offers;
	// zero means 1.5, a backlog that only grows.
	Saturation float64
}

// quietMedian and busyMedian are the runtimes QuietBesideSaturated draws
// around.
const (
	busyMedian  = 4 * time.Minute
	quietMedian = 6 * time.Minute
	quietSigma  = 0.3
	busySigma   = 0.4
)

// QuietBesideSaturated is one tier, the fleet's first, offered more work than
// the whole fleet holds (Saturation of its vCPU room), beside a quiet tier,
// the fleet's last, whose few jobs arrive at random across the same span on
// the same hosts: the shape that shows whether the quiet tier's jobs wait
// behind the busy one's backlog. A tenth of the jobs are the quiet tier's.
func QuietBesideSaturated(seed uint64, p QuietParams) Trace {
	if p.Saturation <= 0 {
		p.Saturation = 1.5
	}

	busy, quiet := p.Fleet.Tiers[0], p.Fleet.Tiers[len(p.Fleet.Tiers)-1]
	p.Tiers = []string{busy.Label, quiet.Label}
	g := newGenerator(seed, p.Params)

	room := 0
	for _, h := range p.Fleet.Hosts {
		room += h.VCPU
	}

	room = min(room, p.Fleet.MaxVCPU)

	quietJobs := max(g.p.Jobs/10, 1)
	busyJobs := g.p.Jobs - quietJobs

	// The busy tier's mean gap makes its offered vCPU-time Saturation of the
	// room: a job's vCPU times its mean runtime, over the room's share.
	meanBusy := float64(busyMedian) * math.Exp(busySigma*busySigma/2)
	gap := float64(busy.VCPU) * meanBusy / (p.Saturation * float64(room))

	var t Trace

	at := g.p.Start
	for i := range busyJobs {
		at = at.Add(time.Duration(g.r.ExpFloat64() * gap))
		t.Arrivals = append(t.Arrivals, g.arrival(at, busy.Label, g.pick(g.p.Repositories), int64(40000+i),
			g.lognormal(busyMedian, busySigma)))
	}

	span := at.Sub(g.p.Start)

	for i := range quietJobs {
		quietAt := g.p.Start.Add(time.Duration(g.r.Float64() * float64(span)))
		t.Arrivals = append(t.Arrivals, g.arrival(quietAt, quiet.Label, g.pick(g.p.Repositories), int64(45000+i),
			g.lognormal(quietMedian, quietSigma)))
	}

	t.Normalize()

	return t
}

// WideParams shape WideShapeMix.
type WideParams struct {
	Params
	// Fleet is what the load is offered to; its tiers are the mix, and the
	// widest of them is the whole-host shape.
	Fleet Fleet
	// OfferedLoad is the share of the fleet's vCPU room the arrivals offer on
	// average; zero means 0.85.
	OfferedLoad float64
	// WideShare is the share of the jobs that are of the widest tier; zero
	// means 0.05.
	WideShare float64
}

// WideMedian is the median runtime WideShapeMix draws every job around, and
// wideSigma the spread it draws them with.
const (
	WideMedian = 8 * time.Minute
	wideSigma  = 0.6
)

// WideShapeMix is a whole-host tier beside smaller ones, at an offered load
// near the fleet's room: the shape #527 is judged on. The widest tier of the
// fleet is the whole-host one and takes WideShare of the jobs; the rest are
// spread over the other tiers, the smallest the most common. Arrivals are a
// Poisson stream whose rate makes the offered vCPU-time OfferedLoad of the
// room: the rate is the room times the load over the expected vCPU-time of a
// job (its vCPU times the lognormal's mean runtime, median·e^(σ²/2)).
func WideShapeMix(seed uint64, p WideParams) Trace {
	if p.OfferedLoad <= 0 {
		p.OfferedLoad = 0.85
	}

	if p.WideShare <= 0 {
		p.WideShare = 0.05
	}

	tiers := slices.Clone(p.Fleet.Tiers)
	slices.SortStableFunc(tiers, func(a, b TierShape) int { return a.VCPU - b.VCPU })

	// A FLEET OF ONE TIER IS ALL WIDE: there is no narrower tier to mix in,
	// and the one tier stands for both.
	wide, narrow := tiers[len(tiers)-1], tiers[:len(tiers)-1]
	if len(narrow) == 0 {
		narrow = tiers
	}

	labels := make([]string, len(narrow))
	for i := range narrow {
		labels[i] = narrow[i].Label
	}

	p.Tiers = labels
	g := newGenerator(seed, p.Params)

	// THE EXPECTED vCPU OF A JOB, under the same bias pick uses: the i-th
	// narrow tier is chosen in proportion to 1/(i+1).
	weights, total := make([]float64, len(narrow)), 0.0
	for i := range narrow {
		weights[i] = 1 / float64(i+1)
		total += weights[i]
	}

	narrowVCPU := 0.0
	for i := range narrow {
		narrowVCPU += weights[i] / total * float64(narrow[i].VCPU)
	}

	meanVCPU := p.WideShare*float64(wide.VCPU) + (1-p.WideShare)*narrowVCPU
	meanRuntime := float64(WideMedian) * math.Exp(wideSigma*wideSigma/2)

	room := 0
	for _, h := range p.Fleet.Hosts {
		room += h.VCPU
	}

	room = min(room, p.Fleet.MaxVCPU)

	// Jobs per nanosecond.
	rate := p.OfferedLoad * float64(room) / (meanVCPU * meanRuntime)

	var t Trace

	at := g.p.Start

	for i := range g.p.Jobs {
		at = at.Add(time.Duration(g.r.ExpFloat64() / rate))

		tier := g.pick(g.p.Tiers)
		if g.r.Float64() < p.WideShare {
			tier = wide.Label
		}

		t.Arrivals = append(t.Arrivals, g.arrival(at, tier, g.pick(g.p.Repositories), int64(50000+i),
			g.lognormal(WideMedian, wideSigma)))
	}

	t.Normalize()

	return t
}

// WideFleet is four 32 vCPU hosts with tiers of 2, 4, 8 and 16 vCPU and a
// whole-host tier of 32, at 4 GiB a vCPU, under a ceiling the hosts add up to.
func WideFleet() Fleet {
	f := Fleet{MaxVCPU: 128, MaxMemory: 512 * config.GiB}

	for i := range 4 {
		f.Hosts = append(f.Hosts, Host{Name: fmt.Sprintf("host-%c", 'a'+i), VCPU: 32, Memory: 128 * config.GiB})
	}

	for _, vcpu := range []int{2, 4, 8, 16, 32} {
		f.Tiers = append(f.Tiers, TierShape{Label: fmt.Sprintf("billet-%dvcpu", vcpu), VCPU: vcpu,
			Memory: config.ByteSize(vcpu) * 4 * config.GiB})
	}

	return f
}
