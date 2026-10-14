package fleetops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage"
)

// window is the span of wall time `jobs show --window` asks about.
type window struct{ from, to time.Time }

// parseWindow reads FROM,TO, each RFC 3339 on the host's clock.
func parseWindow(s string) (window, error) {
	a, b, ok := strings.Cut(s, ",")
	if !ok {
		return window{}, fmt.Errorf("--window %q is not FROM,TO", s)
	}

	from, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(a))
	if err != nil {
		return window{}, fmt.Errorf("--window's FROM: %w", err)
	}

	to, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(b))
	if err != nil {
		return window{}, fmt.Errorf("--window's TO: %w", err)
	}

	if !to.After(from) {
		return window{}, errors.New("--window's TO must be after its FROM")
	}

	return window{from: from, to: to}, nil
}

// windowJSON is what the lease's series shows in a window of wall time,
// integrated as a step's usage is. Usage is null when the series cannot say,
// and Unavailable says why: could-not-tell is never a row of zeros.
//
// USAGE SPLITS THE EDGE INTERVALS BY TIME, which is a guess about when inside
// an interval the counters moved: a burst inside one interval is given to the
// window in proportion to the overlap, whatever side of the edge it fell on.
// Inner and Outer are what the series knows for certain, the whole sample
// intervals inside the window and the whole ones overlapping it, so the true
// figure lies between them; either is null when no such span of samples
// exists.
type windowJSON struct {
	From        string           `json:"from"`
	To          string           `json:"to"`
	Usage       *windowUsageJSON `json:"usage"`
	Inner       *windowSpanJSON  `json:"inner"`
	Outer       *windowSpanJSON  `json:"outer"`
	Unavailable string           `json:"unavailable,omitempty"`
}

// windowSpanJSON is a span of whole sample intervals and its usage.
type windowSpanJSON struct {
	From  string          `json:"from"`
	To    string          `json:"to"`
	Usage windowUsageJSON `json:"usage"`
}

// windowUsageJSON is the usage over the covered part of the window. Complete
// is true only when the series covers all of it; otherwise every quantity is
// over CoveredMillis, not the whole span.
type windowUsageJSON struct {
	SpanMillis      int64 `json:"span_ms"`
	CoveredMillis   int64 `json:"covered_ms"`
	Complete        bool  `json:"complete"`
	Samples         int   `json:"samples"`
	CPUMicros       int64 `json:"cpu_us"`
	DiskReadBytes   int64 `json:"disk_read_bytes"`
	DiskWriteBytes  int64 `json:"disk_write_bytes"`
	NetRxBytes      int64 `json:"net_rx_bytes"`
	NetTxBytes      int64 `json:"net_tx_bytes"`
	EnergyActiveUJ  int64 `json:"energy_active_uj"`
	MemoryPeakBytes int64 `json:"memory_peak_bytes"`
}

// windowJSONOf reads the lease's series from the ledger and integrates it over
// w. It asks GitHub nothing.
func windowJSONOf(ctx context.Context, a *alloc.Allocator, leaseID string, measured *alloc.RecordedUsage,
	w window,
) *windowJSON {
	out := &windowJSON{From: wallStamp(w.from), To: wallStamp(w.to)}

	tl, why := jobTimeline(ctx, a, leaseID, measured)
	if tl == nil {
		out.Unavailable = why

		return out
	}

	u := usageJSONOf(tl.Window(w.from, w.to))
	out.Usage = &u

	inner, outer := sampleSpans(*tl, w)
	if inner != nil {
		out.Inner = &windowSpanJSON{From: wallStamp(inner.from), To: wallStamp(inner.to),
			Usage: usageJSONOf(tl.Window(inner.from, inner.to))}
	}

	if outer != nil {
		out.Outer = &windowSpanJSON{From: wallStamp(outer.from), To: wallStamp(outer.to),
			Usage: usageJSONOf(tl.Window(outer.from, outer.to))}
	}

	return out
}

func usageJSONOf(u usage.WindowUsage) windowUsageJSON {
	return windowUsageJSON{
		SpanMillis: u.Span.Milliseconds(), CoveredMillis: u.Covered.Milliseconds(), Complete: u.Complete(),
		Samples: u.Samples, CPUMicros: u.CPUMicros, DiskReadBytes: u.DiskRead, DiskWriteBytes: u.DiskWrite,
		NetRxBytes: u.NetRx, NetTxBytes: u.NetTx, EnergyActiveUJ: u.EnergyActive, MemoryPeakBytes: u.MemoryPeak,
	}
}

// sampleSpans answers the spans of whole sample intervals inside w (from the
// first sample at or after its start to the last at or before its end) and
// over it (from the last sample at or before its start to the first at or
// after its end). Either is nil when the series has no such samples.
func sampleSpans(tl usage.Timeline, w window) (*window, *window) {
	var (
		innerFrom, innerTo, outerFrom, outerTo time.Time
		haveInnerFrom, haveOuterTo             bool
		haveInnerTo, haveOuterFrom             bool
	)

	for _, p := range tl.Points {
		at := tl.First.Add(time.Duration(p.OffsetMillis) * time.Millisecond)

		if !at.After(w.from) {
			outerFrom, haveOuterFrom = at, true
		}

		if !at.Before(w.from) && !haveInnerFrom {
			innerFrom, haveInnerFrom = at, true
		}

		if !at.After(w.to) {
			innerTo, haveInnerTo = at, true
		}

		if !at.Before(w.to) && !haveOuterTo {
			outerTo, haveOuterTo = at, true
		}
	}

	var inner, outer *window
	if haveInnerFrom && haveInnerTo && innerTo.After(innerFrom) {
		inner = &window{from: innerFrom, to: innerTo}
	}

	if haveOuterFrom && haveOuterTo && outerTo.After(outerFrom) {
		outer = &window{from: outerFrom, to: outerTo}
	}

	return inner, outer
}

// wallStamp is a wall time as the window's JSON writes it.
func wallStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
