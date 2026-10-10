package fleetops

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/usage"
)

// maxChartWindows is the most windows a chart is drawn from. A series with no
// more sample intervals than this is drawn one interval to a window; a longer
// one is cut into this many equal windows, and the page says so. It is what
// bounds a chart's size whatever the series holds.
const maxChartWindows = 720

// chartHeight and chartWidth are every chart's coordinate system: the series'
// first sample is at x 0 and its last at chartWidth, whatever its length, so a
// coordinate is never longer than chartWidth's digits and a chart's size is
// bounded by its windows alone.
const (
	chartHeight = 100
	chartWidth  = 100_000
)

// pageCharts is the charts section of the job page.
type pageCharts struct {
	// Why says why nothing is drawn; every other field is empty when it is set.
	Why string
	// Origin and Width are the offset of the series' first sample and the
	// milliseconds to its last, which the x axis maps onto 0 to Units; X0 and
	// X1 label them.
	Origin, Width, Units int64
	X0, X1               string
	// Focusable says some chart was drawn, so a step can be focused on it.
	Focusable bool
	// Start and End are the series' ends on the host's clock, Length its
	// duration, and Drawn how it was cut into windows to draw.
	Start, End, Length, Drawn string
	Charts                    []pageChart
	// Bands are the steps on the charts' x axis, the same on every chart.
	Bands []chartBand
}

// pageChart is one chart: its lines, or why it is not drawn.
type pageChart struct {
	Title string
	// Unmeasured is "not measured" for a group the host never read; nothing
	// else of the chart is drawn then.
	Unmeasured string
	// Top labels the value at the top of the chart.
	Top   string
	Lines []chartLine
	// Limit is the lease's own bound drawn across the chart, nil when the
	// ledger records none.
	Limit *chartLimit
	Note  string
}

// chartLine is one series drawn on a chart, as an SVG path.
type chartLine struct {
	Class, Label, D string
}

// chartLimit is a horizontal line at Y with its label.
type chartLimit struct {
	Y     string
	Label string
}

// chartBand is one step's window on the charts' x axis. Key is the step's
// place in GitHub's list, which a step number need not be unique enough for.
type chartBand struct {
	X, W  int64
	Class string
	Key   int
}

// chartWindow is one window of wall time a chart's point stands for.
type chartWindow struct {
	from, to time.Time
}

