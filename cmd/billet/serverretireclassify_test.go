package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
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

	assertRetireRoute(t, out, code, "continue", "readable journal")

	if m["identity"] != "unreadable" || m["authority"] != "unreadable" || m["installed_roles"] != "" {
		t.Fatalf("a retired host invented an installed identity path or roles: %s", out)
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
			wantRoute := "hold"
			if c.want == "present" {
				wantRoute = "ordinary"
			} else if m["installed_roles"] != "" || m["identity"] != "unreadable" || m["authority"] != "unreadable" {
				t.Fatalf("an unknown configuration invented installed roles or an identity path: %s", out)
			}
			assertRetireRoute(t, out, code, wantRoute, "row is unreadable")

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

	assertRetireRoute(t, out, code, "continue", "readable journal")

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
func TestTheDryRunHoldsOnAnUnreadableStatus(t *testing.T) {
	f := newRetireFixture(t)
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	mustOK(t, os.Mkdir(retirement.StatusPath(), 0o700))

	_, presence, err := retirement.ReadStatus()
	if presence != retirement.StatusUnreadable || err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("the status was not unreadable, so this case proves nothing: %d %v", presence, err)
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")

	m := retireAnswer(t, out)
	assertRetireRoute(t, out, code, "hold", "The authority status could not be judged:")
	if m["status_presence"] != "unreadable" {
		t.Fatalf("an unreadable status was not reported: %s", out)
	}

	why, ok := m["status_why"].(string)
	if !ok || !strings.Contains(why, "not a regular file") {
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

// AN OBSERVATION WHOSE BUDGET ENDED BEFORE ENTRY ASKS NOTHING. Its journal
// still answers locally; this case alone proves nothing about a running read.
func TestTheDryRunReportsTheJournalWhenTheRowBoundEndedBeforeEntry(t *testing.T) {
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
			assertRetireRoute(t, out, code, "continue", "readable journal")
			wantIdentity, wantAuthority := "minted", "absent"
			if locator {
				wantIdentity, wantAuthority = "unreadable", "unreadable"
			}
			if m["identity"] != wantIdentity || m["authority"] != wantAuthority {
				t.Fatalf("a skipped row observation concealed the identity facts: %s", out)
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

// ONE BOUND REACHES THE WORK ON EITHER ROUTE. The real open and its binding
// verification, and then the snapshot, receive that context, never the
// caller's still-live one. A SUCCESS RETURNED LATE IS NOT EVIDENCE: on the
// ordinary route the successful snapshot holds no row, and must not publish
// absence. Each seam ends the budget only after that operation was entered.
func TestTheDryRunReportsTheJournalWhenTheRowObservationBoundEnds(t *testing.T) {
	for _, locator := range []bool{false, true} {
		for _, step := range []string{"open and binding", "snapshot", "successful snapshot", "successful close"} {
			name := "configuration/" + step
			if locator {
				name = "locator/" + step
			}

			t.Run(name, func(t *testing.T) {
				f, phase := retireObservationFixture(t, locator)
				entered, expire := pinRetireObservationDeadline(t)
				savedOpen, savedLocator := retireReportOpen, retireReportOpenByLocator
				savedSnapshot, savedClose := retireReportSnapshot, retireReportClose
				t.Cleanup(func() {
					retireReportOpen, retireReportOpenByLocator = savedOpen, savedLocator
					retireReportSnapshot, retireReportClose = savedSnapshot, savedClose
				})

				opens, reads, closes := 0, 0, 0

				retireReportOpen = func(ctx context.Context, cfg *config.Config, dsn string) (*state.DB, error) {
					opens++
					db, err := savedOpen(ctx, cfg, dsn)
					mustOK(t, err)
					entered(ctx)

					// THE HANDLE STILL BELONGS TO THE READER. Returning it after
					// the budget ended cannot evade the close or turn the next
					// read into an absence.
					if step == "open and binding" {
						expire()
					}

					return db, nil
				}
				retireReportOpenByLocator = func(ctx context.Context, j retirement.Journal) (*state.DB, ledgerProblem) {
					opens++
					db, problem := savedLocator(ctx, j)
					if problem.refusal != nil || problem.pending != "" || problem.cause != nil || db == nil {
						t.Fatalf("the locator did not open and verify the ledger: %+v", problem)
					}
					entered(ctx)

					if step == "open and binding" {
						expire()
					}

					return db, problem
				}
				retireReportSnapshot = func(ctx context.Context, db *state.DB) (rollout.StatusSnapshot, error) {
					reads++

					entered(ctx)
					if err := ctx.Err(); err != nil && step != "open and binding" {
						t.Fatalf("the budget ended before the snapshot was entered: %v", err)
					}

					if step == "snapshot" {
						expire()
					}

					if step == "open and binding" || step == "snapshot" {
						snapshot, err := savedSnapshot(ctx, db)
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("the snapshot did not receive the deadline: %v", err)
						}

						return snapshot, err
					}

					snapshot, err := savedSnapshot(ctx, db)
					mustOK(t, err)
					if snapshot.Binding != f.identity || (!locator && snapshot.Retirement != nil) ||
						(locator && (snapshot.Retirement == nil || snapshot.Retirement.State != state.RetirementDone)) {
						t.Fatalf("the successful observation did not establish the case: %+v", snapshot)
					}

					if step == "successful snapshot" {
						expire()
					}

					return snapshot, nil
				}
				retireReportClose = func(db *state.DB) error {
					closes++
					mustOK(t, savedClose(db))
					if step == "successful close" {
						expire()
					}

					return nil
				}

				out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
				assertRetireObservationUnreadable(t, out, code, phase, locator, true)
				if opens != 1 || reads != 1 || closes != 1 {
					t.Fatalf("the observation made %d opens, %d snapshots and %d closes, want one of each", opens, reads, closes)
				}
			})
		}
	}
}

// A FAILED CLOSE IS ITS OWN FACT, beside a failed read or an ended budget.
// Neither the snapshot's success nor another failure can discard it, and the
// local journal remains usable even when all three facts arrive together.
func TestTheDryRunKeepsTheCloseFailureBesideTheReadAndItsDeadline(t *testing.T) {
	for _, locator := range []bool{false, true} {
		for _, failedRead := range []bool{false, true} {
			for _, expired := range []bool{false, true} {
				name := "configuration"
				if locator {
					name = "locator"
				}
				if failedRead {
					name += "/failed read"
				}
				if expired {
					name += "/expired"
				}

				t.Run(name, func(t *testing.T) {
					f, phase := retireObservationFixture(t, locator)
					entered, expire := pinRetireObservationDeadline(t)
					savedSnapshot, savedClose := retireReportSnapshot, retireReportClose
					t.Cleanup(func() { retireReportSnapshot, retireReportClose = savedSnapshot, savedClose })

					closes := 0

					retireReportSnapshot = func(ctx context.Context, db *state.DB) (rollout.StatusSnapshot, error) {
						entered(ctx)
						snapshot, err := savedSnapshot(ctx, db)
						mustOK(t, err)
						if failedRead {
							return rollout.StatusSnapshot{}, errors.New("the snapshot's connection failed")
						}

						return snapshot, nil
					}
					retireReportClose = func(db *state.DB) error {
						closes++
						mustOK(t, savedClose(db))
						if expired {
							expire()
						}

						return errors.New("the ledger's pools did not close")
					}

					out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
					why := assertRetireObservationUnreadable(t, out, code, phase, locator, expired)
					if closes != 1 || !strings.Contains(why,
						"close the ledger after the row observation: the ledger's pools did not close") {
						t.Fatalf("the close failure was discarded (%d closes): %s", closes, out)
					}
					if strings.Contains(why, "the snapshot's connection failed") != failedRead {
						t.Fatalf("the read's own answer was lost or invented: %s", out)
					}
				})
			}
		}
	}
}

// THE OWNER GETS THE OBSERVATION'S REMAINING BUDGET. Entry is established
// before it ends; an error, a killed child's exit code and a successful empty
// report all leave the row unreadable and the journal available locally.
func TestTheDryRunBoundsTheOwnersRunningReport(t *testing.T) {
	for _, result := range []string{"error", "exit", "success"} {
		t.Run(result, func(t *testing.T) {
			f, phase := retireObservationFixture(t, false)
			expire := pinRetireObservationDeadline(t)
			savedEUID, savedOwner, savedReexec := statusEUID, statusOwnerOf, retireReexecCapture
			t.Cleanup(func() { statusEUID, statusOwnerOf, retireReexecCapture = savedEUID, savedOwner, savedReexec })

			statusEUID = func() int { return 0 }
			statusOwnerOf = func(string) (uint32, uint32, error) { return 990, 991, nil }
			calls := 0

			retireReexecCapture = func(ctx context.Context, uid, gid uint32, args []string) ([]byte, int, error) {
				calls++
				if uid != 990 || gid != 991 || strings.Join(args, " ") != "rollout status --json --config "+f.cfg {
					t.Fatalf("the owner's invocation was %d:%d %v", uid, gid, args)
				}

				expire()

				switch result {
				case "error":
					return nil, 0, ctx.Err()
				case "exit":
					return nil, 137, nil
				default:
					return mustMarshal(t, rolloutStatusReport{Schema: rolloutStatusSchema,
						Deployment: rolloutStatusDeployment{Bound: true, ID: f.identity}}), 0, nil
				}
			}

			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
			why := assertRetireObservationUnreadable(t, out, code, phase, false, true)
			if calls != 1 {
				t.Fatalf("the report ran as the owner %d times, want once", calls)
			}
			if result == "exit" && !strings.Contains(why, "the owner's report exited 137 while its context ended") {
				t.Fatalf("the child's exit code was lost: %s", out)
			}
		})
	}
}

// retireObservationFixture leaves the journal visible on both routes; the
// locator's ledger is real PostgreSQL, as it is for the completed retirement.
func retireObservationFixture(t *testing.T, locator bool) (*retireFixture, retirement.Phase) {
	t.Helper()

	if locator {
		f := newRequestFixture(t)
		f.reserve(t)
		settleRetirement(t, f)

		return f.retireFixture, retirement.PhaseDone
	}

	f := newRetireFixture(t)
	f.journalAt(t, retirement.PhaseIntent, "ci-1")

	return f, retirement.PhaseIntent
}

// retireObservationDeadline ends on demand, with the deadline error and Done
// signal a real timeout supplies. The seam avoids a race between fixture I/O
// and a short timer: the budget ends only once the operation has entered.
type retireObservationDeadline struct {
	context.Context
	at time.Time
}

func (c *retireObservationDeadline) Deadline() (time.Time, bool) { return c.at, true }

func (c *retireObservationDeadline) Err() error {
	if errors.Is(context.Cause(c.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}

	return c.Context.Err()
}

// pinRetireObservationDeadline takes the observation's one budget and hands
// back two things: `entered`, which a seam calls to prove it was given that
// live budget and not some other context, and `expire`, which ends it where a
// case wants the deadline to fall INSIDE the operation.
//
// THE CONTEXT ITSELF STAYS HERE. A case that stored it in its own variable to
// reach it from a later seam is the shape `fatcontext` refuses, and it does not
// need to: what a seam has to establish is that it entered with the budget, and
// that is a question this helper can answer without the caller holding one.
func pinRetireObservationDeadline(t *testing.T) (entered func(context.Context), expire func()) {
	t.Helper()

	saved := retireReportTimeout
	t.Cleanup(func() { retireReportTimeout = saved })

	var bounded *retireObservationDeadline
	var end context.CancelCauseFunc

	retireReportTimeout = func(parent context.Context, within time.Duration) (context.Context, context.CancelFunc) {
		if bounded != nil || within != retireReportLedgerBound {
			t.Fatalf("the observation replaced or changed its one budget: %s", within)
		}

		ctx, cancel := context.WithCancelCause(parent)
		bounded = &retireObservationDeadline{Context: ctx, at: time.Now().Add(within)}
		end = cancel

		return bounded, func() { cancel(context.Canceled) }
	}

	entered = func(ctx context.Context) {
		t.Helper()

		// IDENTITY, NOT LIVENESS. A seam that runs after an earlier one ended
		// the budget on purpose still entered with THAT budget, which is what
		// this establishes; whether it was live is a separate question the
		// cases that care about it ask of `ctx.Err()` themselves.
		if bounded == nil || ctx != bounded {
			t.Fatalf("the operation did not enter with the observation's context: %v", ctx)
		}
	}

	expire = func() {
		t.Helper()

		if bounded == nil {
			t.Fatal("the observation took no budget to expire")
		}

		end(context.DeadlineExceeded)

		select {
		case <-bounded.Done():
		default:
			t.Fatal("the running operation did not receive the deadline's Done signal")
		}

		if bounded.Err() != context.DeadlineExceeded {
			t.Fatalf("the running operation received %v, want deadline exceeded", bounded.Err())
		}
	}

	return entered, expire
}

func assertRetireObservationUnreadable(t *testing.T, out string, code int, phase retirement.Phase,
	settled, expired bool,
) string {
	t.Helper()

	m := assertRetireRoute(t, out, code, "continue", "readable journal")
	if m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil {
		t.Fatalf("the failed observation stopped the report or answered a row: %s", out)
	}

	why, ok := m["why"].(string)
	want := "the retirement row observation exceeded retireReportLedgerBound (" + retireReportLedgerBound.String() + ")"
	if !ok || why == "" || strings.Contains(why, want) != expired || strings.Contains(why, "the caller's context ended") {
		t.Fatalf("the observation's diagnostic did not account for its budget (expired %t): %s", expired, out)
	}

	journal := asMap(m["journal"])
	if journal["phase"] != string(phase) || journal["settled"] != settled || journal["transition_id"] != retireTestID ||
		m["state"] != string(phase) {
		t.Fatalf("the failed observation hid the local journal: %s", out)
	}

	return why
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

// THE JOURNAL SELECTS EVEN WHEN THE DISPATCH CANNOT. Without one, only a
// positively associated row distinguishes cancellation from the survivor's
// ordinary converge; a commissioned controller with no row is ordinary too.
func TestTheDryRunRoutesTheWholeRowAndJournalPartition(t *testing.T) {
	for _, row := range []struct {
		state     string
		host      string
		fact      retirement.RowFact
		ordinary  string
		requested string
		why       string
	}{
		{"absent", "control-a", retirement.RowAbsent, "ordinary", "hold", "PostgreSQL active-passive"},
		{"reserved", "control-a", retirement.RowReservedMine, "cancel", "hold", "PostgreSQL active-passive"},
		{"intent", "control-a", retirement.RowIntentMine, "hold", "hold", "past reservation"},
		{"done", "control-a", retirement.RowDoneMine, "hold", "hold", "past reservation"},
		{"reserved", "control-b", retirement.RowOtherReserved, "ordinary", "hold", "another host (control-a)"},
		{"intent", "control-b", retirement.RowOtherIntent, "ordinary", "hold", "another host (control-a)"},
		{"done", "control-b", retirement.RowDoneOther, "ordinary", "hold", "another host (control-a)"},
		{"unreadable", "control-a", retirement.RowUnreadable, "hold", "hold", "row is unreadable"},
	} {
		for _, phase := range []retirement.Phase{"", retirement.PhaseIntent, retirement.PhaseStopped, retirement.PhaseDone} {
			t.Run(row.state+"/"+row.host+"/"+string(phase), func(t *testing.T) {
				f := newRetireFixture(t)
				if row.state != "absent" && row.state != "unreadable" {
					r := f.reserveRow(t, "ci-1")
					f.ledger(t, func(db *state.DB) {
						switch row.state {
						case "intent":
							mustOK(t, db.AdvanceRetirementToIntent(t.Context(), f.identity, r.Retiring,
								r.TransitionID, r.Run, retireNow()))
						case "done":
							_, _, err := db.CompleteRetirement(t.Context(), state.RetirementCompletion{
								Deployment: f.identity, Retiring: r.Retiring, Survivor: r.Survivor,
								TransitionID: r.TransitionID, ReservedAt: r.ReservedAt,
								CompletedBy: r.Survivor, At: retireNow(),
							})
							mustOK(t, err)
						}
					})
				}

				if phase != "" {
					f.journalAt(t, phase, "ci-1")
				}
				if row.state == "unreadable" {
					writeFile(t, state.LedgerPath(f.stateDir), "not a database", 0o600)
				}

				for _, requested := range []bool{false, true} {
					args := []string{"--dry-run", "--retiring-host", row.host}
					want, why := row.ordinary, row.why
					if requested {
						args = append(args, "--requested")
						want = row.requested
					} else if want == "cancel" {
						why = "inventory no longer requests"
					}
					if phase != "" {
						want, why = "continue", "readable journal"
					}

					out, code := f.run(t, "", args...)
					m := assertRetireRoute(t, out, code, want, why)
					if m["row_fact"] != string(row.fact) || m["identity"] != "minted" ||
						m["authority"] != "absent" || m["installed_roles"] != "server" {
						t.Fatalf("the row or the installed host did not establish the case: %s", out)
					}
				}
			})
		}
	}
}

// REQUESTED QUALIFIES A NEW REQUEST; it never chooses a journal's route.
// The same eligible host is ordinary without it and adopts its reservation
// with it. Installed node custody remains ineligible after either observation.
func TestTheDryRunQualifiesRequestsFromTheInstalledPair(t *testing.T) {
	f := newRequestFixture(t)

	for _, reserved := range []bool{false, true} {
		if reserved {
			f.reserve(t)
		}
		for _, requested := range []bool{false, true} {
			args := []string{"--dry-run", "--retiring-host", requestRetiring}
			want, why := "ordinary", ""
			if reserved {
				want, why = "cancel", "inventory no longer requests"
			}
			if requested {
				args = append(args, "--requested")
				want, why = "new-request", "eligible"
			}
			out, code := f.run(t, "", args...)
			m := assertRetireRoute(t, out, code, want, why)
			if m["identity"] != "minted" || m["authority"] != "present" || m["installed_roles"] != "server" {
				t.Fatalf("the installed pair was not observed: %s", out)
			}
		}
	}

	if len(f.svc.trace) != 0 {
		t.Fatalf("the classifier changed a service: %v", f.svc.trace)
	}
	if _, err := os.Lstat(filepath.Join(f.unitsDir, ".asked")); !os.IsNotExist(err) {
		t.Fatalf("the classifier asked systemd for a service observation: %v", err)
	}

	f.retainANode(t)
	for _, requested := range []bool{false, true} {
		args := []string{"--dry-run", "--retiring-host", requestRetiring}
		want, why := "cancel", "inventory no longer requests"
		if requested {
			args = append(args, "--requested")
			want, why = "hold", "installed_roles both"
		}
		out, code := f.run(t, "", args...)
		m := assertRetireRoute(t, out, code, want, why)
		if m["installed_roles"] != "both" {
			t.Fatalf("the installed node was hidden: %s", out)
		}
	}

	f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	for _, extra := range [][]string{nil, {"--requested"}} {
		args := append([]string{"--dry-run", "--retiring-host", requestRetiring}, extra...)
		out, code := f.run(t, "", args...)
		assertRetireRoute(t, out, code, "unsupported-variant", "retained-node")
	}
}

// A NODE-ONLY CONFIGURATION SKIPS THE LEDGER; that is no failed observation
// of a controller. Neither the default identity path nor an old ledger may
// supply evidence for a server section the installed configuration lacks.
func TestTheDryRunRecognisesANodeWithoutAttemptingALedgerRead(t *testing.T) {
	f := newRetireFixture(t)
	writeFile(t, f.cfg, "node:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n  provider: docker\n"+
		"  state_dir: "+filepath.Join(t.TempDir(), "node")+"\n", 0o600)
	writeFile(t, state.LedgerPath(f.stateDir), "not a database", 0o600)
	saved := retireReportOpen
	t.Cleanup(func() { retireReportOpen = saved })
	retireReportOpen = func(context.Context, *config.Config, string) (*state.DB, error) {
		t.Fatal("a node-only classifier attempted a ledger read")
		return nil, errors.New("unexpected ledger read")
	}

	for _, requested := range []bool{false, true} {
		args := []string{"--dry-run", "--retiring-host", "control-a"}
		want, why := "ordinary", ""
		if requested {
			args = append(args, "--requested")
			want, why = "hold", "installed_roles node"
		}
		out, code := f.run(t, "", args...)
		m := assertRetireRoute(t, out, code, want, why)
		if m["config"] != "present" || m["installed_roles"] != "node" || m["identity"] != "unreadable" ||
			m["authority"] != "unreadable" || m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil {
			t.Fatalf("the skipped observation invented controller facts: %s", out)
		}
		whyRead, ok := m["why"].(string)
		if !ok || !strings.Contains(whyRead, "was not read") {
			t.Fatalf("the skipped read was called a failed read: %s", out)
		}
	}
}

// AN UNMINTED IDENTITY ADMITS ONLY BESIDE TWO POSITIVE AUTHORITY ABSENCES.
// Each remnant alone, including a dangling link, disproves a fresh host;
// unreadable identity metadata supplies no absence either.
func TestTheDryRunSeparatesUncommissionedAndDamagedControllers(t *testing.T) {
	for _, evidence := range []string{"absent", "ca", "marker", "dangling", "fifo", "unreadable identity", "unreadable authority"} {
		t.Run(evidence, func(t *testing.T) {
			f := newRetireFixture(t)
			f.cfg = writeRetirePostgresConfig(t, f.stateDir)
			mustOK(t, os.Remove(state.DeploymentIDPath(f.stateDir)))
			identity, authority := "absent", "present"
			switch evidence {
			case "absent":
				authority = "absent"
			case "ca":
				mustOK(t, os.Mkdir(wirecert.CADir(f.stateDir), 0o700))
			case "marker":
				writeFile(t, wirecert.AuthorityMarkerPath(f.stateDir), "authority existed", 0o600)
			case "dangling":
				mustOK(t, os.Symlink(filepath.Join(f.stateDir, "missing"), wirecert.CADir(f.stateDir)))
			case "fifo":
				mustOK(t, syscall.Mkfifo(wirecert.AuthorityMarkerPath(f.stateDir), 0o600))
			case "unreadable identity":
				mustOK(t, os.Mkdir(state.DeploymentIDPath(f.stateDir), 0o700))
				identity, authority = "unreadable", "absent"
			case "unreadable authority":
				f.stateDir = filepath.Join(t.TempDir(), "loop")
				mustOK(t, os.Symlink(f.stateDir, f.stateDir))
				f.cfg = writeRetirePostgresConfig(t, f.stateDir)
				identity, authority = "unreadable", "unreadable"
			}

			for _, requested := range []bool{false, true} {
				args := []string{"--dry-run", "--retiring-host", "control-a"}
				want, why := "hold", "controller is damaged"
				if identity == "unreadable" {
					why = "row is unreadable"
				}
				if requested {
					args = append(args, "--requested")
				}
				if evidence == "absent" {
					want, why = "ordinary", ""
					if requested {
						want, why = "new-request", "eligible"
					}
				}
				out, code := f.run(t, "", args...)
				m := assertRetireRoute(t, out, code, want, why)
				if m["identity"] != identity || m["authority"] != authority || m["row_fact"] != string(retirement.RowUnreadable) {
					t.Fatalf("the admission did not use the independently planted evidence: %s", out)
				}
			}
		})
	}
}

// A SHARED ROW BELONGS TO THIS HOST ONLY UNDER ITS DEPLOYMENT BINDING.
// Replacing the local identity cannot turn either host's reservation into
// permission; a readable but unbound ledger withholds the same association.
func TestTheDryRunRequiresTheBindingBeforeAssociatingEitherHost(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "foreign"}[bound], func(t *testing.T) {
			f := newRetireFixture(t)
			if !bound {
				f.stateDir = t.TempDir()
				f.cfg = writeCAConfig(t, f.stateDir)
				id, err := state.DeploymentID(f.stateDir)
				mustOK(t, err)
				f.identity = id
			}
			f.reserveRow(t, "ci-1")
			if bound {
				writeFile(t, state.DeploymentIDPath(f.stateDir), strings.Repeat("e", 32)+"\n", 0o600)
			}
			for _, host := range []string{"control-a", "control-b"} {
				for _, extra := range [][]string{nil, {"--requested"}} {
					args := append([]string{"--dry-run", "--retiring-host", host}, extra...)
					out, code := f.run(t, "", args...)
					m := assertRetireRoute(t, out, code, "hold", "row is unreadable")
					if m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil || m["identity"] != "minted" {
						t.Fatalf("an unassociated reservation was assigned a host: %s", out)
					}
				}
			}
		})
	}
}

// CONTINUITY IS THIS CONVERGE'S EVIDENCE, not a standalone preview's
// prerequisite. Losing either the holder or the prepared id holds even an
// ordinary host; omitting the expected holder applies neither comparison.
func TestTheDryRunHoldsOnlyTheGuardContinuityItWasGiven(t *testing.T) {
	for _, guard := range []string{"gone", "other holder", "other id", "same"} {
		t.Run(guard, func(t *testing.T) {
			f := newRetireFixture(t)
			if guard != "gone" {
				holder := "ci-1"
				if guard == "other holder" {
					holder = "ci-other"
				}
				mustHold(t, holder)
				rec := f.guard.record(t)
				rec.ID = retireTestID
				if guard == "other id" {
					rec.ID = markerID
				}
				writeFile(t, filepath.Join(f.guard.active(), guardRecordName), string(mustMarshal(t, rec)), 0o600)
			}

			for _, journal := range []bool{false, true} {
				if journal {
					f.journalAt(t, retirement.PhaseIntent, "ci-1")
				}
				for _, expected := range []bool{false, true} {
					args := []string{"--dry-run", "--retiring-host", "control-a", "--expected-guard", retireTestID}
					want, why := "ordinary", ""
					if journal {
						want, why = "continue", "readable journal"
					}
					if expected {
						args = append(args, "--expected-holder", "ci-1")
						switch guard {
						case "gone":
							want, why = "hold", "guard is gone"
						case "other holder":
							want, why = "hold", "holder ci-other"
						case "other id":
							want, why = "hold", "guard id differs"
						}
					}
					out, code := f.run(t, "", args...)
					assertRetireRoute(t, out, code, want, why)
				}
			}
		})
	}
}

// A BINARY POINTER EXCLUDES EVERY RETIREMENT ROUTE UNDER THE CONVERGE'S
// GUARD. Ordinary recovery still reaches the tasks that own that pointer,
// and a standalone preview does not claim continuity it never established.
func TestTheDryRunKeepsBinaryRecoveryOnTheOrdinaryRoute(t *testing.T) {
	for _, route := range []string{"ordinary", "cancel", "continue", "unsupported-variant"} {
		t.Run(route, func(t *testing.T) {
			f := newRetireFixture(t)
			mustHold(t, "ci-1")
			extra := []string{}
			switch route {
			case "cancel":
				f.reserveRow(t, "ci-1")
			case "continue", "unsupported-variant":
				f.journalAt(t, retirement.PhaseIntent, "ci-1")
				if route == "unsupported-variant" {
					j, presence, err := retirement.ReadJournal()
					mustOK(t, err)
					if presence != retirement.JournalPresent {
						t.Fatal("the fixture's journal is absent")
					}
					j.Variant, j.Config = retirement.VariantRetainedNode, "present"
					j.StagedSHA256 = strings.Repeat("a", 64)
					mustOK(t, j.Write(retireNow()))
				}
			}

			pointer := filepath.Join(f.guard.active(), guardPointerName)
			recovery := filepath.Join(f.guard.root, "recovery-20260911T100000-0badcafe")
			mustOK(t, os.Mkdir(recovery, 0o700))
			mustOK(t, os.Symlink(recovery, pointer))
			for _, expected := range []bool{false, true} {
				args := append([]string{"--dry-run", "--retiring-host", "control-a"}, extra...)
				want, why := route, ""
				if expected {
					args = append(args, "--expected-holder", "ci-1")
					if route != "ordinary" {
						want, why = "hold", "binary transaction's pointer"
					}
				}
				out, code := f.run(t, "", args...)
				assertRetireRoute(t, out, code, want, why)
			}
		})
	}
}

// AND THE SAME RULE ON A NEW REQUEST, which needs a COMMISSIONED pair and so
// cannot be staged beside the routes above: an uncommissioned host is refused
// by name now, and a host with no ledger to read holds for that instead.
func TestTheDryRunKeepsABinaryPointerOffANewRequest(t *testing.T) {
	f := newRequestFixture(t)

	pointer := filepath.Join(f.guard.active(), guardPointerName)
	recovery := filepath.Join(f.guard.root, "recovery-20260911T100000-0badcafe")

	mustOK(t, os.Mkdir(recovery, 0o700))
	mustOK(t, os.Symlink(recovery, pointer))

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested")
	assertRetireRoute(t, out, code, "new-request", "eligible")

	out, code = f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested",
		"--expected-holder", requestRun)
	assertRetireRoute(t, out, code, "hold", "binary transaction's pointer")
}

// NO LOCAL ARTEFACT EXPLAINS ITSELF. A status, stage or marker with no
// journal holds even when the row is this host's cancellable reservation.
func TestTheDryRunHoldsOnUnexplainedRetirementArtefacts(t *testing.T) {
	for _, artefact := range []string{"status", "malformed status", "stage", "marker"} {
		t.Run(artefact, func(t *testing.T) {
			f := newRetireFixture(t)
			f.reserveRow(t, "ci-1")
			switch artefact {
			case "status":
				mustOK(t, retirement.WriteStatus(retirement.PhaseDone, retirement.VariantServerOnly, retireNow()))
			case "malformed status":
				writeFile(t, retirement.StatusPath(), "not JSON", 0o644)
			case "stage":
				mustOK(t, retirement.WriteStage([]byte("staged configuration")))
			case "marker":
				mustHold(t, "ci-1")
				markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
			}
			for _, extra := range [][]string{nil, {"--requested"}} {
				args := append([]string{"--dry-run", "--retiring-host", "control-a"}, extra...)
				out, code := f.run(t, "", args...)
				m := assertRetireRoute(t, out, code, "hold", "no journal to explain it")
				if m["row_fact"] != string(retirement.RowReservedMine) {
					t.Fatalf("the artefact concealed the readable reservation: %s", out)
				}
			}
		})
	}
}

// UNREADABLE LOCAL RECORDS HOLD BEFORE A JOURNAL SELECTS, and the first
// failed observation keeps its reason even when another one fails too.
func TestTheDryRunReportsFailedLocalObservationsAsHolds(t *testing.T) {
	for _, broken := range []string{"journal unreadable", "journal malformed", "claim unknown", "claim unpublished",
		"guard record", "configuration"} {
		t.Run(broken, func(t *testing.T) {
			f := newRetireFixture(t)
			f.journalAt(t, retirement.PhaseIntent, "ci-1")
			why := ""
			switch broken {
			case "journal unreadable":
				mustOK(t, os.Remove(retirement.JournalPath()))
				mustOK(t, os.Mkdir(retirement.JournalPath(), 0o700))
				mustOK(t, os.Mkdir(retirement.StatusPath(), 0o700))
				why = "journal could not be judged"
			case "journal malformed":
				writeFile(t, retirement.JournalPath(), "not JSON", 0o600)
				why = "journal could not be judged"
			case "claim unknown":
				mustOK(t, os.Mkdir(f.guard.root, 0o700))
				mustOK(t, syscall.Mkfifo(f.guard.active(), 0o600))
				why = "claim is unknown"
			case "claim unpublished":
				mustOK(t, os.MkdirAll(f.guard.active(), 0o700))
				why = "claim is unpublished-guard"
			case "guard record":
				mustHold(t, "ci-1")
				writeFile(t, filepath.Join(f.guard.active(), guardRecordName), "not JSON", 0o600)
				why = "guard's record could not be read"
			case "configuration":
				mustOK(t, os.Remove(f.cfg))
				mustOK(t, os.Mkdir(f.cfg, 0o700))
				why = "configuration could not be read"
			}
			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a", "--requested")
			m := assertRetireRoute(t, out, code, "hold", why)
			if broken != "configuration" && (m["identity"] != "minted" || m["authority"] != "absent") {
				t.Fatalf("a failed local observation hid the identity facts: %s", out)
			}
		})
	}
}

// CLASSIFIER OPERANDS BELONG TO DRY RUN ALONE. A combination refusal must
// precede the guard, so no attempted reservation acquires anything first.
func TestTheRetireClassifierFlagsAreRefusedOutsideDryRun(t *testing.T) {
	f := newRetireFixture(t)
	for _, operand := range [][]string{{"--requested"}, {"--expected-holder", "ci-1"}, {"--expected-guard", retireTestID}} {
		args := append([]string{"--reserve", "--run", "ci-1", "--retiring-host", "control-a", "--survivor-host", "control-b"}, operand...)
		out, code := f.run(t, "", args...)
		m := retireAnswer(t, out)
		if code != exitRefused || m["reason"] != retireReasonCombination || m["outcome"] != retireOutcomeRefused {
			t.Fatalf("a classifier operand reached a mutating mode: %s", out)
		}
	}
	if _, err := os.Lstat(f.guard.root); !os.IsNotExist(err) {
		t.Fatalf("a classifier operand created the upgrade root: %v", err)
	}
}

// A FRESH HOST IS OBSERVED, NEVER COMMISSIONED BY THE OBSERVATION. No open
// may create its identity, authority, ledger or locks; no service is asked
// to change the facts that the classifier is meant to report.
func TestTheNewRetireObservationsCreateAndAcquireNothing(t *testing.T) {
	f := newRetireFixture(t)
	missing := filepath.Join(t.TempDir(), "not-created")
	f.cfg = writeRetirePostgresConfig(t, missing)
	savedOpen, savedConverge := retireReportOpen, converge
	t.Cleanup(func() { retireReportOpen, converge = savedOpen, savedConverge })
	retireReportOpen = func(context.Context, *config.Config, string) (*state.DB, error) {
		t.Fatal("an uncommissioned classifier attempted a ledger open")
		return nil, errors.New("unexpected ledger open")
	}
	converge = func(...lifeops.ConvergeOption) converger {
		t.Fatal("the classifier touched a service")
		return &fakeConverger{}
	}

	// AND AN UNCOMMISSIONED HOST IS NOT A HOST TO RETIRE. Without a request
	// it converges ordinarily; with one it is refused by name, because there
	// is no deployment here — no identity, so no row a reservation could
	// name — and routing it to one would fail for a reason the operator would
	// have to work backwards from.
	for _, extra := range [][]string{nil, {"--requested"}} {
		args := append([]string{"--dry-run", "--retiring-host", "control-a"}, extra...)
		want, why := "ordinary", ""

		if len(extra) != 0 {
			want, why = "hold", "has not been commissioned"
		}

		out, code := f.run(t, "", args...)
		m := assertRetireRoute(t, out, code, want, why)
		if m["identity"] != "absent" || m["authority"] != "absent" || m["row_fact"] != string(retirement.RowUnreadable) {
			t.Fatalf("the missing directory did not establish the case: %s", out)
		}
	}
	for _, path := range []string{missing, retirement.InitLockPath(missing), retirement.GlobalLockPath(),
		retirement.RetiredDir(), f.guard.root} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the classifier created %s: %v", path, err)
		}
	}
}

