// Package cloudenergy estimates the energy a job drew on a machine billet bought
// from a cloud, where nothing of billet's watches the host: no RAPL, no cgroup and
// no wall meter, only the shape the lease was charged and how long the job ran.
//
// AN ESTIMATE IS NEVER A MEASUREMENT. It is its own energy source, Source, which a
// report words as an estimate and never adds to a measured figure, since a model
// of someone else's hardware and a counter on billet's own measure different
// things and a sum of them is neither.
//
// The model is Cloud Carbon Footprint's for compute (its methodology, read
// 2026-11-14): average watts per vCPU is the minimum plus the utilization times
// the span to the maximum, with AWS's averages across microarchitectures for the
// two ends and its default utilization for a hyperscale data centre. Memory,
// storage, networking and the data centre's PUE are left out, so the figure is
// the processor's share of the server, which is what package RAPL is on a host
// billet runs. A leaf: the standard library only.
package cloudenergy

import (
	"fmt"
	"math"
	"time"
)

// Source is the energy source an estimate is reported under. No host reports
// it, so a usage report's source can never be mistaken for one.
const Source = "estimate"

// The coefficients, each Cloud Carbon Footprint's AWS figure.
const (
	// MinWattsPerVCPU and MaxWattsPerVCPU are the average watts per vCPU at 0%
	// and 100% CPU across the microarchitectures AWS runs.
	MinWattsPerVCPU = 0.74
	MaxWattsPerVCPU = 3.5
	// DefaultUtilization is the average CPU utilization assumed for a
	// hyperscale data centre, used where the job's own is not known.
	DefaultUtilization = 0.5
)

// maxVCPU and maxDuration bound what is estimated: far past any shape a cloud
// sells and any job GitHub lets run, and far inside what an int64 of
// microjoules holds (1,024 vCPUs at 3.5 W for a year is about 1.1e17).
const (
	maxVCPU     = 1 << 10
	maxDuration = 366 * 24 * time.Hour
)

// Estimate is what one job drew by the model, and what the model was given.
type Estimate struct {
	Microjoules int64
	VCPU        int64
	Duration    time.Duration
	Utilization float64
}

// Of estimates a job of vcpu vCPUs that ran for d at DefaultUtilization. It
// answers false for a shape or a duration that is not recorded or not
// plausible, and for one too short to come to a microjoule, which is a job with
// no estimate, never one of zero.
func Of(vcpu int64, d time.Duration) (Estimate, bool) {
	if vcpu <= 0 || vcpu > maxVCPU || d <= 0 || d > maxDuration {
		return Estimate{}, false
	}

	watts := float64(vcpu) * WattsPerVCPU(DefaultUtilization)
	microjoules := math.Round(watts * d.Seconds() * 1e6)
	if microjoules < 1 {
		return Estimate{}, false
	}

	return Estimate{Microjoules: int64(microjoules), VCPU: vcpu, Duration: d,
		Utilization: DefaultUtilization}, true
}

// WattsPerVCPU is the model's average draw of one vCPU at utilization u, a
// fraction from 0 to 1.
func WattsPerVCPU(u float64) float64 {
	return MinWattsPerVCPU + u*(MaxWattsPerVCPU-MinWattsPerVCPU)
}

// Method says how an estimate was made, in the words every report uses for it.
func (e Estimate) Method() string {
	// ROUNDED TO THE UNIT OF ITS OWN SIZE, so a duration under a second keeps
	// its milliseconds, one under a millisecond its microseconds, and none reads
	// as 0s.
	d := e.Duration
	switch {
	case d >= time.Second:
		d = d.Round(time.Second)
	case d >= time.Millisecond:
		d = d.Round(time.Millisecond)
	case d >= time.Microsecond:
		d = d.Round(time.Microsecond)
	}

	return fmt.Sprintf("estimated, not measured: %d vCPU for %s at an assumed %.0f%% CPU, %.2f to %.2f W "+
		"per vCPU (Cloud Carbon Footprint's AWS averages; memory, storage and the data centre left out)",
		e.VCPU, d, e.Utilization*100, MinWattsPerVCPU, MaxWattsPerVCPU)
}
