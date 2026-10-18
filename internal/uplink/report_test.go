package uplink

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
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
	// a tenth of the rounds each but for 0, 1 and 2, which the round < 4 guard
	// leaves quiet (round 0 sets the baselines and is blind), and 13,
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

// delayedFrom is a prober whose reflectors all answer 40 ms above their idle
// round trip from round from on.
func delayedFrom(from int) *fakeProber {
	return &fakeProber{reflectors: three, rtt: func(round int) map[string]time.Duration {
		answers := idle()
		if round >= from {
			for k, v := range answers {
				answers[k] = v + 40*time.Millisecond
			}
		}

		return answers
	}}
}

// A CUT IN THE ROUND THAT IS REPORTED IS IN THAT REPORT, with the rate it set:
// the report is logged after the round's step, not before it.
func TestACutInTheReportedRoundIsInItsReport(t *testing.T) {
	t.Parallel()

	// THE QUEUE FILLS THE WINDOW IN THE REPORTING ROUND ITSELF: three delayed
	// rounds, the last of them the 600th.
	r := newRig(t, delayedFrom(reportRounds-3), 400)
	r.run(reportRounds)

	reports := r.reports()
	if len(reports) != 1 || len(r.cuts()) != 1 {
		t.Fatalf("%d reports and %d cuts, want the one cut in the one report's own round", len(reports),
			len(r.cuts()))
	}

	a := attrs(reports[0])
	if a["cuts"].Int64() != 1 || a["down_mbit"].Float64() != round(r.sets[len(r.sets)-1][1]) {
		t.Errorf("the report says cuts=%v and down_mbit=%v, want the round's cut and the %.1f it set",
			a["cuts"], a["down_mbit"], r.sets[len(r.sets)-1][1])
	}
}

// A CHANGE TC REFUSED LEAVES WHAT CAKE HOLDS UNKNOWN, and the report says so
// rather than give the rates last applied as the ones in force.
func TestARefusedChangeIsReportedAsUnknown(t *testing.T) {
	t.Parallel()

	r := newRig(t, delayedFrom(reportRounds-3), 400)
	r.run(reportRounds - 1)
	r.st.set = func(context.Context, float64, float64) error {
		return errors.New("tc: RTNETLINK answers: Device or resource busy")
	}

	r.clock = r.clock.Add(Tick)
	if err := r.st.tick(t.Context()); err == nil {
		t.Fatal("the reporting round's cut was not asked of tc, so the case this is about did not arise")
	}

	reports := r.reports()
	if len(reports) != 1 {
		t.Fatalf("%d reports", len(reports))
	}
	a := attrs(reports[0])
	for _, key := range []string{"up_mbit", "down_mbit"} {
		if a[key].Kind() != slog.KindString || a[key].String() != "unknown" {
			t.Errorf("after a refused change the report says %s=%v, want unknown", key, a[key])
		}
	}

	// AND THE CUT TC REFUSED IS NOT ONE TAKEN.
	if a["cuts"].Int64() != 0 || len(r.cuts()) != 0 {
		t.Errorf("a cut tc refused is reported as cuts=%v (logged %d), want none", a["cuts"], len(r.cuts()))
	}
}

// A ROUND THAT COUNTED PROBES BUT MEASURED NO DELAY IS LOSSY, NOT BLIND: most of
// its reflectors went unanswered, which is the line saying something; a blind
// round counted none. And the mean is over the rounds, not the peak.
func TestAReportTellsLossFromBlindnessAndMeanFromPeak(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	twoLost := Sample{Probes: []Probe{{Reflector: "a", Lost: true}, {Reflector: "b", Lost: true}, {Reflector: "c"}}}

	var r lineReport
	r.observe(p, twoLost, false, Sample{}, [2]float64{10, 100})
	r.observe(p, Sample{}, false, Sample{}, [2]float64{30, 300})

	if r.lossy != 1 || r.blind != 1 || len(r.delays) != 0 || r.delayed != 0 {
		t.Errorf("two lost of three then nothing counted read as %d lossy, %d blind, %d delays, %d delayed; "+
			"want 1, 1, 0 and 0", r.lossy, r.blind, len(r.delays), r.delayed)
	}

	logs := &records{}
	r.log(t.Context(), slog.New(logs), time.Unix(0, 0).Add(reportEvery), [2]float64{}, false, newController())

	a := attrs(logs.list[0])
	for key, want := range map[string]float64{
		"up_mean_mbit": 20, "down_mean_mbit": 200, "up_peak_mbit": 30, "down_peak_mbit": 300,
	} {
		if got := a[key].Float64(); got != want {
			t.Errorf("the report says %s=%v, want %v", key, got, want)
		}
	}
}

// A GATEWAY ASKED PART OF THE TIME HAS ITS STALLS REPORTED, though the route
// lost it before the report.
func TestAGatewayDroppedMidReportKeepsItsStalls(t *testing.T) {
	t.Parallel()

	p := DefaultParams()
	quiet := Sample{Delay: time.Millisecond, DelayKnown: true, Probes: []Probe{{Reflector: "a"}}}
	stall := Sample{Delay: 60 * time.Millisecond, DelayKnown: true, Probes: []Probe{{Reflector: "g"}}}

	var report lineReport
	report.observe(p, quiet, true, stall, [2]float64{})
	report.observe(p, quiet, false, Sample{}, [2]float64{})

	logs := &records{}
	report.log(t.Context(), slog.New(logs), time.Unix(0, 0).Add(reportEvery), [2]float64{}, false, newController())

	if len(logs.list) != 1 {
		t.Fatalf("%d records", len(logs.list))
	}
	if got, ok := attrs(logs.list[0])["gateway_stalled_rounds"]; !ok || got.Int64() != 1 {
		t.Errorf("a gateway stalled once and then dropped reported gateway_stalled_rounds=%v (present %v), want 1",
			got, ok)
	}
}

// THE PERCENTILES ARE BY NEAREST RANK: the smallest delay at least p percent of
// the rounds reach, so the median of an even count is its lower middle one.
func TestTheDelayPercentilesAreByNearestRank(t *testing.T) {
	t.Parallel()

	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	var spread lineReport
	for _, n := range rand.New(rand.NewPCG(9, 10)).Perm(100) {
		spread.delays = append(spread.delays, ms(n+1))
	}

	pair := lineReport{delays: []time.Duration{ms(40), 0}}
	one := lineReport{delays: []time.Duration{ms(7)}}

	for _, c := range []struct {
		name   string
		report lineReport
		p      int
		want   time.Duration
	}{
		{"the median of 1..100 ms", spread, 50, ms(50)},
		{"the 95th of 1..100 ms", spread, 95, ms(95)},
		{"the 100th of 1..100 ms", spread, 100, ms(100)},
		{"the median of two", pair, 50, 0},
		{"the 95th of two", pair, 95, ms(40)},
		{"the median of one", one, 50, ms(7)},
	} {
		if got := c.report.percentile("p", c.p); got.Value.Duration() != c.want {
			t.Errorf("%s is %v, want %v", c.name, got.Value, c.want)
		}
	}
}
