package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/runnerrelease"
)

// THE EXIT CODE IS WHAT A MONITOR READS, so it is what gets asserted — one code per
// state, and the states are decided by the release history rather than by the newest
// release's age.
//
// DRIVING THE COMMAND, NOT THE MODEL. runnerrelease has its own tests for where the
// window opens; what is unprovable from there is whether `billet runner check` maps
// each answer onto the right exit code and says the right thing, and a test that
// called the model would prove the model twice.
func TestRunnerCheckReportsEachStateWithItsOwnExitCode(t *testing.T) {
	opened := time.Now().UTC()

	for _, tc := range []struct {
		name     string
		fresh    runnerrelease.Freshness
		wantErr  error
		contains []string
	}{
		{
			name: "current",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.336.0", InstalledKnown: true, HistoryComplete: true,
			},
			contains: []string{"2.336.0", "current release"},
		},
		{
			name: "inside the window",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.337.0", InstalledKnown: true, HistoryComplete: true,
				FirstNewer: "2.337.0", FirstNewerPublished: opened.Add(-24 * time.Hour),
			},
			contains: []string{"2.337.0", "days to take it up"},
		},
		{
			name: "due",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.339.0", InstalledKnown: true, HistoryComplete: true,
				FirstNewer:          "2.337.0",
				FirstNewerPublished: opened.Add(-(runnerrelease.Warn + time.Hour)),
			},
			wantErr: errRunnerDue,
			// THE RELEASE THAT STARTED THE CLOCK AND THE ONE TO TAKE UP ARE BOTH
			// NAMED. They are different questions, and an operator reading only the
			// newest release beside a deadline cannot see where the clock began.
			contains: []string{"2.337.0", "started the ordinary 30-day window",
				"2.339.0 is the newest"},
		},
		{
			name: "expired",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.339.0", InstalledKnown: true, HistoryComplete: true,
				FirstNewer:          "2.337.0",
				FirstNewerPublished: opened.Add(-(runnerrelease.Grace + time.Hour)),
			},
			wantErr:  errExpiredRunner,
			contains: []string{"stopped queueing jobs", "pinned.txt", "2.339.0"},
		},
		{
			// AN UPPER BOUND STILL PROVES AN EXPIRY. The installed release is older
			// than the history billet reads, so the window opened at or before the
			// earliest release it can see — which makes "already closed" sound.
			name: "expired, from a version the history does not reach",
			fresh: runnerrelease.Freshness{
				Installed: "2.100.0", Latest: "2.339.0",
				FirstNewer:          "2.337.0",
				FirstNewerPublished: opened.Add(-(runnerrelease.Grace + time.Hour)),
			},
			wantErr:  errExpiredRunner,
			contains: []string{"older than the history", "the latest it could have been"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// NOT PARALLEL: this swaps a package-level seam and captures stdout.
			useFreshness(t, answer(tc.fresh))

			var err error

			out := capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("cmdRunner returned %v, want %v", err, tc.wantErr)
			}

			for _, want := range tc.contains {
				if !strings.Contains(out, want) {
					t.Errorf("the report does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}

// A VERSION THE HISTORY DOES NOT REACH IS NOT A GREEN CHECK.
//
// It is the state where billet cannot say how long is left — the window may have
// opened before the earliest release it can see — so the days it would print would
// be an over-estimate. Reporting it as nothing to do is the false negative the whole
// change is about, and inventing a number would be worse.
//
// BOTH SHAPES OF IT, because they differ by exactly the thing that made the second
// one dangerous: `Current` means only that nothing NEWER was found, which is also
// true of a version billet could not place at all. Asking `Current` first printed
// "which is the current release" and exited 0 for precisely that version.
func TestRunnerCheckWillNotGuessAboutAVersionItCannotPlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh runnerrelease.Freshness
	}{
		{
			name: "older than the history, with newer releases in it",
			fresh: runnerrelease.Freshness{
				Installed: "2.100.0", Latest: "2.339.0",
				FirstNewer:          "2.337.0",
				FirstNewerPublished: time.Now().UTC().Add(-24 * time.Hour),
			},
		},
		{
			// NOTHING NEWER AND NOT IN THE HISTORY EITHER: a hand-built runner, a
			// typo in a generation's metadata, a version that was never published.
			// Current() is true here and it means nothing.
			name: "absent from the history with nothing newer in it",
			fresh: runnerrelease.Freshness{
				Installed: "2.999.0", Latest: "2.339.0",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// NOT PARALLEL: this swaps a package-level seam and captures stdout.
			useFreshness(t, answer(tc.fresh))

			var err error

			out := capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

			if err == nil {
				t.Fatalf("a version billet could not place was reported as fine:\n%s", out)
			}

			if errors.Is(err, errRunnerDue) || errors.Is(err, errExpiredRunner) {
				t.Fatalf("an unplaceable version was reported as a verdict: %v", err)
			}

			if !strings.Contains(err.Error(), "could not establish") {
				t.Errorf("the message does not say what could not be established: %v", err)
			}

			if strings.Contains(out, "current release") {
				t.Errorf("a version billet could not place was called the current "+
					"release:\n%s", out)
			}
		})
	}
}

// THE VERSION IN THE REPORT IS THE VERSION THE ANSWER IS ABOUT.
//
// Resolve normalizes what it is given — a leading "v", surrounding space — and
// answers about the result. A command that printed the string it passed IN would
// attribute this verdict to a version it was not computed for, which is the
// one-representation divergence internal/config has produced three times: a value
// validated in one form and consumed in another, silently.
func TestRunnerCheckReportsTheVersionTheAnswerIsAbout(t *testing.T) {
	useFreshness(t, func(
		_ context.Context, _ *http.Client, installed string,
	) (runnerrelease.Freshness, error) {
		return runnerrelease.Freshness{
			// The model's own spelling, deliberately not the caller's.
			Installed: "normalized-" + installed, Latest: "normalized-" + installed,
			InstalledKnown: true, HistoryComplete: true,
		}, nil
	})

	out := capture(t, func() {
		if err := cmdRunner(t.Context(), []string{"check"}); err != nil {
			t.Errorf("cmdRunner: %v", err)
		}
	})

	if !strings.Contains(out, "normalized-") {
		t.Errorf("the report names its own copy of the version rather than the one the "+
			"answer was computed for:\n%s", out)
	}
}

// AN INCOMPLETE SCAN IS NOT A GREEN CHECK EITHER, and it is the second thing that
// takes "nothing to do" away without touching the expiry proof.
//
// billet reads a bounded window of the release history. When it runs out of budget
// with the history still going, a release it never read could be newer than the
// installed one and published earlier than the opener it found — so the deadline it
// computed is later than the truth, and reporting the fleet as current or as having
// days in hand would be the same false negative the newest-release calculation
// produced.
func TestRunnerCheckWillNotGuessFromAnIncompleteHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh runnerrelease.Freshness
	}{
		{
			name: "with nothing newer found",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.336.0", InstalledKnown: true,
			},
		},
		{
			name: "inside the window it could see",
			fresh: runnerrelease.Freshness{
				Installed: "2.336.0", Latest: "2.337.0", InstalledKnown: true,
				FirstNewer:          "2.337.0",
				FirstNewerPublished: time.Now().UTC().Add(-24 * time.Hour),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useFreshness(t, answer(tc.fresh))

			var err error

			out := capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

			if err == nil {
				t.Fatalf("an incomplete scan was reported as nothing to do:\n%s", out)
			}

			if !strings.Contains(err.Error(), "could not establish") {
				t.Errorf("the message does not say what could not be established: %v", err)
			}
		})
	}
}

