package uplink

import (
	"log/slog"
	"math"
	"testing"
	"time"
)

// reports are the records of reports the run logged.
func (r *rig) reports() []slog.Record {
	var reports []slog.Record

	for i := range r.log.list {
		if r.log.list[i].Message == "the line since the last report" {
			reports = append(reports, r.log.list[i])
		}
	}

	return reports
}

// reportRounds is how many rounds a report covers.
const reportRounds = int(reportEvery / Tick)

// A RUN SAYS WHERE IT HOLDS THE LINE EVERY reportEvery, CUT OR NO CUT: the rate
// CAKE holds, the line found, what the host moved under it and what the line
// showed. A run that logged only its cuts said nothing on a day it took none.
func TestARunReportsWhereItHoldsTheLineWithNoCut(t *testing.T) {
	t.Parallel()

	r := newRig(t, &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration { return idle() }}, 200)
	r.controller.Down.Limit = 650

	r.run(reportRounds - 1)
	if got := len(r.reports()); got != 0 {
		t.Fatalf("%d reports before %v had passed", got, reportEvery)
	}

	r.run(1)
	reports := r.reports()
	if len(reports) != 1 || len(r.cuts()) != 0 {
		t.Fatalf("%d reports and %d cuts after %v of a quiet line, want one report and no cut", len(reports),
			len(r.cuts()), reportEvery)
	}

	// ONE ROUND IS BLIND, the run's first, which set the reflectors' baselines
	// and so counted none of them.
	a := attrs(reports[0])
	for key, want := range map[string]int64{
		"rounds": int64(reportRounds), "delayed_rounds": 0, "lossy_rounds": 0, "blind_rounds": 1, "cuts": 0,
	} {
		if got := a[key].Int64(); got != want {
			t.Errorf("the report says %s=%d, want %d", key, got, want)
		}
	}

	for key, want := range map[string]float64{
		"down_peak_mbit": 200, "down_mean_mbit": 200, "up_peak_mbit": 5, "down_limit_mbit": 650,
		"down_mbit": r.st.applied[1], "up_mbit": r.st.applied[0],
	} {
		if got := a[key].Float64(); math.Abs(got-want) > 0.11 {
			t.Errorf("the report says %s=%.1f, want %.1f", key, got, want)
		}
	}

	if d := a["delay_p95"]; d.Kind() != slog.KindDuration || d.Duration() > time.Millisecond {
		t.Errorf("the report says delay_p95=%v; every reflector answered at its baseline", d)
	}
	if _, ok := a["gateway_stalled_rounds"]; ok {
		t.Error("a run asking no gateway reported its stalls")
	}
	if over := a["over"].Duration(); over != reportEvery {
		t.Errorf("the report covers %v, want %v", over, reportEvery)
	}

	// AND THE NEXT REPORT COUNTS ONLY ITS OWN ROUNDS, with no blind one.
	r.run(reportRounds)
	reports = r.reports()
	if len(reports) != 2 {
		t.Fatalf("%d reports after twice %v", len(reports), reportEvery)
	}
	if b := attrs(reports[1]); b["rounds"].Int64() != int64(reportRounds) || b["blind_rounds"].Int64() != 0 {
		t.Fatalf("the second report covers %v rounds with %v blind, want %d of its own and none blind",
			b["rounds"], b["blind_rounds"], reportRounds)
	}
}

// A LINE THAT SHOWED ITSELF IS IN THE REPORT: rounds delayed and rounds that
// lost a probe are counted apart, the cut a run took is counted beside its own
// log line, a stalled gateway is counted when one is asked, and a round that
// measured nothing is blind, not clear.
func TestAReportCountsWhatTheLineShowed(t *testing.T) {
	t.Parallel()

	p := &fakeProber{reflectors: three, gateway: "192.168.1.254", rtt: func(round int) map[string]time.Duration {
		answers := idle()
		answers["192.168.1.254"] = time.Millisecond

		switch {
		case round < 4:
		case round >= 400 && round < 412:
			// A QUEUE THAT LASTS, which is cut for.
			for k, v := range answers {
				if k != "192.168.1.254" {
					answers[k] = v + 40*time.Millisecond
				}
			}
		case round%10 == 0:
			// THE DELAY: every reflector 40 ms above its baseline.
			for k, v := range answers {
				if k != "192.168.1.254" {
					answers[k] = v + 40*time.Millisecond
				}
			}
		case round%10 == 1:
			// THE LOSS: one reflector unanswered.
			delete(answers, three[round%3])
		case round%10 == 2:
			// THE STALL: the gateway 60 ms above its own.
			answers["192.168.1.254"] = 60 * time.Millisecond
		case round%10 == 3:
			// A RISE UNDER BLOAT, which is no queue.
			for k, v := range answers {
				if k != "192.168.1.254" {
					answers[k] = v + 8*time.Millisecond
				}
			}
		}

		return answers
	}}
	r := newRig(t, p, 400)

	r.run(reportRounds)

	reports := r.reports()
	if len(reports) != 1 {
		t.Fatalf("%d reports after %v", len(reports), reportEvery)
	}

	a := attrs(reports[0])
	// ROUNDS 10, 20, ... are delayed, 11, 21, ... lossy and 12, 22, ... stalled,
	// a tenth of the rounds each, the first ten setting baselines, and 13,
	// 23, ... rise under Bloat; 400 to 411 are delayed throughout, which adds
	// the ten of them that were not delayed rounds already (all but 400 and 410)
	// and takes away two losses (401, 411) and one stall (402).
	tenth := int64(reportRounds/10 - 1)
	for key, want := range map[string]int64{
		"delayed_rounds": tenth + 10, "lossy_rounds": tenth - 2, "gateway_stalled_rounds": tenth - 1,
		"blind_rounds": 1,
	} {
		if got := a[key].Int64(); got != want {
			t.Errorf("the report says %s=%d, want %d", key, got, want)
		}
	}
	if got := a["cuts"].Int64(); got == 0 || got != int64(len(r.cuts())) {
		t.Errorf("the report counts %d cuts and the run logged %d, want the same and at least the burst's one",
			got, len(r.cuts()))
	}

	// A LINE THAT MEASURED NOTHING IS BLIND IN THE REPORT, never clear.
	blind := newRig(t, &fakeProber{reflectors: three, rtt: func(int) map[string]time.Duration { return nil }}, 400)
	blind.run(reportRounds)
	if reports = blind.reports(); len(reports) != 1 {
		t.Fatalf("%d reports from a blind run", len(reports))
	}
	a = attrs(reports[0])
	if a["blind_rounds"].Int64() != int64(reportRounds) || a["delay_p50"].String() != "unmeasured" {
		t.Errorf("a run whose reflectors never answered reported %v blind rounds and delay_p50=%v, want %d and "+
			"unmeasured", a["blind_rounds"], a["delay_p50"], reportRounds)
	}
}