// UNCOMMISSIONED DOES NOT MEAN ELIGIBLE FOR RETIREMENT. The installed
// backend, controller mode and node custody each still qualify the request.
func TestTheDryRunQualifiesAnUncommissionedRequest(t *testing.T) {
	for _, clause := range []string{"ledger is sqlite", "controllers are single", "installed_roles both"} {
		t.Run(clause, func(t *testing.T) {
			f := newRetireFixture(t)
			mustOK(t, os.Remove(state.DeploymentIDPath(f.stateDir)))
			roles := "server"
			switch clause {
			case "controllers are single":
				f.cfg = writePostgresConfig(t, f.stateDir)
			case "installed_roles both":
				f.cfg = writeRetirePostgresConfig(t, f.stateDir)
				body := mustRead(t, f.cfg) + "node:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n" +
					"  provider: docker\n  state_dir: " + filepath.Join(t.TempDir(), "node") + "\n"
				writeFile(t, f.cfg, body, 0o600)
				roles = "both"
			}
			for _, requested := range []bool{false, true} {
				args := []string{"--dry-run", "--retiring-host", "control-a"}
				want := "ordinary"
				if requested {
					args = append(args, "--requested")
					want = "hold"
				}
				out, code := f.run(t, "", args...)
				m := assertRetireRoute(t, out, code, want, clause)
				if m["identity"] != "absent" || m["authority"] != "absent" || m["installed_roles"] != roles {
					t.Fatalf("the uncommissioned host did not establish the clause: %s", out)
				}
			}
		})
	}
}

