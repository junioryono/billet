package main

import (
	"fmt"
	"time"
)

// recordWindow is what `billet jobs show --json --window` adds: the lease's
// series integrated over the window asked for, the spans of whole sample
// intervals inside it and over it, or why there is none.
type recordWindow struct {
	From        string       `json:"from"`
	To          string       `json:"to"`
	Usage       *windowUsage `json:"usage"`
	Inner       *windowSpan  `json:"inner"`
	Outer       *windowSpan  `json:"outer"`
	Unavailable string       `json:"unavailable"`
}

// windowSpan is a span of whole sample intervals and its usage.
type windowSpan struct {
	From  string      `json:"from"`
	To    string      `json:"to"`
	Usage windowUsage `json:"usage"`
}

// windowUsage is the part of a window's usage this checker reads. A counter
// the record did not carry is nil, never zero.
type windowUsage struct {
	SpanMillis     int64  `json:"span_ms"`
	CoveredMillis  int64  `json:"covered_ms"`
	Complete       bool   `json:"complete"`
	DiskWriteBytes *int64 `json:"disk_write_bytes"`
}

// loadWindow is the wall time a job's load ran in, from its expectation.
//
// ONE SECOND IS ADDED TO THE END: the job stamps both ends with `date` to the
// second, so its start is at or after the first stamp and its end before the
// second stamp's next second. What the window holds besides the load is the
// shell between the stamps, never the job's setup.
func loadWindow(e *expectation) (time.Time, time.Time, bool) {
	from, err := time.Parse(time.RFC3339Nano, e.LoadStartedAt)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}

	end, err := time.Parse(time.RFC3339Nano, e.LoadFinishedAt)
	if err != nil || end.Before(from) {
		return time.Time{}, time.Time{}, false
	}

	return from, end.Add(time.Second), true
}

// stamp is a wall time as collect asks billet for it and as billet answers.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// windowArg is the --window collect passes for a job, or "" when its
// expectation gives no load window.
func windowArg(e *expectation) string {
	from, to, ok := loadWindow(e)
	if !ok {
		return ""
	}

	return stamp(from) + "," + stamp(to)
}