// buildCharts draws the series the steps ran against. Without a report or a
// series on the host's clock it draws nothing and says why.
func buildCharts(tl *usage.Timeline, unaligned string, rec alloc.JobRecord, m *alloc.RecordedUsage,
	steps []github.JobStep,
) pageCharts {
	switch {
	case m == nil:
		return pageCharts{Why: "no usage report was recorded, so there is nothing to draw"}
	case tl == nil:
		return pageCharts{Why: "nothing is drawn, because " + unaligned}
	case len(tl.Points) < 2 || tl.Points[len(tl.Points)-1].OffsetMillis == tl.Points[0].OffsetMillis:
		return pageCharts{Why: "the series holds no interval to draw (its samples are all at one instant)"}
	}
	first := tl.Points[0].OffsetMillis
	span := tl.Points[len(tl.Points)-1].OffsetMillis
	out := pageCharts{
		Origin: first, Width: span - first, Units: chartWidth, X0: offsetLabel(first), X1: offsetLabel(span),
		Start:  stamp(tl.First.Add(time.Duration(first) * time.Millisecond)),
		End:    stamp(tl.First.Add(time.Duration(span) * time.Millisecond)),
		Length: (time.Duration(span-first) * time.Millisecond).String(),
	}
	windows, sampled := chartWindows(*tl)
	if sampled {
		out.Drawn = fmt.Sprintf("Every one of the series' %s samples is drawn: a rate is the average over "+
			"each sample interval.", groupDigits(int64(len(tl.Points))))
	} else {
		out.Drawn = fmt.Sprintf("Downsampled for drawing: the series' %s samples are drawn as %d windows "+
			"of %s each, a rate as the average over its window and memory as the highest level sampled "+
			"in it.", groupDigits(int64(len(tl.Points))), len(windows), windows[0].to.Sub(windows[0].from))
	}
	used := make([]usage.WindowUsage, len(windows))
	for i, w := range windows {
		used[i] = windowOf(*tl, w.from, w.to)
	}
	x := func(t time.Time) int64 { return out.at(t.Sub(tl.First).Milliseconds()) }

	cpu := pageChart{Title: "CPU (cores busy)"}
	if !m.Measured(alloc.UsageCPU) {
		cpu.Unmeasured = "not measured"
	} else {
		var limit float64
		if rec.VCPU > 0 {
			limit = float64(rec.VCPU)
		}
		cpu.Top, cpu.Lines, cpu.Limit = rateChart(windows, used, x, limit,
			func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + " cores" },
			rateSeries{"a", "cpu", func(u usage.WindowUsage) float64 {
				return float64(u.CPUMicros) / 1e6 / u.Covered.Seconds()
			}})
		if cpu.Limit != nil {
			cpu.Limit.Label = fmt.Sprintf("the lease's %d vCPU", rec.VCPU)
		}
	}

	memory := pageChart{Title: "Memory (the cgroup's level)"}
	if !m.Measured(alloc.UsageMemory) {
		memory.Unmeasured = "not measured"
	} else {
		memory.Top, memory.Lines, memory.Limit = memoryChart(*tl, windows, used, x, sampled, rec.Memory)
		if memory.Limit != nil {
			memory.Limit.Label = "tier memory " + cli.HumanBytes(rec.Memory)
		}
		memory.Note = "For a microVM the level counts the VMM and the host's page cache for its disk, " +
			"so it can pass the guest's memory."
	}

	perSecond := func(f float64) string { return floatBytes(f) + "/s" }
	disk := pageChart{Title: "Disk (bytes a second, host IO)"}
	if !m.Measured(alloc.UsageIO) {
		disk.Unmeasured = "not measured"
	} else {
		disk.Top, disk.Lines, _ = rateChart(windows, used, x, 0, perSecond,
			rateSeries{"a", "read", bytesPerSecond(func(u usage.WindowUsage) int64 { return u.DiskRead })},
			rateSeries{"b", "written", bytesPerSecond(func(u usage.WindowUsage) int64 { return u.DiskWrite })})
		disk.Note = "IO that missed the host's page cache, plus writeback: not what the guest asked for."
	}

	network := pageChart{Title: "Network (bytes a second, the guest's view)"}
	if !m.Measured(alloc.UsageNet) {
		network.Unmeasured = "not measured"
	} else {
		network.Top, network.Lines, _ = rateChart(windows, used, x, 0, perSecond,
			rateSeries{"a", "received", bytesPerSecond(func(u usage.WindowUsage) int64 { return u.NetRx })},
			rateSeries{"b", "sent", bytesPerSecond(func(u usage.WindowUsage) int64 { return u.NetTx })})
	}
	out.Charts = []pageChart{cpu, memory, disk, network}
	for _, c := range out.Charts {
		out.Focusable = out.Focusable || c.Unmeasured == ""
	}

	for i, step := range steps {
		from, to, ok := out.stepWindow(step, tl)
		if !ok {
			continue
		}
		out.Bands = append(out.Bands, chartBand{X: from, W: to - from, Class: "band" + strconv.Itoa(i%2), Key: i})
	}

	return out
}

// at is an offset from the series' first sample on the charts' x axis.
func (c pageCharts) at(ms int64) int64 {
	return int64(math.Round(float64(ms-c.Origin) / float64(c.Width) * chartWidth))
}

// offsetLabel writes an offset from the series' first sample as the page's
// script does when it focuses a step: +1m05s, +2h03m09s.
func offsetLabel(ms int64) string {
	s := (ms + 500) / 1000
	h, m := s/3600, s/60%60
	s %= 60
	if h > 0 {
		return fmt.Sprintf("+%dh%02dm%02ds", h, m, s)
	}

	return fmt.Sprintf("+%dm%02ds", m, s)
}

