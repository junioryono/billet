package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func TestRetirementReadmitsAfterProductionTimerObservation(t *testing.T) {
	for _, timer := range []string{upgradeTimerUnit, backupTimerUnit} {
		for _, verb := range []string{"stop", "disable"} {
			for _, unsafe := range []bool{false, true} {
				name := timer + "/" + verb + "/clean"
				if unsafe {
					name = timer + "/" + verb + "/unsafe"
				}
				t.Run(name, func(t *testing.T) {
					f := newRequestFixture(t)
					f.retainANode(t)
					f.reserve(t)
					j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
					f.manager.set(timer, "ActiveState", "active")
					if r := admitRetireOperation(t.Context(), j, "stop", timer); r != nil {
						t.Fatalf("clean prior admission: %+v", r)
					}
					before := mustRead(t, retirement.JournalPath())
					observations, submissions := 0, 0
					insp := retireOperationInspector()
					lifeops.WithObserver(func(_ context.Context, args []string) {
						if args[0] == "stop" || args[0] == "disable" {
							submissions++
						}
						if args[0] != "show" || !slices.Contains(args, "--property=LoadState") ||
							!slices.Contains(args, "--property=FragmentPath") || args[len(args)-1] != timer {
							return
						}
						observations++
						if unsafe && (verb == "stop" || observations == 2) {
							setRetireEffect(t, f, timer, "PropagatesStopTo", nodeUnit)
						}
					})(insp)
					r := stopAndDisableForRetirement(t.Context(), lifeops.NewConverger(insp), j, timer)
					if !unsafe {
						if r != nil || observations != 2 || submissions != 2 {
							t.Fatalf("clean helper: observations=%d submissions=%d refusal=%+v", observations, submissions, r)
						}
						return
					}
					wantStops := 0
					if verb == "disable" {
						wantStops = 1
					}
					if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "PropagatesStopTo="+nodeUnit) || submissions != wantStops {
						t.Fatalf("timer query drift crossed submission: observations=%d submissions=%d refusal=%+v", observations, submissions, r)
					}
					body, err := os.ReadFile(filepath.Join(f.unitsDir, ".submitted"))
					if wantStops == 0 {
						if !os.IsNotExist(err) {
							t.Fatalf("stop reached manager: %q %v", body, err)
						}
					} else if err != nil || string(body) != "stop "+timer+"\n" {
						t.Fatalf("disable reached manager: %q %v", body, err)
					}
					if mustRead(t, retirement.JournalPath()) != before || requireRetireJournal(t).Phase != j.Phase {
						t.Fatal("timer refusal advanced the journal")
					}
				})
			}
		}
	}
}