// compareWindow compares what billet measured over the job's own load window
// with the expectation, when the record carries that window. No other job is
// subtracted, so another job's setup cannot move the verdict.
//
// THE VERDICT IS TAKEN ON THE BOUNDS, NOT THE SPLIT. What the window holds is
// known only to lie between its inner span (whole sample intervals inside it)
// and its outer span (whole ones over it): a burst inside an edge interval
// may be on either side of the edge. PASS needs both inside the tolerance,
// FAIL both outside it on the same side, and anything between is UNMEASURED,
// with what the edges could hold.
func compareWindow(r result, e *expectation, m *metric, job record) result {
	w := job.Window
	r.metric.rule = m.windowRule

	from, to, ok := loadWindow(e)
	if !ok {
		r.reason = "the expectation gives no load window to compare over"

		return r
	}

	gotFrom, errFrom := time.Parse(time.RFC3339Nano, w.From)
	gotTo, errTo := time.Parse(time.RFC3339Nano, w.To)

	if errFrom != nil || errTo != nil || !gotFrom.Equal(from) || !gotTo.Equal(to) {
		r.reason = fmt.Sprintf("the record's window is %s to %s, not this job's load window %s to %s; collect "+
			"it again", w.From, w.To, stamp(from), stamp(to))

		return r
	}

	r.against = fmt.Sprintf("its own load window, %s to %s", w.From, w.To)

	switch {
	case w.Unavailable != "" || w.Usage == nil:
		r.reason = "billet could not integrate the load window: " + w.Unavailable

		return r
	case !covers(w.Usage, from, to):
		r.reason = fmt.Sprintf("billet's series covers %d ms of the %d ms load window, so what it shows is not "+
			"the load's whole", w.Usage.CoveredMillis, w.Usage.SpanMillis)

		return r
	case w.Usage.DiskWriteBytes == nil:
		r.reason = "the record's window carries no disk_write_bytes"

		return r
	case w.Outer == nil || !spanCovers(w.Outer) || w.Outer.Usage.DiskWriteBytes == nil:
		r.reason = "billet's series does not cover the whole sample intervals over the load window, so what its " +
			"edges hold cannot be bounded"

		return r
	}

	// NO WHOLE INTERVAL INSIDE THE WINDOW BOUNDS IT BELOW BY NOTHING.
	inner := 0.0
	if w.Inner != nil {
		if !spanCovers(w.Inner) || w.Inner.Usage.DiskWriteBytes == nil {
			r.reason = "billet's series does not cover the whole sample intervals inside the load window"

			return r
		}

		inner = float64(*w.Inner.Usage.DiskWriteBytes)
	}

	outer := float64(*w.Outer.Usage.DiskWriteBytes)

	if why := boundsDisagree(w, from, to, inner, outer); why != "" {
		r.reason = "the record's bounds on the load window " + why

		return r
	}

	r.value = m.windowed(w.Usage)
	r.low, r.high = m.bounds(r.expected, float64(e.Seconds), nil)

	if r.low <= 0 {
		r.reason = fmt.Sprintf("the load is too small to compare: %s accepts [%s, %s], which holds nothing "+
			"written; run a larger one", r.metric.rule, formatValue(r.low, m.unit), formatValue(r.high, m.unit))

		return r
	}

	switch {
	case inner >= r.low && outer <= r.high:
		r.compared, r.verdict = true, pass
	case outer < r.low || inner > r.high:
		r.compared, r.verdict = true, fail
	default:
		r.reason = fmt.Sprintf("the load window held between %s (its whole sample intervals) and %s (with the "+
			"intervals at its edges), which the tolerance [%s, %s] cannot decide; the job should be quiet for a "+
			"sample interval at each end of its load", formatValue(inner, m.unit), formatValue(outer, m.unit),
			formatValue(r.low, m.unit), formatValue(r.high, m.unit))
	}

	return r
}

// boundsDisagree answers why a record's inner and outer spans cannot be the
// bounds of the window from..to, or "" when they can: the inner span inside
// the window, the window inside the outer span, and inner no more than outer,
// none of it negative. A record that says otherwise decides no verdict.
func boundsDisagree(w *recordWindow, from, to time.Time, inner, outer float64) string {
	span := func(s *windowSpan) (time.Time, time.Time, bool) {
		a, errA := time.Parse(time.RFC3339Nano, s.From)
		b, errB := time.Parse(time.RFC3339Nano, s.To)

		return a, b, errA == nil && errB == nil && b.After(a)
	}

	outerFrom, outerTo, ok := span(w.Outer)
	if !ok || outerFrom.After(from) || outerTo.Before(to) {
		return fmt.Sprintf("are not bounds: the outer span %s to %s does not hold the window", w.Outer.From, w.Outer.To)
	}

	if w.Inner != nil {
		innerFrom, innerTo, ok := span(w.Inner)
		if !ok || innerFrom.Before(from) || innerTo.After(to) {
			return fmt.Sprintf("are not bounds: the inner span %s to %s is not inside the window", w.Inner.From,
				w.Inner.To)
		}
	}

	if inner < 0 || inner > outer {
		return fmt.Sprintf("are not bounds: %.0f B inside and %.0f B over it", inner, outer)
	}

	return ""
}

// covers reports whether a usage covers the whole of from..to by its own
// account: complete, and covering as many milliseconds as it spans and as the
// window lasts. A record whose figures disagree with each other covers nothing.
func covers(u *windowUsage, from, to time.Time) bool {
	span := to.Sub(from).Milliseconds()

	return u.Complete && span > 0 && u.SpanMillis == span && u.CoveredMillis == span
}

// spanCovers is covers for a span, by the span's own times.
func spanCovers(s *windowSpan) bool {
	from, errFrom := time.Parse(time.RFC3339Nano, s.From)
	to, errTo := time.Parse(time.RFC3339Nano, s.To)

	return errFrom == nil && errTo == nil && covers(&s.Usage, from, to)
}