// chartWindows cuts a timeline into the windows a chart is drawn from: its
// sample intervals when there are at most maxChartWindows of them, and
// otherwise that many equal windows. It reports whether every sample is drawn.
func chartWindows(tl usage.Timeline) ([]chartWindow, bool) {
	at := func(ms int64) time.Time { return tl.First.Add(time.Duration(ms) * time.Millisecond) }
	var offsets []int64
	for _, p := range tl.Points {
		if n := len(offsets); n == 0 || offsets[n-1] != p.OffsetMillis {
			offsets = append(offsets, p.OffsetMillis)
		}
	}
	if len(offsets)-1 <= maxChartWindows {
		out := make([]chartWindow, 0, len(offsets)-1)
		for i := 1; i < len(offsets); i++ {
			out = append(out, chartWindow{at(offsets[i-1]), at(offsets[i])})
		}
		return out, true
	}
	first, last := at(offsets[0]), at(offsets[len(offsets)-1])
	width := last.Sub(first) / maxChartWindows
	out := make([]chartWindow, maxChartWindows)
	for i := range out {
		out[i] = chartWindow{first.Add(time.Duration(i) * width), first.Add(time.Duration(i+1) * width)}
	}
	out[len(out)-1].to = last

	return out, false
}

// windowOf is tl.Window(from, to), read from only the points the window
// depends on.
//
// Window settles the whole series on every call, and a chart asks for hundreds
// of windows of a series that can hold over a hundred thousand points. A window
// reads only the intervals that overlap it, and settling a point looks back
// over the two offsets before it (a counter that fell into the point before,
// and the catch-up after), so the points from three offsets before the
// window's first interval to the whole run of samples at its end settle there
// exactly as the whole series does. TestWindowOfIsWindow holds the two equal
// over random series with gaps, falls and repeated offsets.
func windowOf(tl usage.Timeline, from, to time.Time) usage.WindowUsage {
	pts := tl.Points
	a, b := from.Sub(tl.First), to.Sub(tl.First)
	offset := func(i int) time.Duration { return time.Duration(pts[i].OffsetMillis) * time.Millisecond }

	start := sort.Search(len(pts), func(i int) bool { return offset(i) > a })
	for range 3 {
		if start == 0 {
			break
		}
		run := pts[start-1].OffsetMillis
		for start > 0 && pts[start-1].OffsetMillis == run {
			start--
		}
	}
	end := sort.Search(len(pts), func(i int) bool { return offset(i) >= b })
	if end < len(pts) {
		run := pts[end].OffsetMillis
		for end < len(pts) && pts[end].OffsetMillis == run {
			end++
		}
	}

	return usage.Timeline{First: tl.First, Points: pts[start:end]}.Window(from, to)
}

// rateSeries is one line of a rate chart and its rate over a window, read
// only for a window the series covers some of.
type rateSeries struct {
	class, label string
	rate         func(usage.WindowUsage) float64
}

// bytesPerSecond is a byte count's rate over the part of a window the series
// covers.
func bytesPerSecond(count func(usage.WindowUsage) int64) func(usage.WindowUsage) float64 {
	return func(u usage.WindowUsage) float64 { return float64(count(u)) / u.Covered.Seconds() }
}

// rateChart draws each series as a step line, one level for each window the
// series covered and a break for each it did not, scaled so the highest value
// or the limit, whichever is larger, sits near the top.
func rateChart(windows []chartWindow, used []usage.WindowUsage, x func(time.Time) int64, limit float64,
	label func(float64) string, series ...rateSeries,
) (string, []chartLine, *chartLimit) {
	rates := make([][]float64, len(series))
	highest := limit
	for s, line := range series {
		rates[s] = make([]float64, len(windows))
		for i, u := range used {
			rates[s][i] = math.NaN()
			if u.Covered <= 0 {
				continue
			}
			rate := line.rate(u)
			rates[s][i] = rate
			highest = max(highest, rate)
		}
	}
	top := chartTop(highest)
	lines := make([]chartLine, len(series))
	for s, line := range series {
		var p svgPath
		for i, w := range windows {
			if math.IsNaN(rates[s][i]) {
				p.lift()
				continue
			}
			p.level(x(w.from), x(w.to), chartY(rates[s][i], top))
		}
		lines[s] = chartLine{Class: line.class, Label: line.label, D: p.String()}
	}
	var lim *chartLimit
	if limit > 0 {
		lim = &chartLimit{Y: chartY(limit, top)}
	}

	return label(top), lines, lim
}