// AND AN EXPIRY IS STILL A PROOF WHEN THE SCAN WAS INCOMPLETE, which is the whole
// reason incompleteness only takes ONE direction away: a release billet never read
// could only have opened the window EARLIER, so a deadline it can see has passed is
// a deadline that has certainly passed.
func TestRunnerCheckStillProvesAnExpiryFromAnIncompleteHistory(t *testing.T) {
	useFreshness(t, answer(runnerrelease.Freshness{
		Installed: "2.336.0", Latest: "2.339.0", InstalledKnown: true,
		FirstNewer:          "2.337.0",
		FirstNewerPublished: time.Now().UTC().Add(-(runnerrelease.Grace + time.Hour)),
	}))

	var err error

	_ = capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

	if !errors.Is(err, errExpiredRunner) {
		t.Fatalf("cmdRunner returned %v; an expiry an incomplete scan can see is still "+
			"an expiry", err)
	}
}

// BEHIND WITH NO WINDOW IS REPORTED AS A REBUILD, not as a timed one and not as
// silence.
//
// A higher version published BEFORE the installed release is evidence the fleet is
// behind and is not an opener, so there is nothing to count from — and falling
// through to the timed branch printed an empty release name, the year 0001 and a
// negative number of days, then exited 0.
func TestRunnerCheckReportsBeingBehindWithNoWindowToCount(t *testing.T) {
	useFreshness(t, answer(runnerrelease.Freshness{
		Installed: "2.285.3", Latest: "2.301.1",
		InstalledKnown: true, HistoryComplete: true,
	}))

	var err error

	out := capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

	if !errors.Is(err, errRunnerDue) {
		t.Fatalf("cmdRunner returned %v; a fleet behind with no window is a rebuild to "+
			"schedule", err)
	}

	for _, want := range []string{"2.301.1", "already available"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}

	// AND IT OFFERS NO DATE IT DOES NOT HAVE.
	if strings.Contains(out, "0001") {
		t.Errorf("the report printed the zero time:\n%s", out)
	}
}

