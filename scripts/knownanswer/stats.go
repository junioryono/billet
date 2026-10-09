package main

import (
	"fmt"
	"math"
)

// summary is the spread of one metric across runs.
type summary struct {
	n        int
	mean, sd float64
	// ci95 is the half-width of the 95% confidence interval for the mean,
	// Student's t on n-1 degrees of freedom; NaN with fewer than two runs.
	ci95 float64
}

func summarize(xs []float64) summary {
	s := summary{n: len(xs), sd: math.NaN(), ci95: math.NaN()}
	if s.n == 0 {
		s.mean = math.NaN()

		return s
	}
	for _, x := range xs {
		s.mean += x
	}
	s.mean /= float64(s.n)
	if s.n < 2 {
		return s
	}
	var ss float64
	for _, x := range xs {
		ss += (x - s.mean) * (x - s.mean)
	}
	s.sd = math.Sqrt(ss / float64(s.n-1))
	s.ci95 = tCritical95(s.n-1) * s.sd / math.Sqrt(float64(s.n))

	return s
}

func (s summary) ciText(format func(float64) string) string {
	if math.IsNaN(s.ci95) {
		return "n/a (one run)"
	}

	return "±" + format(s.ci95)
}

// cvText is the coefficient of variation, sd over the mean's magnitude. It is
// meaningless for a mean near zero (the idle job's), and says so.
func (s summary) cvText() string {
	if math.IsNaN(s.sd) {
		return "n/a (one run)"
	}
	if math.Abs(s.mean) < 1e-9 {
		return "n/a (mean is zero)"
	}

	return fmt.Sprintf("%.1f%%", 100*s.sd/math.Abs(s.mean))
}

// tTable is Student's t at 0.975 for 1 to 30 degrees of freedom.
var tTable = []float64{
	12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
}

// tCritical95 is the two-sided 95% critical value. Past 30 degrees of freedom
// it takes the next tabulated row DOWN, which is the wider interval, so a
// rounding error never narrows the claim.
func tCritical95(df int) float64 {
	switch {
	case df < 1:
		return math.NaN()
	case df <= len(tTable):
		return tTable[df-1]
	case df < 40:
		return tTable[len(tTable)-1]
	case df < 60:
		return 2.021
	case df < 120:
		return 2.000
	default:
		return 1.980
	}
}
