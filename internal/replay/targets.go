package replay

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// TargetCharge is what one target's jobs held over an interval. A lease bought
// and never given a job belongs to no job, so it is in no target's figures.
type TargetCharge struct {
	VCPUSeconds, GiBSeconds float64
}

// ChargedByTarget is what each target's jobs held between from and to, from the
// ledger's own charges: a lease the ledger never ended runs to the interval's
// end. A tier the fleet names no target for is the default target's.
func (r *Report) ChargedByTarget(from, to time.Time) map[string]TargetCharge {
	out := map[string]TargetCharge{}
	byTier := r.targetByTier()

	for i := range r.Records {
		rec := &r.Records[i]

		start, end := rec.ChargedFrom, rec.FinishedAt
		if end.IsZero() || end.After(to) {
			end = to
		}

		if start.Before(from) {
			start = from
		}

		if !end.After(start) {
			continue
		}

		seconds := end.Sub(start).Seconds()
		sum := out[byTier[rec.Tier]]
		sum.VCPUSeconds += float64(rec.VCPU) * seconds
		sum.GiBSeconds += float64(rec.Memory) / float64(config.GiB) * seconds
		out[byTier[rec.Tier]] = sum
	}

	return out
}

// targetByTier is the target each of the fleet's tiers belongs to.
func (r *Report) targetByTier() map[string]string {
	out := make(map[string]string, len(r.Fleet.Tiers))
	for _, t := range r.Fleet.Tiers {
		out[t.Label] = config.ShareTarget(config.Tier{Target: t.Target})
	}

	return out
}

// ContestedWindow is the interval in which every target that ran a job was both
// holding room and still had a job to start: from the last of their first
// charges to the first of their last starts. Outside it one target may hold
// everything because nobody else is asking, which says nothing about the order.
// False with fewer than two such targets, or when no such interval exists.
func (r *Report) ContestedWindow() (time.Time, time.Time, bool) {
	first, last := map[string]time.Time{}, map[string]time.Time{}
	byTier := r.targetByTier()

	for i := range r.Records {
		rec := &r.Records[i]
		target := byTier[rec.Tier]

		if at, seen := first[target]; !seen || rec.ChargedFrom.Before(at) {
			first[target] = rec.ChargedFrom
		}

		if rec.StartedAt.After(last[target]) {
			last[target] = rec.StartedAt
		}
	}

	if len(first) < 2 {
		return time.Time{}, time.Time{}, false
	}

	var from, to time.Time

	for target, at := range first {
		if at.After(from) {
			from = at
		}

		if to.IsZero() || last[target].Before(to) {
			to = last[target]
		}
	}

	return from, to, to.After(from)
}

// TargetTable is the charges of a contested window as a table a record can
// carry: each target's vCPU-hours and GiB-hours over the whole window and its
// share of the vCPU in each of parts even slices of it, which is what shows
// the division converging.
func (r *Report) TargetTable(parts int) string {
	from, to, ok := r.ContestedWindow()
	if !ok {
		return "no window in which two targets contended\n"
	}

	weights := map[string]int{}
	for _, target := range r.Fleet.Targets {
		weights[target.Name] = target.Weight
	}

	whole := r.ChargedByTarget(from, to)

	var b strings.Builder

	fmt.Fprintf(&b, "Contested for %s.\n\n| Target | Weight | vCPU-hours | GiB-hours |", to.Sub(from).Round(time.Second))

	for part := range parts {
		fmt.Fprintf(&b, " vCPU share, slice %d |", part+1)
	}

	b.WriteString("\n|---|---|---|---|" + strings.Repeat("---|", parts) + "\n")

	step := to.Sub(from) / time.Duration(max(parts, 1))

	for _, target := range slices.Sorted(maps.Keys(whole)) {
		fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f |", target, weights[target], whole[target].VCPUSeconds/3600,
			whole[target].GiBSeconds/3600)

		for part := range parts {
			slice := r.ChargedByTarget(from.Add(time.Duration(part)*step), from.Add(time.Duration(part+1)*step))

			total := 0.0
			for _, c := range slice {
				total += c.VCPUSeconds
			}

			share := 0.0
			if total > 0 {
				share = slice[target].VCPUSeconds / total
			}

			fmt.Fprintf(&b, " %.0f%% |", 100*share)
		}

		b.WriteString("\n")
	}

	return b.String()
}
