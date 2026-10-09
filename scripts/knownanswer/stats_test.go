package main

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

func TestSummarizeIsTheTextbookMeanAndInterval(t *testing.T) {
	s := summarize([]float64{1, 2, 3, 4, 5})
	// sd = sqrt(2.5); half-width = t(0.975, 4) x sd / sqrt(5) = 2.776 x 1.5811 / 2.2361.
	if s.n != 5 || s.mean != 3 || !near(s.sd, 1.5811) || !near(s.ci95, 1.9630) {
		t.Errorf("summary = %+v", s)
	}
	if got := s.cvText(); got != "52.7%" {
		t.Errorf("cv = %q, want 52.7%%", got)
	}

	one := summarize([]float64{7})
	if one.mean != 7 || !math.IsNaN(one.ci95) || one.ciText(func(float64) string { return "x" }) != "n/a (one run)" {
		t.Errorf("one run = %+v", one)
	}
	if got := summarize([]float64{-1, 1}).cvText(); got != "n/a (mean is zero)" {
		t.Errorf("cv of a zero mean = %q", got)
	}
}

// PAST THE TABLE THE INTERVAL ONLY WIDENS: each degree of freedom takes the row
// at or below it.
func TestTheCriticalValueNeverNarrowsTheInterval(t *testing.T) {
	for df, want := range map[int]float64{1: 12.706, 4: 2.776, 30: 2.042, 35: 2.042, 40: 2.021, 59: 2.021,
		60: 2.000, 119: 2.000, 120: 1.980, 10000: 1.980} {
		if got := tCritical95(df); got != want {
			t.Errorf("t(%d) = %v, want %v", df, got, want)
		}
	}
	if !math.IsNaN(tCritical95(0)) {
		t.Errorf("t(0) is a number")
	}
}
