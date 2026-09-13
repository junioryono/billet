package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

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

	if m["status_presence"] != "present" || asMap(m["status"])["phase"] != string(retirement.PhaseDone) {
		t.Fatalf("the published status was not reported as present at done: %s", out)
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

// THE STATUS IS A PUBLICATION THE JOURNAL CAN REPAIR. Refusing its damaged
// bytes would keep the caller from reaching the settled journal that tells
// the next run what to publish; the classifier reports the damage and leaves
// the bytes alone.
func TestTheDryRunReportsAMalformedStatusBesideASettledJournal(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)

	settleRetirement(t, f)

	body := `{"phase":"damaged","variant":"server-only","updated_at":"2026-09-11T10:00:00Z"}` + "\n"
	writeFile(t, retirement.StatusPath(), body, 0o644)

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported {
		t.Fatalf("a repairable publication refused the classifier: %s", out)
	}

	if m["status_presence"] != "malformed" || m["status"] != nil {
		t.Fatalf("the damaged status was not reported as malformed: %s", out)
	}

	why, ok := m["status_why"].(string)
	if !ok || !strings.Contains(why, `names the phase "damaged", which this billet does not know`) {
		t.Fatalf("the status reader's diagnostic was lost: %s", out)
	}

	journal := asMap(m["journal"])
	if journal["phase"] != string(retirement.PhaseDone) || journal["settled"] != true ||
		journal["transition_id"] != retireTestID {
		t.Fatalf("the publication hid the settled journal: %s", out)
	}

	if m["row_fact"] != string(retirement.RowDoneMine) {
		t.Fatalf("the publication stopped the row observation: %s", out)
	}

	after, err := os.ReadFile(retirement.StatusPath())
	mustOK(t, err)

	if string(after) != body {
		t.Fatalf("the classifier repaired the status: %s", after)
	}
}

// A READ THAT FAILED IS NOT BYTES THAT DID NOT PARSE. A directory at the
// status path is refused by the regular-file reader on both platforms, even
// as root; chmod would not establish an unreadable file for that account.
func TestTheDryRunStillRefusesAnUnreadableStatus(t *testing.T) {
	f := newRetireFixture(t)
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	mustOK(t, os.Mkdir(retirement.StatusPath(), 0o700))

	_, presence, err := retirement.ReadStatus()
	if presence != retirement.StatusUnreadable || err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("the status was not unreadable, so this case proves nothing: %d %v", presence, err)
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

	m := retireAnswer(t, out)
	if code != exitUnknown || m["outcome"] != retireOutcomeUnknown || m["reason"] != retireReasonStatus {
		t.Fatalf("an unreadable status did not refuse: %s", out)
	}

	why, ok := m["why"].(string)
	if !ok || !strings.Contains(why, "the authority status could not be judged:") ||
		!strings.Contains(why, "not a regular file") {
		t.Fatalf("the failed read's diagnostic was lost: %s", out)
	}
}

// THE JOURNAL NAMES THE PARTICIPANTS, even when today's inventory names
// someone else. A standalone dry run admits no --survivor-host, so this calls
// the classifier with both operands set to prove neither supplies the record.
func TestTheDryRunReportsTheJournalsHostsRatherThanTheInvocations(t *testing.T) {
	f := newRetireFixture(t)
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	answer, refusal := retireDryRun(t.Context(), retireMode{configPath: f.cfg,
		retiringHost: "inventory-retiring", survivorHost: "inventory-survivor"})
	if refusal != nil {
		t.Fatalf("the classifier refused the journal: %+v", refusal)
	}

	report, ok := answer.(*retireReport)
	if !ok || report.Outcome != retireOutcomeReported || report.Journal == nil {
		t.Fatalf("the classifier did not report the journal: %+v", answer)
	}

	if report.Journal.Retiring != "control-a" || report.Journal.Survivor != "control-b" {
		t.Fatalf("the reported hosts are not the journal's: %+v", report.Journal)
	}

	// THE JSON IS WHAT THE ROLE READS; the typed answer alone cannot prove
	// the member names it needs are published.
	out, code := f.run(t, "", "--dry-run", "--retiring-host", "inventory-retiring")
	m := retireAnswer(t, out)
	journal := asMap(m["journal"])

	if code != 0 || m["outcome"] != retireOutcomeReported || journal["retiring"] != "control-a" ||
		journal["survivor"] != "control-b" {
		t.Fatalf("the journal's hosts did not reach the command's answer: %s", out)
	}

	if m["status_presence"] != "absent" || m["status"] != nil {
		t.Fatalf("an absent status was not reported as absent: %s", out)
	}
}

// ONE BOUND COVERS EITHER ROUTE TO THE ROW. Its expiry leaves a local journal
// visible and the command successful, because a caller can recover from that
// record without the ledger; a deadline is never evidence of an absent row.
func TestTheDryRunReportsTheJournalWhenTheRowObservationBoundEnds(t *testing.T) {
	for _, locator := range []bool{false, true} {
		name := "through the configuration"
		if locator {
			name = "through the settled journal's locator"
		}

		t.Run(name, func(t *testing.T) {
			var f *retireFixture

			phase := retirement.PhaseIntent
			if locator {
				request := newRequestFixture(t)
				request.reserve(t)
				settleRetirement(t, request)
				f, phase = request.retireFixture, retirement.PhaseDone
			} else {
				f = newRetireFixture(t)
				f.journalAt(t, phase, "ci-1")
			}

			saved := retireReportLedgerBound
			retireReportLedgerBound = -time.Nanosecond

			t.Cleanup(func() { retireReportLedgerBound = saved })

			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

			m := retireAnswer(t, out)
			if code != 0 || m["outcome"] != retireOutcomeReported || m["row_fact"] != string(retirement.RowUnreadable) ||
				m["row"] != nil {
				t.Fatalf("an expired row observation stopped the report or answered a row: %s", out)
			}

			why, ok := m["why"].(string)
			if !ok || !strings.Contains(why, "the retirement row observation exceeded retireReportLedgerBound (-1ns)") {
				t.Fatalf("the row's unreadability did not name its bound: %s", out)
			}

			journal := asMap(m["journal"])
			if journal["phase"] != string(phase) || journal["settled"] != locator || journal["transition_id"] != retireTestID {
				t.Fatalf("the row's deadline hid the local journal: %s", out)
			}
		})
	}
}

// THE CALLER'S ENDING IS ITS OWN FACT. A cancelled caller must not be told
// the ledger spent the report's budget when that budget is still available.
func TestTheDryRunDistinguishesTheCallersCancellationFromItsRowBound(t *testing.T) {
	f := newRetireFixture(t)
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	answer, refusal := retireDryRun(ctx, retireMode{configPath: f.cfg, retiringHost: "control-a"})
	if refusal != nil {
		t.Fatalf("the row's cancellation refused the local report: %+v", refusal)
	}

	report, ok := answer.(*retireReport)
	if !ok || report.Outcome != retireOutcomeReported || report.RowFact != retirement.RowUnreadable || report.Row != nil {
		t.Fatalf("a cancelled row observation answered a row: %+v", answer)
	}

	if !strings.Contains(report.Why, "the caller's context ended") || strings.Contains(report.Why, "retireReportLedgerBound") {
		t.Fatalf("the caller's cancellation was called the report's bound: %s", report.Why)
	}

	if report.Journal == nil || report.Journal.Phase != retirement.PhaseIntent {
		t.Fatalf("the caller's cancellation hid the local journal: %+v", report.Journal)
	}
}
