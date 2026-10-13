package fleetops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/usage"
)

// showWindow runs `billet jobs show --json --window` as the CLI does and
// decodes the window it answers.
func showWindow(t *testing.T, cfg, lease, window string) *windowJSON {
	t.Helper()

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--json", "--window", window,
			lease}); err != nil {
			t.Errorf("billet jobs show --json --window: %v", err)
		}
	})

	var got struct {
		Window *windowJSON `json:"window"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the answer is not one JSON object: %v\n%s", err, out)
	}

	if got.Window == nil {
		t.Fatalf("no window in the answer:\n%s", out)
	}

	return got.Window
}

// THE WINDOW IS THE SERIES' OWN ACCOUNT OF THOSE SECONDS: 100 MB written in
// each of seconds 3 to 6 and nothing else, so the four seconds from 3 to 7
// hold all 400 MB, and the series covers them whole.
func TestJobsShowWindowIntegratesTheSeries(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	var points []usage.Point
	for s := range int64(11) {
		written := min(max(s-3, 0), 4) * 100_000_000
		points = append(points, usage.Point{OffsetMillis: s * 1000, DiskWrite: written})
	}

	measured := measuredUsage
	lease := seedJob(t, stateDir, jobSeed{requestID: 5, usage: &measured, job: &forgedJob,
		series: clockedSeriesOf(t, points)})

	from, to := pageStart.Add(3*time.Second), pageStart.Add(7*time.Second)
	w := showWindow(t, cfg, lease, from.Format(time.RFC3339)+","+to.Format(time.RFC3339))

	if w.Usage == nil {
		t.Fatalf("a clocked series gave no usage: %+v", *w)
	}

	if !w.Usage.Complete || w.Usage.SpanMillis != 4000 || w.Usage.CoveredMillis != 4000 ||
		w.Usage.DiskWriteBytes != 400_000_000 {
		t.Errorf("window = %+v, want the 400 MB written in it, covered whole", *w.Usage)
	}

	// EDGES ON SAMPLES ARE THEIR OWN BOUNDS: the whole intervals inside the
	// window and over it are both the window itself.
	for name, span := range map[string]*windowSpanJSON{"inner": w.Inner, "outer": w.Outer} {
		if span == nil || span.From != wallStamp(from) || span.To != wallStamp(to) ||
			span.Usage.DiskWriteBytes != 400_000_000 {
			t.Errorf("%s = %+v, want the window itself", name, span)
		}
	}

	// Half of second 2 to half of second 7: one half interval of nothing, then
	// three and a half of 100 MB a second.
	from, to = pageStart.Add(2500*time.Millisecond), pageStart.Add(6500*time.Millisecond)
	if w := showWindow(t, cfg, lease, from.Format(time.RFC3339Nano)+","+to.Format(time.RFC3339Nano)); w.Usage == nil ||
		w.Usage.DiskWriteBytes != 350_000_000 {
		t.Errorf("a window inside the intervals = %+v, want 350 MB", w)
	}

	// THE ANSWER KEEPS THE FRACTIONS IT WAS ASKED FOR, in UTC, so the checker
	// can tell it is the window it asked for; a zone in the request is the
	// same instant.
	zone := time.FixedZone("PDT", -7*3600)
	w = showWindow(t, cfg, lease, from.In(zone).Format(time.RFC3339Nano)+","+to.In(zone).Format(time.RFC3339Nano))
	if w.From != from.UTC().Format(time.RFC3339Nano) || w.To != to.UTC().Format(time.RFC3339Nano) ||
		!strings.Contains(w.From, ".5Z") {
		t.Errorf("window %s to %s, want %s to %s", w.From, w.To, from.UTC().Format(time.RFC3339Nano),
			to.UTC().Format(time.RFC3339Nano))
	}
}

// A SERIES THAT CANNOT BE PLACED ON THE HOST'S CLOCK GIVES NO USAGE AND SAYS
// WHY, never a window of zeros.
func TestJobsShowWindowSaysWhyThereIsNone(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	measured := measuredUsage
	lease := seedJob(t, stateDir, jobSeed{requestID: 6, usage: &measured, job: &forgedJob,
		series: clocklessSeries(t)})

	w := showWindow(t, cfg, lease, "2026-10-09T12:00:01Z,2026-10-09T12:00:04Z")
	if w.Usage != nil || !strings.Contains(w.Unavailable, "without its start on the host's clock") {
		t.Errorf("window = %+v, want no usage and the reason", *w)
	}

	unmeasured := seedJob(t, stateDir, jobSeed{requestID: 7, job: &forgedJob})
	if w := showWindow(t, cfg, unmeasured, "2026-10-09T12:00:01Z,2026-10-09T12:00:04Z"); w.Usage != nil ||
		w.Unavailable != "no usage report was recorded" {
		t.Errorf("an unmeasured job's window = %+v", *w)
	}
}

func TestJobsShowRefusesAWindowItCannotAnswer(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	measured := measuredUsage
	lease := seedJob(t, stateDir, jobSeed{requestID: 8, usage: &measured, job: &forgedJob})

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--window", "2026-10-09T12:00:01Z,2026-10-09T12:00:04Z"}, "give --json too"},
		{[]string{"--json", "--window", "2026-10-09T12:00:01Z"}, "is not FROM,TO"},
		{[]string{"--json", "--window", "yesterday,2026-10-09T12:00:04Z"}, "FROM"},
		{[]string{"--json", "--window", "2026-10-09T12:00:04Z,2026-10-09T12:00:01Z"}, "must be after"},
		{[]string{"--json", "--window="}, "is not FROM,TO"},
		{[]string{"--window="}, "give --json too"},
	} {
		args := append(append([]string{"show", "--config", cfg}, tc.args...), lease)
		if err := Jobs(t.Context(), processEnv(), args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one saying %q", tc.args, err, tc.want)
		}
	}
}

// THE EDGES ARE BOUNDED, NOT GUESSED: 100 MB written somewhere in the interval
// from second 3 to 4, a window from 3.5 to 6.5. Splitting by time gives it
// half; the whole intervals inside (4 to 6) hold none of it and the whole
// intervals over it (3 to 7) all of it, which is all the series can be sure of.
func TestJobsShowWindowBoundsWhatTheEdgesHold(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	var points []usage.Point
	for s := range int64(11) {
		written := int64(0)
		if s >= 4 {
			written = 100_000_000
		}
		points = append(points, usage.Point{OffsetMillis: s * 1000, DiskWrite: written})
	}

	measured := measuredUsage
	lease := seedJob(t, stateDir, jobSeed{requestID: 9, usage: &measured, job: &forgedJob,
		series: clockedSeriesOf(t, points)})

	from, to := pageStart.Add(3500*time.Millisecond), pageStart.Add(6500*time.Millisecond)
	w := showWindow(t, cfg, lease, from.Format(time.RFC3339Nano)+","+to.Format(time.RFC3339Nano))

	if w.Usage == nil || w.Usage.DiskWriteBytes != 50_000_000 {
		t.Errorf("split by time = %+v, want half of the burst", w.Usage)
	}

	if w.Inner == nil || w.Inner.Usage.DiskWriteBytes != 0 || w.Inner.From != wallStamp(pageStart.Add(4*time.Second)) ||
		w.Inner.To != wallStamp(pageStart.Add(6*time.Second)) {
		t.Errorf("inner = %+v, want seconds 4 to 6 and nothing written", w.Inner)
	}

	if w.Outer == nil || w.Outer.Usage.DiskWriteBytes != 100_000_000 ||
		w.Outer.From != wallStamp(pageStart.Add(3*time.Second)) || w.Outer.To != wallStamp(pageStart.Add(7*time.Second)) {
		t.Errorf("outer = %+v, want seconds 3 to 7 and the whole burst", w.Outer)
	}

	// A window reaching past the last sample has no outer span: the series
	// cannot bound what it never saw.
	from, to = pageStart.Add(8*time.Second), pageStart.Add(20*time.Second)
	if w := showWindow(t, cfg, lease, from.Format(time.RFC3339)+","+to.Format(time.RFC3339)); w.Outer != nil {
		t.Errorf("a window past the series has outer %+v", w.Outer)
	}
}