// ALREADY HELD LOCKS CANNOT OBSTRUCT AN OBSERVATION. The identity and
// authority facts must remain readable while every writer's exclusion is
// held elsewhere, and the ledger handle the classifier uses forbids writes.
func TestTheDryRunObservesTheIdentityUnderHeldWriterLocks(t *testing.T) {
	f := newRetireFixture(t)
	root, err := takeTxLock()
	mustOK(t, err)
	t.Cleanup(root.release)

	global, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true})
	mustOK(t, err)
	t.Cleanup(func() { mustOK(t, global.Release()) })

	for _, path := range []string{wirecert.AuthorityLockPath(f.stateDir), state.DirectoryLockPath(f.stateDir)} {
		lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		mustOK(t, err)
		mustOK(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
		t.Cleanup(func() { mustOK(t, lock.Close()) })
	}

	savedWait, savedOpen, savedConverge := identityAccessWait, retireReportOpen, converge
	identityAccessWait = time.Nanosecond
	t.Cleanup(func() { identityAccessWait, retireReportOpen, converge = savedWait, savedOpen, savedConverge })
	opens := 0
	retireReportOpen = func(ctx context.Context, cfg *config.Config, dsn string) (*state.DB, error) {
		opens++
		db, err := savedOpen(ctx, cfg, dsn)
		mustOK(t, err)
		if _, err := db.ClaimController(ctx, "a-classifier-cannot-claim", f.identity); !errors.Is(err, state.ErrInspect) {
			mustOK(t, db.Close())
			t.Fatalf("the classifier's handle permits a claim or migration: %v", err)
		}
		return db, nil
	}
	converge = func(...lifeops.ConvergeOption) converger {
		t.Fatal("the classifier touched a service under the writer's locks")
		return &fakeConverger{}
	}

	out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
	m := assertRetireRoute(t, out, code, "ordinary", "")
	if opens != 1 || m["identity"] != "minted" || m["authority"] != "absent" || m["row_fact"] != string(retirement.RowAbsent) {
		t.Fatalf("the held locks obstructed the observations (%d opens): %s", opens, out)
	}
}

// assertRetireRoute holds the JSON the caller consumes, including the reason
// clause that distinguishes this decision from an unrelated hold.
func assertRetireRoute(t *testing.T, out string, code int, route, clause string) map[string]any {
	t.Helper()

	m := retireAnswer(t, out)
	if code != 0 || m["outcome"] != retireOutcomeReported || m["route"] != route {
		t.Fatalf("the classifier answered route %v at exit %d, want %s: %s", m["route"], code, route, out)
	}
	if route == "ordinary" {
		if _, present := m["route_why"]; present {
			t.Fatalf("an ordinary route carried a reason: %s", out)
		}
	} else if why, ok := m["route_why"].(string); !ok || why == "" || !strings.Contains(why, clause) {
		t.Fatalf("the route did not name %q: %s", clause, out)
	}

	return m
}
