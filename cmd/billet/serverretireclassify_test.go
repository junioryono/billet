package main

import (
	"os"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// THE HOST THE CLASSIFIER EXISTS FOR IS THE ONE IT USED TO REFUSE. A completed
// server-only retirement has NO installed configuration — that is what the
// transition does — and its identity is at the archive, so a dry run that
// loaded the configuration answered a configuration refusal about the very
// state its caller most needs classified. It reports instead: the configuration
// as a typed fact, the row read through the journal's LOCATOR, and the phase
// the host actually holds.
func TestTheDryRunClassifiesARetiredHostRatherThanRefusingIt(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	settleRetirement(t, f)

	if _, err := os.Lstat(f.cfg); !os.IsNotExist(err) {
		t.Fatalf("this host still has a configuration, so it is not the case under test: %v", err)
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported {
		t.Fatalf("a retired host was not classified: %s", out)
	}

	if m["config"] != "absent" {
		t.Fatalf("the configuration was not reported as absent: %s", out)
	}

	// THE STATE IS THE HOST'S, read when the report is made. Pinned to
	// `nothing` it said the opposite of the truth here.
	if m["state"] != string(retirement.PhaseDone) {
		t.Fatalf("the report's state is not the journal's phase: %s", out)
	}

	journal := asMap(m["journal"])
	if journal["phase"] != string(retirement.PhaseDone) || journal["settled"] != true {
		t.Fatalf("the report's journal: %s", out)
	}

	// AND THE ROW WAS READ THROUGH THE LOCATOR: the configuration names no
	// ledger and the journal does, which is the whole point of recording it.
	if m["row_fact"] == string(retirement.RowUnreadable) {
		t.Fatalf("the row was not read through the journal's locator: %s", out)
	}

	if asMap(m["row"])["state"] != state.RetirementDone {
		t.Fatalf("the row the locator read: %s", out)
	}
}

// EVERY SHAPE OF CONFIGURATION IS A FACT THIS REPORT CARRIES. A classifier that
// refused on one of them would make the host holding it the one nothing can
// ask about, and a caller that read a file it could not parse as an absence
// would converge a damaged host as a fresh one.
func TestTheDryRunTypesTheConfigurationRatherThanRefusingOnIt(t *testing.T) {
	for name, c := range map[string]struct {
		stage func(t *testing.T, path string)
		want  string
	}{
		"a configuration that is not there": {
			stage: func(t *testing.T, path string) {
				t.Helper()
				mustOK(t, os.Remove(path))
			},
			want: "absent",
		},
		"bytes that do not parse": {
			stage: func(t *testing.T, path string) {
				t.Helper()
				writeFile(t, path, "server: [this is not a mapping\n", 0o600)
			},
			want: "malformed",
		},
		"a configuration that reads": {
			stage: func(t *testing.T, _ string) { t.Helper() },
			want:  "present",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixture(t)

			c.stage(t, f.cfg)

			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

			m := retireAnswer(t, out)
			if code != 0 || m["outcome"] != retireOutcomeReported {
				t.Fatalf("a %s was not classified: %s", name, out)
			}

			if m["config"] != c.want {
				t.Fatalf("the configuration was reported as %v, want %s: %s", m["config"], c.want, out)
			}

			// A configuration that names no ledger, on a host whose journal
			// names none either, leaves the row unread WITH ITS REASON, never
			// an absence a caller could act on. With a journal the locator is
			// tried instead, which the locator test covers.
			if c.want != "present" && m["row_fact"] != string(retirement.RowUnreadable) {
				t.Fatalf("a configuration that names no ledger left the row: %s", out)
			}
		})
	}
}

// THE REPORT'S `state` IS READ FROM THE HOST, like every other answer this
// command gives: `nothing` only when there is neither a journal nor a published
// status, and the journal's phase whenever there is one.
func TestTheDryRunsStateIsTheHostsAndNotAConstant(t *testing.T) {
	f := newRetireFixture(t)

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

	m := retireAnswer(t, out)
	if code != 0 || m["state"] != stateNothingRetire {
		t.Fatalf("a host with no retirement: %s", out)
	}

	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	out, code = f.run(t, "", "--dry-run", "--retiring-host", "control-a")

	m = retireAnswer(t, out)
	if code != 0 || m["state"] != string(retirement.PhaseIntent) {
		t.Fatalf("a host at intent: %s", out)
	}
}

// A DRY RUN STILL TAKES NOTHING, AND THE LOCATOR READ IS WHERE THAT IS EASIEST
// TO LOSE. The tail's own opener takes the directory lock at the archive and
// creates the directory if it is not there, which is right for a run about to
// write the deployment's row and wrong for one whose whole contract is that it
// looks. The damaging case is this one: a retirement at `intent` whose archive
// does not exist yet, classified by a converge that would then have CREATED THE
// DESTINATION the transition is about to rename onto.
func TestTheDryRunTakesNothingThroughTheLocator(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)

	// The configuration names no ledger, so the row is read through the
	// locator; the archive the locator names is not there yet.
	mustOK(t, os.Remove(f.cfg))

	if _, err := os.Lstat(j.Locator.Archive); !os.IsNotExist(err) {
		t.Fatalf("the archive already exists, so this case proves nothing: %v", err)
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported {
		t.Fatalf("the dry run: %s", out)
	}

	// THE ROW WAS NOT READ — there is no directory to read it through — and
	// nothing was created to make one.
	if m["row_fact"] != string(retirement.RowUnreadable) {
		t.Fatalf("a locator naming an absent archive answered a row: %s", out)
	}

	if _, err := os.Lstat(j.Locator.Archive); !os.IsNotExist(err) {
		t.Fatalf("the dry run created the archive the transition is about to rename onto: %v", err)
	}
}

// AND IT CREATES NOTHING ON THE ORDINARY PATH EITHER: no upgrade root by
// classifying a guard, no configuration by finding none.
func TestTheDryRunStillCreatesNothing(t *testing.T) {
	f := newRetireFixture(t)

	mustOK(t, os.Remove(f.cfg))

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	if m := retireAnswer(t, out); code != 0 || m["outcome"] != retireOutcomeReported {
		t.Fatalf("the dry run: %s", out)
	}

	for _, path := range []string{f.guard.root, f.cfg} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the dry run created %s: %v", path, err)
		}
	}
}
