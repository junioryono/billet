package node

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/usage"
)

// A JOB'S GAPS ARE IN THE NODE'S LOG BY CAUSE, beside its lease, and a series
// the sampler saw whole says nothing (#495).
func TestAJobsGapsAreLoggedByCause(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	first := time.Date(2026, 10, 15, 8, 50, 27, 0, time.UTC)
	sum := usage.Summary{First: first, Points: []usage.Point{{OffsetMillis: 0},
		{OffsetMillis: 1000}, {OffsetMillis: 3000, Gap: usage.GapFailedRead}, {OffsetMillis: 4000},
		{OffsetMillis: 4500, Gap: usage.GapFinalEnergy}}}

	logGaps(log, "l1", "billet-l1", sum)
	for _, want := range []string{"runner=billet-l1", "lease=l1", "gaps=2", "unseen=2.5s", "failed_read=2s",
		"final_energy=500ms"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log line does not say %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	sum.Points[2].Gap, sum.Points[4].Gap = usage.GapNone, usage.GapNone
	logGaps(log, "l1", "billet-l1", sum)
	if out.Len() != 0 {
		t.Errorf("a series seen whole was logged:\n%s", out.String())
	}
}
