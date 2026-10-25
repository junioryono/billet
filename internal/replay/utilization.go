package replay

import (
	"fmt"
	"io"
	"slices"
	"time"
)

// UtilizationBucket is the default width Utilization averages over.
const UtilizationBucket = 15 * time.Minute

// UtilizationPoint is the share of the fleet's room the ledger charged over
// one bucket, averaged over time.
type UtilizationPoint struct {
	From         time.Time
	VCPU, Memory float64
}

// span is the interval a report's figures over time cover: from the first
// charge to the last end, or to the horizon when the replay had one. A charge
// the ledger never ended runs to the span's end.
func (r *Report) span() (time.Time, time.Time, bool) {
	var from, to time.Time

	for _, c := range r.charges() {
		if from.IsZero() || c.from.Before(from) {
			from = c.from
		}

		if !c.open() && c.to.After(to) {
			to = c.to
		}
	}

	if !r.Horizon.IsZero() {
		to = r.Horizon
	}

	if from.IsZero() || !to.After(from) {
		return time.Time{}, time.Time{}, false
	}

	return from, to, true
}

// room is the fleet's room: the deployment ceiling, or what the hosts hold
// between them when that is less.
func (r *Report) room() (float64, float64) {
	vcpu, memory := 0, int64(0)

	for _, h := range r.Fleet.Hosts {
		vcpu += h.VCPU
		memory += int64(h.Memory)
	}

	return float64(min(vcpu, r.Fleet.MaxVCPU)), float64(min(memory, int64(r.Fleet.MaxMemory)))
}

// loadStep is one step of what the ledger charged over the span: from at
// until nextFrom, the fleet's vCPU and memory and each host's vCPU.
type loadStep struct {
	at       time.Time
	vcpu     int
	memory   int64
	perNode  map[string]int
	nextFrom time.Time
}

// steps sweeps every charge into the steps of the load between from and to.
func (r *Report) steps(from, to time.Time) []loadStep {
	type change struct {
		at     time.Time
		vcpu   int
		memory int64
		node   string
	}

	var changes []change

	for _, c := range r.charges() {
		changes = append(changes, change{c.from, c.vcpu, int64(c.memory), c.node})

		if !c.open() {
			changes = append(changes, change{c.to, -c.vcpu, -int64(c.memory), c.node})
		}
	}

	slices.SortStableFunc(changes, func(a, b change) int { return a.at.Compare(b.at) })

	var (
		out    []loadStep
		vcpu   int
		memory int64
	)

	perNode := map[string]int{}

	for i := 0; i < len(changes); {
		at := changes[i].at
		for ; i < len(changes) && changes[i].at.Equal(at); i++ {
			vcpu += changes[i].vcpu
			memory += changes[i].memory
			perNode[changes[i].node] += changes[i].vcpu
		}

		out = append(out, loadStep{at: at, vcpu: vcpu, memory: memory, perNode: cloneLoad(perNode)})
	}

	// Each step lasts until the next, the last until the span's end; steps are
	// cut to the span.
	kept := out[:0]

	for i := range out {
		end := to
		if i+1 < len(out) {
			end = out[i+1].at
		}

		start := out[i].at
		if start.Before(from) {
			start = from
		}

		if end.After(to) {
			end = to
		}

		if !end.After(start) {
			continue
		}

		out[i].at, out[i].nextFrom = start, end
		kept = append(kept, out[i])
	}

	return kept
}

func cloneLoad(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}

	return out
}

// Utilization is the time-weighted share of the fleet's room the ledger
// charged, per bucket from the span's start, escrow included: what the
// deployment held, not what its jobs ran.
func (r *Report) Utilization(bucket time.Duration) []UtilizationPoint {
	from, to, ok := r.span()
	if !ok || bucket <= 0 {
		return nil
	}

	roomVCPU, roomMemory := r.room()
	if roomVCPU <= 0 || roomMemory <= 0 {
		return nil
	}

	n := int((to.Sub(from) + bucket - 1) / bucket)
	vcpu := make([]float64, n)
	memory := make([]float64, n)

	for _, s := range r.steps(from, to) {
		for t := s.at; t.Before(s.nextFrom); {
			b := int(t.Sub(from) / bucket)
			end := earlier(from.Add(time.Duration(b+1)*bucket), s.nextFrom)
			d := end.Sub(t).Seconds()
			vcpu[b] += float64(s.vcpu) * d
			memory[b] += float64(s.memory) * d
			t = end
		}
	}

	out := make([]UtilizationPoint, n)

	for b := range n {
		start := from.Add(time.Duration(b) * bucket)
		width := earlier(start.Add(bucket), to).Sub(start).Seconds()
		out[b] = UtilizationPoint{From: start, VCPU: vcpu[b] / width / roomVCPU, Memory: memory[b] / width / roomMemory}
	}

	return out
}

// WriteUtilization writes Utilization over bucket as CSV, one bucket a line:
// its start in RFC 3339 and the shares of the room's vCPU and memory charged.
func (r *Report) WriteUtilization(w io.Writer, bucket time.Duration) error {
	if _, err := fmt.Fprintln(w, "from,vcpu,memory"); err != nil {
		return fmt.Errorf("replay: write utilization: %w", err)
	}

	for _, p := range r.Utilization(bucket) {
		if _, err := fmt.Fprintf(w, "%s,%.4f,%.4f\n", p.From.Format(time.RFC3339), p.VCPU, p.Memory); err != nil {
			return fmt.Errorf("replay: write utilization: %w", err)
		}
	}

	return nil
}

// MeanUtilization is the share of the fleet's room charged over the whole
// span, by vCPU and by memory.
func (r *Report) MeanUtilization() (float64, float64) {
	from, to, ok := r.span()
	if !ok {
		return 0, 0
	}

	roomVCPU, roomMemory := r.room()
	if roomVCPU <= 0 || roomMemory <= 0 {
		return 0, 0
	}

	var vcpu, memory float64

	for _, s := range r.steps(from, to) {
		d := s.nextFrom.Sub(s.at).Seconds()
		vcpu += float64(s.vcpu) * d
		memory += float64(s.memory) * d
	}

	span := to.Sub(from).Seconds()

	return vcpu / span / roomVCPU, memory / span / roomMemory
}

// EmptyHostHours is the host-hours over the span in which a host carried
// nothing at all: room a whole-host shape could have used.
func (r *Report) EmptyHostHours() float64 {
	from, to, ok := r.span()
	if !ok {
		return 0
	}

	// The steps cover the span without a gap: the first begins at its start,
	// the first charge, and each lasts until the next.
	steps := r.steps(from, to)

	var empty time.Duration

	for _, h := range r.Fleet.Hosts {
		for _, s := range steps {
			if s.perNode[h.Name] == 0 {
				empty += s.nextFrom.Sub(s.at)
			}
		}
	}

	return empty.Hours()
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}
