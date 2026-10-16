package fleetops

import (
	"fmt"
	"io"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage"
)

// maxGapsShown bounds the gaps the text report and the page list one by one,
// and maxGapsListed the gaps a JSON record lists; past either, the rest are a
// count and a total. A series whose sampler failed every other read marks tens
// of thousands, and a record of them would bury everything else.
const (
	maxGapsShown  = 20
	maxGapsListed = 1000
)

// gapMeaning is what each cause says about the interval, in the words the
// text report and the page print beside its name.
var gapMeaning = map[usage.GapCause]string{
	usage.GapUnspecified:   "not seen; the node or control plane that kept it records no cause",
	usage.GapMissedTick:    "the sampler took no sample for two intervals or more",
	usage.GapSlowRead:      "a read of the job took a whole interval",
	usage.GapFailedRead:    "a counter the sampler had read before could not be read",
	usage.GapLateFirstRead: "before a counter was first read, which every sample until then held at zero",
	usage.GapCounterFell:   "a counter fell, or caught up after falling",
	usage.GapFinalEnergy:   "the last interval, whose package energy the final read does not take",
	usage.GapSamplerStuck:  "the last interval, whose final read the sampler did not answer in time",
	usage.GapHandover:      "between the node process that handed the job over and the one that adopted it",
	usage.GapSeveral:       "intervals with different causes, merged when the series was halved",
}

// gapText is one gap as the text report and the page word it.
func gapText(g usage.Gap) string {
	meaning, ok := gapMeaning[g.Cause]
	if !ok {
		meaning = "a cause this build does not know"
	}

	return fmt.Sprintf("%s to %s (%s): %s, %s", milliStamp(g.From), milliStamp(g.To),
		g.Length().Round(time.Millisecond), g.Cause, meaning)
}

// milliStamp is a wall time to the millisecond, which is what a series keeps.
func milliStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// seriesView is what a job's series says about its own sampling: when it ran,
// how many samples it holds, and every interval it did not see.
type seriesView struct {
	head string
	gaps []string
	more string
}

// seriesViewOf words the series for the text report and the page. Without a
// report it says nothing, since the usage line already says why.
func seriesViewOf(tl *usage.Timeline, unaligned string, m *alloc.RecordedUsage) (seriesView, bool) {
	switch {
	case m == nil:
		return seriesView{}, false
	case tl == nil:
		return seriesView{head: "not placed on the host's clock, because " + unaligned}, true
	case len(tl.Points) == 0:
		return seriesView{head: "holds no samples"}, true
	}

	first := tl.First.Add(time.Duration(tl.Points[0].OffsetMillis) * time.Millisecond)
	last := tl.First.Add(time.Duration(tl.Points[len(tl.Points)-1].OffsetMillis) * time.Millisecond)
	v := seriesView{head: fmt.Sprintf("%s samples on the host's clock from %s to %s",
		groupDigits(int64(len(tl.Points))), milliStamp(first), milliStamp(last))}

	gaps := tl.Gaps()
	if len(gaps) == 0 {
		v.head += "; the sampler saw every interval"

		return v, true
	}

	var unseen time.Duration
	for _, g := range gaps {
		unseen += g.Length()
	}
	noun := "intervals"
	if len(gaps) == 1 {
		noun = "interval"
	}
	v.head += fmt.Sprintf("; %d %s not seen, %s in all, which no window counts", len(gaps), noun,
		unseen.Round(time.Millisecond))

	for i, g := range gaps {
		if i == maxGapsShown {
			v.more = fmt.Sprintf("and %d more", len(gaps)-maxGapsShown)

			break
		}
		v.gaps = append(v.gaps, gapText(g))
	}

	return v, true
}

// renderSeries writes the series section of the text report, labelled as
// renderJob's lines are, or nothing when there is no report.
func renderSeries(w io.Writer, tl *usage.Timeline, unaligned string, m *alloc.RecordedUsage) {
	v, ok := seriesViewOf(tl, unaligned, m)
	if !ok {
		return
	}
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	fmt.Fprintln(w)
	line("series", "%s", v.head)
	for _, g := range v.gaps {
		line("", "%s", g)
	}
	if v.more != "" {
		line("", "%s", v.more)
	}
}

// seriesJSON is the series' account of its own sampling in a JSON record:
// its first and last sample on the host's clock, how many samples it holds,
// how long it did not see in all, and each interval it did not see, the first
// maxGapsListed of them, with GapsMore the count past those.
type seriesJSON struct {
	First        string    `json:"first"`
	Last         string    `json:"last"`
	Samples      int       `json:"samples"`
	UnseenMillis int64     `json:"unseen_ms"`
	Gaps         []gapJSON `json:"gaps"`
	GapsMore     int       `json:"gaps_more"`
}

// gapJSON is one interval the series did not see.
type gapJSON struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Millis int64  `json:"ms"`
	Cause  string `json:"cause"`
}

// seriesJSONOf is a series' account of its own sampling, or nil with no series
// on the host's clock.
func seriesJSONOf(tl *usage.Timeline) *seriesJSON {
	if tl == nil || len(tl.Points) == 0 {
		return nil
	}
	at := func(p usage.Point) string {
		return wallStamp(tl.First.Add(time.Duration(p.OffsetMillis) * time.Millisecond))
	}
	out := &seriesJSON{First: at(tl.Points[0]), Last: at(tl.Points[len(tl.Points)-1]), Samples: len(tl.Points)}
	out.Gaps, out.GapsMore, out.UnseenMillis = gapsJSONOf(tl.Gaps())

	return out
}

// gapsJSONOf lists gaps for a JSON record, the first maxGapsListed of them,
// and answers how many more there are and how long all of them lasted. An
// empty list is an empty array, never null: a series that saw everything
// says so.
func gapsJSONOf(gaps []usage.Gap) ([]gapJSON, int, int64) {
	out := []gapJSON{}
	var unseen time.Duration
	for i, g := range gaps {
		unseen += g.Length()
		if i < maxGapsListed {
			out = append(out, gapJSON{From: wallStamp(g.From), To: wallStamp(g.To),
				Millis: g.Length().Milliseconds(), Cause: g.Cause.String()})
		}
	}

	return out, max(len(gaps)-maxGapsListed, 0), unseen.Milliseconds()
}

// gapsWithin is every gap that overlaps from..to, cut to it.
func gapsWithin(gaps []usage.Gap, from, to time.Time) []usage.Gap {
	var out []usage.Gap
	for _, g := range gaps {
		lo, hi := g.From, g.To
		if lo.Before(from) {
			lo = from
		}
		if hi.After(to) {
			hi = to
		}
		if hi.After(lo) {
			out = append(out, usage.Gap{From: lo, To: hi, Cause: g.Cause})
		}
	}

	return out
}
