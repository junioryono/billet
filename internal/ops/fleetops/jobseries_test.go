package fleetops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/usage"
)

// measurementCharts is how many charts of the job's usage a page draws, the
// strip of its gaps aside.
func measurementCharts(page string) int {
	return strings.Count(page, "<svg data-chart") -
		strings.Count(page, `aria-label="Intervals the sampler did not see"`)
}

// run15 is run 15's idle job's series around its one gap: a sample a second,
// and the one at 08:51:36.29 dropped for a read writeback kept moving (#495).
func run15() *usage.Timeline {
	tl := &usage.Timeline{First: time.Date(2026, 10, 15, 8, 50, 27, 129_000_000, time.UTC)}
	for i := range int64(10) {
		p := usage.Point{OffsetMillis: i * 1000, CPUUsage: i * 1000}
		if i >= 6 {
			p.OffsetMillis += 1000
		}
		if i == 6 {
			p.Gap = usage.GapFailedRead
		}
		tl.Points = append(tl.Points, p)
	}

	return tl
}

// THE TEXT REPORT SAYS WHEN THE SERIES RAN AND EVERY INTERVAL IT DID NOT SEE,
// with how long and why, and says so when it saw them all.
func TestTheTextReportListsTheSeriesGaps(t *testing.T) {
	t.Parallel()

	measured := &alloc.RecordedUsage{JobUsage: exampleUsage(exampleSeries())}
	var out bytes.Buffer
	renderSeries(&out, run15(), "", measured)
	got := out.String()
	for _, want := range []string{
		"series     10 samples on the host's clock from 2026-10-15T08:50:27.129Z to 2026-10-15T08:50:37.129Z; " +
			"1 interval not seen, 2s in all, which no window counts\n",
		"           2026-10-15T08:50:32.129Z to 2026-10-15T08:50:34.129Z (2s): failed_read, a counter the " +
			"sampler had read before could not be read\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the series section does not say %q:\n%s", want, got)
		}
	}

	seen := run15()
	seen.Points[6].Gap = usage.GapNone
	out.Reset()
	renderSeries(&out, seen, "", measured)
	if !strings.Contains(out.String(), "; the sampler saw every interval\n") {
		t.Errorf("a series with no gap does not say so:\n%s", out.String())
	}

	out.Reset()
	renderSeries(&out, nil, "the series was recorded without its start", measured)
	if !strings.Contains(out.String(), "series     not placed on the host's clock, because the series was "+
		"recorded without its start\n") {
		t.Errorf("a series off the clock does not say why:\n%s", out.String())
	}

	out.Reset()
	renderSeries(&out, run15(), "", nil)
	if out.Len() != 0 {
		t.Errorf("a job with no usage report printed a series section:\n%s", out.String())
	}
}

// A SERIES OF MANY GAPS LISTS THE FIRST FEW AND COUNTS THE REST, in the text
// and in a JSON record alike, and the record's total covers every one.
func TestManyGapsAreListedToABound(t *testing.T) {
	t.Parallel()

	tl := &usage.Timeline{First: pageStart}
	for i := range int64(2 * (maxGapsListed + 10)) {
		p := usage.Point{OffsetMillis: i * 1000}
		if i%2 == 1 {
			p.Gap = usage.GapSlowRead
		}
		tl.Points = append(tl.Points, p)
	}
	gaps := maxGapsListed + 10

	var out bytes.Buffer
	renderSeries(&out, tl, "", &alloc.RecordedUsage{})
	if n := strings.Count(out.String(), "slow_read"); n != maxGapsShown {
		t.Errorf("the text lists %d gaps, want %d", n, maxGapsShown)
	}
	if want := fmt.Sprintf("and %d more\n", gaps-maxGapsShown); !strings.Contains(out.String(), want) {
		t.Errorf("the text does not count the rest as %q:\n%s", want, out.String())
	}

	s := seriesJSONOf(tl)
	if len(s.Gaps) != maxGapsListed || s.GapsMore != 10 || s.UnseenMillis != int64(gaps)*1000 {
		t.Errorf("the record lists %d gaps, %d more, %d ms unseen; want %d, 10 and %d", len(s.Gaps), s.GapsMore,
			s.UnseenMillis, maxGapsListed, gaps*1000)
	}
}

