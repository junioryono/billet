package fleetops

import (
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cloudenergy"
)

// energyEstimate is a job's energy by cloudenergy's model, false for every job
// that has no estimate.
//
// ONLY A LEASE THAT BOUGHT A SHAPE IS ESTIMATED: it ran on a machine billet
// rented, where nothing of billet's reads the host. A host-backed job with no
// energy measured is a node without node.monitoring, or a reading that failed,
// and a model standing in for either would hide it. A job whose energy was
// measured is never estimated too.
func energyEstimate(instanceType string, u *alloc.JobUsage, vcpu int64, d time.Duration,
	ran bool,
) (cloudenergy.Estimate, bool) {
	if instanceType == "" || !ran || (u != nil && u.Measured(alloc.UsageEnergy)) {
		return cloudenergy.Estimate{}, false
	}

	return cloudenergy.Of(vcpu, d)
}

// recordEnergyEstimate is energyEstimate for one job's record.
//
// OVER THE DURATION `billet jobs stats` TAKES, from the start to GitHub's result,
// not the runtime this report prints beside it, which runs to the lease's
// archive: one job is estimated, or not, alike in both.
func recordEnergyEstimate(rec alloc.JobRecord, u *alloc.RecordedUsage) (cloudenergy.Estimate, bool) {
	var measured *alloc.JobUsage
	if u != nil {
		measured = &u.JobUsage
	}
	d, ran := rec.Duration()

	return energyEstimate(rec.InstanceType, measured, rec.VCPU, d, ran)
}

// estimateText is how a report words an estimate: the figure, then how it was
// made, so it is never read without the word estimated.
func estimateText(e cloudenergy.Estimate) string {
	return estimateJoules(e.Microjoules) + ", " + e.Method()
}

// estimateJoules words an estimate's energy as joules does, with the
// significant figures it would otherwise round to 0.0 J: an estimate is only
// ever made of something, so it never reads as nothing.
func estimateJoules(microjoules int64) string {
	if microjoules < 50_000 {
		return fmt.Sprintf("%.3g J", float64(microjoules)/1e6)
	}

	return joules(microjoules)
}

// energyEstimateJSON is a job's estimate in `billet jobs show --json`: the
// figure, what the model was given, and its coefficients.
type energyEstimateJSON struct {
	Joules          float64 `json:"joules"`
	Method          string  `json:"method"`
	VCPU            int64   `json:"vcpu"`
	DurationSeconds float64 `json:"duration_seconds"`
	Utilization     float64 `json:"assumed_utilization"`
	MinWattsPerVCPU float64 `json:"min_watts_per_vcpu"`
	MaxWattsPerVCPU float64 `json:"max_watts_per_vcpu"`
}

// energyEstimateJSONOf is nil for a job with no estimate.
func energyEstimateJSONOf(e cloudenergy.Estimate, ok bool) *energyEstimateJSON {
	if !ok {
		return nil
	}

	return &energyEstimateJSON{Joules: float64(e.Microjoules) / 1e6, Method: e.Method(), VCPU: e.VCPU,
		DurationSeconds: e.Duration.Seconds(), Utilization: e.Utilization,
		MinWattsPerVCPU: cloudenergy.MinWattsPerVCPU, MaxWattsPerVCPU: cloudenergy.MaxWattsPerVCPU}
}