// memoryChart draws the memory level: the highest level sampled at each
// offset when every sample is drawn, and otherwise the highest level sampled in
// each window, with a break for a window that holds no sample.
//
// A LEVEL IS NEVER DRAWN ACROSS TIME THE SAMPLER DID NOT SEE: the line breaks
// before a sample marked AfterGap, and a sample with breaks on both sides is a
// dot.
func memoryChart(tl usage.Timeline, windows []chartWindow, used []usage.WindowUsage, x func(time.Time) int64,
	sampled bool, limitBytes int64,
) (string, []chartLine, *chartLimit) {
	highest := float64(max(limitBytes, 0))
	for _, p := range tl.Points {
		highest = max(highest, float64(p.MemoryCurrent))
	}
	top := chartTop(highest)
	var p svgPath
	if sampled {
		// ONE POINT AN OFFSET, so samples sharing one cannot grow the path past
		// what chartWindows bounded.
		for i := 0; i < len(tl.Points); {
			at, level, gap := tl.Points[i].OffsetMillis, tl.Points[i].MemoryCurrent, false
			for ; i < len(tl.Points) && tl.Points[i].OffsetMillis == at; i++ {
				level = max(level, tl.Points[i].MemoryCurrent)
				gap = gap || tl.Points[i].AfterGap
			}
			px, y := x(tl.First.Add(time.Duration(at)*time.Millisecond)), chartY(float64(level), top)
			if gap || !p.down {
				p.lift()
				p.level(px, px, y)
				continue
			}
			p.to(px, y)
		}
	} else {
		for i, w := range windows {
			if used[i].Samples == 0 {
				p.lift()
				continue
			}
			p.level(x(w.from), x(w.to), chartY(float64(used[i].MemoryPeak), top))
		}
	}
	var lim *chartLimit
	if limitBytes > 0 {
		lim = &chartLimit{Y: chartY(float64(limitBytes), top)}
	}

	return floatBytes(top), []chartLine{{Class: "a", Label: "memory", D: p.String()}}, lim
}

// floatBytes writes a byte count held as a float, as cli.HumanBytes does for
// one that fits an int64, without converting one that does not: a chart's top
// is padded above the highest value, and a rate can pass the largest int64.
func floatBytes(f float64) string {
	if f < math.MaxInt64/2 {
		return cli.HumanBytes(int64(math.Round(f)))
	}

	return fmt.Sprintf("%.1f EiB", f/(1<<60))
}

// chartTop is the value at the top of a chart: a little above the highest, so
// a line at the highest value is not drawn on the frame. A chart of zeros has
// a top of one unit.
func chartTop(highest float64) float64 {
	if highest <= 0 || math.IsNaN(highest) {
		return 1
	}

	return highest * 1.08
}

// chartY is a value's y coordinate on a chart whose top is top.
func chartY(v, top float64) string {
	return strconv.FormatFloat(chartHeight-v/top*chartHeight, 'f', 2, 64)
}

// svgPath writes an SVG path's data: lines that a lift breaks.
type svgPath struct {
	b    strings.Builder
	down bool
	x    int64
}

// lift ends the line being drawn; the next point starts a new one.
func (p *svgPath) lift() { p.down = false }

// to draws a line to (x, y), or starts one there.
func (p *svgPath) to(x int64, y string) {
	if p.down {
		fmt.Fprintf(&p.b, "L%d %s", x, y)
	} else {
		fmt.Fprintf(&p.b, "M%d %s", x, y)
	}
	p.down, p.x = true, x
}

// level draws a level at y from x0 to x1, joining the line drawn so far when it
// ended at x0.
func (p *svgPath) level(x0, x1 int64, y string) {
	if p.down && p.x == x0 {
		fmt.Fprintf(&p.b, "V%s", y)
	} else {
		fmt.Fprintf(&p.b, "M%d %s", x0, y)
	}
	fmt.Fprintf(&p.b, "H%d", x1)
	p.down, p.x = true, x1
}

func (p *svgPath) String() string { return p.b.String() }

// stepWindow is a step's window on the charts' x axis, clipped to the series,
// and whether any of it lies there. A step without both times, or with no
// length, has none, and so has every step when no chart was drawn.
func (c pageCharts) stepWindow(step github.JobStep, tl *usage.Timeline) (int64, int64, bool) {
	if !c.Focusable || tl == nil || step.StartedAt.IsZero() || step.CompletedAt.IsZero() ||
		!step.CompletedAt.After(step.StartedAt) {
		return 0, 0, false
	}
	from := max(step.StartedAt.Sub(tl.First).Milliseconds(), c.Origin)
	to := min(step.CompletedAt.Sub(tl.First).Milliseconds(), c.Origin+c.Width)
	if to <= from {
		return 0, 0, false
	}

	return c.at(from), c.at(to), true
}