// THE JSON RECORD CARRIES THE SERIES' OWN ACCOUNT, and a window lists the gaps
// inside it, cut to it, which is what a short covered_ms is made of.
func TestTheJSONRecordAndItsWindowListTheGaps(t *testing.T) {
	t.Parallel()

	tl := run15()
	measured := &alloc.RecordedUsage{JobUsage: exampleUsage(exampleSeries())}
	w := window{from: tl.First.Add(5500 * time.Millisecond), to: tl.First.Add(9 * time.Second)}

	var out bytes.Buffer
	if err := renderJobJSON(&out, alloc.JobRecord{LeaseID: "l1"}, measured, tl, "", nil,
		windowJSONOf(tl, "", w)); err != nil {
		t.Fatalf("renderJobJSON: %v", err)
	}
	var got struct {
		Usage struct {
			Series *seriesJSON `json:"series"`
		} `json:"usage"`
		Window *windowJSON `json:"window"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out.String())
	}
	s := got.Usage.Series
	if s == nil || s.Samples != 10 || s.First != "2026-10-15T08:50:27.129Z" || s.UnseenMillis != 2000 ||
		len(s.Gaps) != 1 || s.Gaps[0] != (gapJSON{From: "2026-10-15T08:50:32.129Z", To: "2026-10-15T08:50:34.129Z",
		Millis: 2000, Cause: "failed_read"}) {
		t.Errorf("usage.series = %+v", s)
	}
	win := got.Window
	if win == nil || win.Usage == nil || len(win.Gaps) != 1 ||
		win.Gaps[0] != (gapJSON{From: "2026-10-15T08:50:32.629Z", To: "2026-10-15T08:50:34.129Z", Millis: 1500,
			Cause: "failed_read"}) {
		t.Fatalf("window = %+v", win)
	}
	if win.Usage.SpanMillis-win.Usage.CoveredMillis != win.Gaps[0].Millis {
		t.Errorf("the window's gaps (%d ms) are not what its coverage is short by (%d of %d ms)", win.Gaps[0].Millis,
			win.Usage.CoveredMillis, win.Usage.SpanMillis)
	}

	// A SERIES THAT SAW EVERYTHING SAYS SO WITH AN EMPTY LIST, in its own
	// account and in a window alike: none is not could-not-tell.
	seen := run15()
	seen.Points[6].Gap = usage.GapNone
	out.Reset()
	if err := renderJobJSON(&out, alloc.JobRecord{LeaseID: "l1"}, measured, seen, "", nil,
		windowJSONOf(seen, "", w)); err != nil {
		t.Fatalf("renderJobJSON: %v", err)
	}
	whole := decodeRecord(t, out.Bytes())
	var series map[string]json.RawMessage
	if err := json.Unmarshal(whole["usage"]["series"], &series); err != nil {
		t.Fatalf("decode usage.series: %v", err)
	}
	if string(series["gaps"]) != "[]" || string(whole["window"]["gaps"]) != "[]" {
		t.Errorf("a series that saw everything lists gaps %s and its window %s, want [] and []",
			series["gaps"], whole["window"]["gaps"])
	}

	// AND ONE THAT CANNOT TELL SAYS NULL, AND WHY.
	out.Reset()
	if err := renderJobJSON(&out, alloc.JobRecord{LeaseID: "l1"}, measured, nil, "the usage report came with "+
		"no series", nil, windowJSONOf(nil, "the usage report came with no series", w)); err != nil {
		t.Fatalf("renderJobJSON: %v", err)
	}
	whole = decodeRecord(t, out.Bytes())
	if string(whole["usage"]["series"]) != "null" ||
		string(whole["usage"]["series_unavailable"]) != `"the usage report came with no series"` ||
		string(whole["window"]["gaps"]) != "null" {
		t.Errorf("a report with no series on the clock says series %s (%s) and window gaps %s, want null, "+
			"why, and null", whole["usage"]["series"], whole["usage"]["series_unavailable"], whole["window"]["gaps"])
	}
}

// decodeRecord is a JSON record's usage and window, each field as written.
func decodeRecord(t *testing.T, body []byte) map[string]map[string]json.RawMessage {
	t.Helper()

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, key := range []string{"usage", "window"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(top[key], &fields); err != nil {
			t.Fatalf("decode %s: %v\n%s", key, err, body)
		}
		out[key] = fields
	}

	return out
}

// THE PAGE DRAWS THE GAPS ONCE, as a strip on the charts' axis, and lists them
// under what the host measured; a series that saw everything draws no strip.
func TestThePageShowsTheSeriesGaps(t *testing.T) {
	t.Parallel()

	tl := run15()
	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(exampleSeries())}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{none: "no GitHub job"}, tl, "")
	strip := figure(t, page, "Not seen by the sampler")
	if !strings.Contains(strip, `<path class="gap" d="M50000 0H70000V100H50000Z">`) {
		t.Errorf("the strip does not draw the gap from 5s to 7s of 10s:\n%s", strip)
	}
	if usageText := section(t, page, "usage"); !strings.Contains(usageText,
		"<li>2026-10-15T08:50:32.129Z to 2026-10-15T08:50:34.129Z (2s): failed_read") {
		t.Errorf("the usage section does not list the gap:\n%s", usageText)
	}
	if measurementCharts(page) != 4 {
		t.Errorf("the page draws %d measurement charts, want 4", measurementCharts(page))
	}

	seen := run15()
	seen.Points[6].Gap = usage.GapNone
	page = renderPage(t, alloc.JobRecord{LeaseID: "l1"}, measured, jobSteps{none: "no GitHub job"}, seen, "")
	if strings.Contains(page, "Not seen by the sampler") || strings.Contains(page, `class="gap"`) {
		t.Error("a series that saw every interval drew a gap strip")
	}
}

// THE STRIP IS BOUNDED WHATEVER THE SERIES HOLDS: a series at the ledger's
// bound with every other interval unseen draws at most maxChartWindows bands,
// and its page stays within the bound a series page keeps.
func TestTheGapStripIsBounded(t *testing.T) {
	t.Parallel()

	tl := largestSeries(t)
	for i := range tl.Points {
		tl.Points[i].Gap = usage.GapNone
		if i%2 == 1 {
			tl.Points[i].Gap = usage.GapSlowRead
		}
	}
	x := func(at time.Time) int64 {
		width := tl.Points[len(tl.Points)-1].OffsetMillis
		return at.Sub(tl.First).Milliseconds() * chartWidth / width
	}
	if bands := strings.Count(gapStrip(x, tl.Gaps()), "M"); bands > maxChartWindows {
		t.Errorf("the strip draws %d bands, over %d", bands, maxChartWindows)
	}

	measured := &alloc.RecordedUsage{Node: "epyc-1", JobUsage: exampleUsage(tl.Points)}
	page := renderPage(t, alloc.JobRecord{LeaseID: "l1", VCPU: 2, Memory: 8 << 30}, measured,
		jobSteps{none: "no GitHub job"}, tl, "")
	if len(page) > maxSeriesPageBytes {
		t.Errorf("a page of a series with every other interval unseen is %d bytes, over the %d bound", len(page),
			maxSeriesPageBytes)
	}
}