// AND A FAILURE TO ASK IS NOT A VERDICT EITHER. A machine with no egress cannot find
// out, and reporting that as a fleet about to stop working is the false alarm that
// teaches people to ignore the true one — so it is neither exit 2 nor exit 3.
func TestRunnerCheckSeparatesNotKnowingFromBeingOutOfDate(t *testing.T) {
	useFreshness(t, func(
		context.Context, *http.Client, string,
	) (runnerrelease.Freshness, error) {
		return runnerrelease.Freshness{}, errors.New("dial tcp: no route to host")
	})

	var err error

	_ = capture(t, func() { err = cmdRunner(t.Context(), []string{"check"}) })

	if err == nil {
		t.Fatal("a failed lookup was reported as nothing to do")
	}

	if errors.Is(err, errRunnerDue) || errors.Is(err, errExpiredRunner) {
		t.Fatalf("a failed lookup was reported as a verdict about the fleet: %v", err)
	}

	if !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("the message drops what actually went wrong: %v", err)
	}
}

// THE VERSION ASKED ABOUT IS THE ONE THE FLEET RUNS, falling back to the pin when
// this machine cannot read a published image. It is what the whole answer is keyed
// on, so a command that asked about something else would be exactly wrong while
// looking right.
func TestRunnerCheckAsksAboutTheVersionItReports(t *testing.T) {
	var asked string

	useFreshness(t, func(
		_ context.Context, _ *http.Client, installed string,
	) (runnerrelease.Freshness, error) {
		asked = installed

		return runnerrelease.Freshness{
			Installed: installed, Latest: installed, InstalledKnown: true, HistoryComplete: true,
		}, nil
	})

	out := capture(t, func() {
		if err := cmdRunner(t.Context(), []string{"check"}); err != nil {
			t.Errorf("cmdRunner: %v", err)
		}
	})

	if asked != runnerrelease.Pinned() {
		t.Errorf("billet asked github about %q while this build installs %q",
			asked, runnerrelease.Pinned())
	}

	if !strings.Contains(out, runnerrelease.Pinned()) {
		t.Errorf("the report does not name the version it asked about:\n%s", out)
	}
}

func answer(
	fresh runnerrelease.Freshness,
) func(context.Context, *http.Client, string) (runnerrelease.Freshness, error) {
	return func(context.Context, *http.Client, string) (runnerrelease.Freshness, error) {
		return fresh, nil
	}
}

func useFreshness(
	t *testing.T,
	resolve func(context.Context, *http.Client, string) (runnerrelease.Freshness, error),
) {
	t.Helper()

	saved := resolveRunnerFreshness

	t.Cleanup(func() { resolveRunnerFreshness = saved })

	resolveRunnerFreshness = resolve
}
