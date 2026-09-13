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
// ask about; identity and authority observations decide bootstrap admission.
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

			// A MINTED IDENTITY WITHHOLDS BOOTSTRAP ADMISSION, including
			// when no usable configuration remains to name its ledger.
			if c.want != "present" {
				dir := filepath.Join(retirement.Root, "server")
				mustOK(t, os.MkdirAll(dir, 0o700))
				writeFile(t, state.DeploymentIDPath(dir), f.identity+"\n", 0o600)
			}

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
			} else if m["installed_roles"] != "" || m["identity"] != "minted" || m["authority"] != "absent" {
				t.Fatalf("an unknown configuration hid the prepared path's identity or invented roles: %s", out)
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

// A LATER FAILED READ OVERRULES AN EARLIER ADMISSION. The row's close is
// after the first local observations and before the final state observation,
// so replacing a record there exercises the second read, not the first.
func TestTheDryRunCarriesAFailedFinalStateObservationIntoTheRoute(t *testing.T) {
	for _, earlier := range []string{"ordinary", "continue", "new-request", "cancel", "unsupported-variant", "hold"} {
		for _, failed := range []string{"journal", "status"} {
			if earlier == "continue" && failed == "status" {
				// A READABLE JOURNAL SUPPLIES THE STATE without a status read.
				continue
			}
			t.Run(earlier+"/"+failed, func(t *testing.T) {
				var f *retireFixture
				rowFact := retirement.RowAbsent
				args := []string{"--dry-run", "--retiring-host", "control-a"}
				why := ""
				if earlier == "new-request" || earlier == "cancel" {
					request := newRequestFixture(t)
					f = request.retireFixture
					if earlier == "cancel" {
						request.reserve(t)
						rowFact, why = retirement.RowReservedMine, "inventory no longer requests"
					} else {
						args = append(args, "--requested")
						why = "eligible"
					}
				} else {
					f = newRetireFixture(t)
				}
				switch earlier {
				case "continue":
					f.journalAt(t, retirement.PhaseIntent, "ci-1")
					why = "readable journal"
				case "unsupported-variant":
					// INSTALLED NODE CUSTODY SELECTS WITHOUT A JOURNAL, so the
					// final observation must judge the status as well as the journal.
					body := mustRead(t, f.cfg) + "\nnode:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n" +
						"  provider: docker\n  state_dir: " + filepath.Join(t.TempDir(), "node") + "\n"
					writeFile(t, f.cfg, body, 0o600)
					args = append(args, "--requested")
					why = "keeps a node"
				case "hold":
					args = append(args, "--requested")
					why = "PostgreSQL active-passive"
				}
				out, code := f.run(t, "", args...)
				before := assertRetireRoute(t, out, code, earlier, why)
				wantState := stateNothingRetire
				if earlier == "continue" {
					wantState = string(retirement.PhaseIntent)
				}
				if before["state"] != wantState || before["row_fact"] != string(rowFact) {
					t.Fatalf("the earlier observations did not establish the admission: %s", out)
				}

				saved := retireReportClose
				t.Cleanup(func() { retireReportClose = saved })
				closes := 0
				retireReportClose = func(db *state.DB) error {
					closes++
					mustOK(t, saved(db))
					path := retirement.StatusPath()
					if failed == "journal" {
						path = retirement.JournalPath()
						if earlier == "continue" {
							mustOK(t, os.Remove(path))
						}
					}
					mustOK(t, os.MkdirAll(path, 0o700))

					return nil
				}

				out, code = f.run(t, "", args...)
				if earlier != "hold" {
					why = "the host's own state could not be established when the report was made"
				}
				m := assertRetireRoute(t, out, code, "hold", why)
				if closes != 1 || m["state"] != retireStateUnknown || m["row_fact"] != string(rowFact) ||
					m["status_presence"] != "absent" || m["stage"] != "absent" {
					t.Fatalf("the failure did not follow successful observations (%d closes): %s", closes, out)
				}
				if earlier == "continue" {
					if asMap(m["journal"])["phase"] != string(retirement.PhaseIntent) {
						t.Fatalf("the earlier journal read did not succeed: %s", out)
					}
				} else if m["journal"] != nil {
					t.Fatalf("the earlier journal read did not establish absence: %s", out)
				}
				if earlier == "hold" && m["route_why"] != before["route_why"] {
					t.Fatalf("the final read replaced an existing hold's reason: %s", out)
				}
			})
		}
	}
}

// UNRECOGNISED OBSERVATIONS HOLD BY VALUE. A new row fact or local presence
// must not fall through to ordinary, and zero values supply no admission.
func TestTheRetireRouteHoldsEveryUnrecognisedCombination(t *testing.T) {
	for name, report := range map[string]retireReport{
		"zero":           {},
		"unknown row":    {StatusPresence: "absent", Stage: "absent", RowFact: retirement.RowFact("future")},
		"zero row":       {StatusPresence: "absent", Stage: "absent"},
		"unknown status": {StatusPresence: "future", Stage: "absent", RowFact: retirement.RowAbsent},
		"unknown stage":  {StatusPresence: "absent", Stage: "future", RowFact: retirement.RowAbsent},
	} {
		t.Run(name, func(t *testing.T) {
			for _, requested := range []bool{false, true} {
				route, why := retireRoute(&report, nil, requested)
				if route != "hold" || why != "this combination of records is one this billet does not recognise, and an "+
					"unrecognised host is not one to converge over" {
					t.Fatalf("an unrecognised combination answered %q, %q", route, why)
				}
			}
		})
	}
}

// A FUTURE ROW STATE IS NOT A COMPLETED RETIREMENT. The snapshot seam
// supplies that state under a valid binding so both controllers must hold.
func TestTheDryRunHoldsAnUnrecognisedRowState(t *testing.T) {
	f := newRetireFixture(t)
	saved := retireReportSnapshot
	t.Cleanup(func() { retireReportSnapshot = saved })
	reads := 0
	retireReportSnapshot = func(ctx context.Context, db *state.DB) (rollout.StatusSnapshot, error) {
		reads++
		snapshot, err := saved(ctx, db)
		mustOK(t, err)
		if snapshot.Binding != f.identity || snapshot.Retirement != nil {
			t.Fatalf("the snapshot did not establish the bound ledger: %+v", snapshot)
		}
		snapshot.Retirement = &state.Retirement{Deployment: f.identity, Retiring: "control-a", State: "future"}

		return snapshot, nil
	}
	for _, host := range []string{"control-a", "control-b"} {
		for _, extra := range [][]string{nil, {"--requested"}} {
			args := append([]string{"--dry-run", "--retiring-host", host}, extra...)
			out, code := f.run(t, "", args...)
			m := assertRetireRoute(t, out, code, "hold", `the retirement row's state "future" is not recognised by this billet`)
			if m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil || m["identity"] != "minted" ||
				m["state"] != stateNothingRetire {
				t.Fatalf("the unknown row state did not reach the route: %s", out)
			}
		}
	}
	if reads != 4 {
		t.Fatalf("the classifier read %d snapshots, want four", reads)
	}
}

// A LOCATOR READ MUST NOT CREATE ITS ARCHIVE. The tail's own opener takes
// the directory lock at the archive and creates it if it is not there, which
// belongs to a run about to write the deployment's row. At intent the archive
// does not exist yet; a classifier using that open would create the destination
// the transition is about to rename onto.
func TestTheDryRunCreatesNoArchiveThroughTheLocator(t *testing.T) {
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

// THE ORDINARY PATH CREATES NO UPGRADE ROOT OR CONFIGURATION. A SQLite read
// may leave driver sidecars beside an existing ledger; this case names only
// the paths the classifier itself must leave absent.
func TestTheDryRunCreatesNoUpgradeRootOrConfiguration(t *testing.T) {
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
	assertRetireRoute(t, out, code, "hold", "the authority status could not be judged:")
	if m["status_presence"] != "unreadable" {
		t.Fatalf("an unreadable status was not reported: %s", out)
	}

	why, ok := m["status_why"].(string)
	if !ok || !strings.Contains(why, "not a regular file") {
		t.Fatalf("the failed read's diagnostic was lost: %s", out)
	}
}

// A STATUS LINK IS AN UNREADABLE ARTEFACT, whether its target is absent or
// a valid publication. Neither target grants ordinary converge, a new request
// or cancellation over the unexplained name.
func TestTheDryRunHoldsOnStatusLinksWithNoJournal(t *testing.T) {
	for _, target := range []string{"dangling", "live"} {
		for _, row := range []string{"absent", "reserved"} {
			t.Run(target+"/"+row, func(t *testing.T) {
				f := newRequestFixture(t)
				rowFact := retirement.RowAbsent
				if row == "reserved" {
					f.reserve(t)
					rowFact = retirement.RowReservedMine
				}
				for _, extra := range [][]string{nil, {"--requested"}} {
					args := append([]string{"--dry-run", "--retiring-host", requestRetiring}, extra...)
					want, why := "ordinary", ""
					if row == "reserved" {
						want, why = "cancel", "inventory no longer requests"
					}
					if len(extra) != 0 {
						want, why = "new-request", "eligible"
					}
					out, code := f.run(t, "", args...)
					assertRetireRoute(t, out, code, want, why)
				}

				path := filepath.Join(retirement.Root, "status-target")
				if target == "live" {
					mustOK(t, retirement.WriteStatus(retirement.PhaseDone, retirement.VariantServerOnly, retireNow()))
					st, presence, err := retirement.ReadStatus()
					mustOK(t, err)
					if presence != retirement.StatusPresent || st.Phase != retirement.PhaseDone {
						t.Fatalf("the link's target was not a valid status: %+v, presence %d", st, presence)
					}
					mustOK(t, os.Rename(retirement.StatusPath(), path))
				} else if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("the dangling link's target was not absent: %v", err)
				}
				mustOK(t, os.Symlink(path, retirement.StatusPath()))
				_, presence, err := retirement.ReadStatus()
				if presence != retirement.StatusUnreadable || err == nil || os.IsNotExist(err) {
					t.Fatalf("a status link was followed or called absent: presence %d, error %v", presence, err)
				}

				for _, extra := range [][]string{nil, {"--requested"}} {
					args := append([]string{"--dry-run", "--retiring-host", requestRetiring}, extra...)
					out, code := f.run(t, "", args...)
					m := assertRetireRoute(t, out, code, "hold", "the authority status could not be judged:")
					if m["status_presence"] != "unreadable" || m["status"] != nil || m["journal"] != nil ||
						m["row_fact"] != string(rowFact) || m["state"] != retireStateUnknown {
						t.Fatalf("the status link did not establish an unexplained artefact: %s", out)
					}
				}
			})
		}
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
			entered, expire := pinRetireObservationDeadline(t)
			savedEUID, savedOwner, savedReexec := statusEUID, statusOwnerOf, retireReexecCapture
			t.Cleanup(func() { statusEUID, statusOwnerOf, retireReexecCapture = savedEUID, savedOwner, savedReexec })

			statusEUID = func() int { return 0 }
			statusOwnerOf = func(string) (uint32, uint32, error) { return 990, 991, nil }
			calls := 0

			retireReexecCapture = func(ctx context.Context, uid, gid uint32, args []string) ([]byte, int, error) {
				calls++
				entered(ctx)
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

		if !errors.Is(bounded.Err(), context.DeadlineExceeded) {
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
		{"reserved", "control-a", retirement.RowReservedMine, "hold", "hold", "PostgreSQL active-passive"},
		{"intent", "control-a", retirement.RowIntentMine, "hold", "hold", "past its reservation"},
		{"done", "control-a", retirement.RowDoneMine, "hold", "hold", "past its reservation"},
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
// with it. Installed node custody selects the unsupported request variant.
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
			want, why = "unsupported-variant", "keeps a node"
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

// NODE CUSTODY NAMES THE UNSUPPORTED VARIANT BEFORE PAIR ELIGIBILITY. A
// fresh request on either backend must not disguise a retained node as a
// backend or controller-mode refusal.
func TestTheDryRunNamesAFreshRetainedNodeRequestBeforePairEligibility(t *testing.T) {
	for _, pair := range []string{"sqlite", "postgres single", "postgres active-passive"} {
		t.Run(pair, func(t *testing.T) {
			var f *retireFixture
			if pair == "sqlite" {
				f = newRetireFixture(t)
			} else {
				f = newRequestFixture(t).retireFixture
				if pair == "postgres single" {
					f.cfg = writePostgresConfig(t, f.stateDir)
				}
			}
			body := mustRead(t, f.cfg) + "\nnode:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n" +
				"  provider: docker\n  state_dir: " + filepath.Join(t.TempDir(), "node") + "\n"
			writeFile(t, f.cfg, body, 0o600)

			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a", "--requested")
			m := assertRetireRoute(t, out, code, "unsupported-variant", "does not support retiring a host that keeps a node")
			if m["config"] != "present" || m["installed_roles"] != "both" || m["row_fact"] != string(retirement.RowAbsent) {
				t.Fatalf("the installed host did not establish a fresh retained-node request: %s", out)
			}
		})
	}
}

// CANCELLATION MUST ADOPT THE RESERVATION FIRST. A single controller's
// readable reservation is no permission to send the role to that refusal.
func TestTheDryRunHoldsCancellationUnderASingleControllerConfiguration(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)
	f.cfg = writePostgresConfig(t, f.stateDir)

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring)
	m := assertRetireRoute(t, out, code, "hold", "reservation cannot be adopted under this configuration")
	assertRetireRoute(t, out, code, "hold", "controllers are single")
	if m["config"] != "present" || m["installed_roles"] != "server" || m["row_fact"] != string(retirement.RowReservedMine) {
		t.Fatalf("the configuration hid the readable reservation: %s", out)
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
			want, why = "hold", "has a node and no server"
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

// AUTHORITY REMNANTS ESTABLISH DAMAGE beside an absent identity. Only two
// positive absences admit bootstrap; unreadable metadata supplies neither.
func TestTheDryRunJudgesControllersWhoseIdentityCannotNameTheRow(t *testing.T) {
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
						want, why = "hold", "no controller to retire"
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
// prerequisite. Each supplied holder or prepared id is compared independently;
// omitting both expectations permits a preview with no guard.
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
				for _, expected := range []string{"neither", "holder", "id", "both"} {
					args := []string{"--dry-run", "--retiring-host", "control-a"}
					want, why := "ordinary", ""
					if journal {
						want, why = "continue", "readable journal"
					}
					holderExpected := expected == "holder" || expected == "both"
					idExpected := expected == "id" || expected == "both"
					if holderExpected {
						args = append(args, "--expected-holder", "ci-1")
					}
					if idExpected {
						args = append(args, "--expected-guard", retireTestID)
					}
					switch {
					case guard == "gone" && expected != "neither":
						want, why = "hold", "guard is gone"
					case guard == "other holder" && holderExpected:
						want, why = "hold", "holder ci-other"
					case guard == "other id" && idExpected:
						want, why = "hold", "guard id differs"
					}
					out, code := f.run(t, "", args...)
					assertRetireRoute(t, out, code, want, why)
				}
			}
		})
	}
}

// AN OBSERVED GUARD MUST BE READY FOR A MUTATING ROUTE. The acquirer's cleanup
// window and an interrupted rewrite each hold a retirement whenever observed,
// while ordinary tasks remain reachable. The observation must neither settle
// nor repair the record, with or without continuity flags.
func TestTheDryRunRequiresAMutationReadyGuardOnlyForRetirement(t *testing.T) {
	for _, route := range []string{"ordinary", "new-request", "continue", "cancel", "unsupported-variant"} {
		for _, damage := range []string{"preparing", "interrupted rewrite"} {
			t.Run(route+"/"+damage, func(t *testing.T) {
				var f *retireFixture
				rowFact, why := retirement.RowAbsent, ""
				args := []string{"--dry-run", "--retiring-host", "control-a"}
				if route == "new-request" || route == "cancel" {
					request := newRequestFixture(t)
					f = request.retireFixture
					if route == "cancel" {
						request.reserve(t)
						rowFact, why = retirement.RowReservedMine, "inventory no longer requests"
					} else {
						args = append(args, "--requested")
						why = "eligible"
					}
				} else {
					f = newRetireFixture(t)
					mustHold(t, "ci-1")
					if route == "continue" {
						f.journalAt(t, retirement.PhaseIntent, "ci-1")
						why = "readable journal"
					}
					if route == "unsupported-variant" {
						body := mustRead(t, f.cfg) + "\nnode:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n" +
							"  provider: docker\n  state_dir: " + filepath.Join(t.TempDir(), "node") + "\n"
						writeFile(t, f.cfg, body, 0o600)
						args = append(args, "--requested")
						why = "keeps a node"
					}
				}
				rec := f.guard.record(t)
				rec.ID = retireTestID
				writeGuardRecordForTest(t, f.guard, rec)
				heldArgs := append(append([]string{}, args...), "--expected-holder", "ci-1", "--expected-guard", retireTestID)
				out, code := f.run(t, "", heldArgs...)
				m := assertRetireRoute(t, out, code, route, why)
				if m["row_fact"] != string(rowFact) || rec.Preparing {
					t.Fatalf("the undamaged guard's host did not establish the route: %s", out)
				}

				temporary := filepath.Join(f.guard.active(), guardTmpName)
				if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
					t.Fatalf("the undamaged guard already carries a temporary: %v", err)
				}
				if damage == "preparing" {
					rec.Preparing, rec.Token = true, strings.Repeat("a", 32)
					writeGuardRecordForTest(t, f.guard, rec)
				} else {
					writeFile(t, temporary, "interrupted guard record", 0o600)
				}
				recordPath := filepath.Join(f.guard.active(), guardRecordName)
				before := mustRead(t, recordPath)
				if planted := f.guard.record(t); planted.Preparing != (damage == "preparing") {
					t.Fatalf("the record did not establish the preparing fact: %t", planted.Preparing)
				}

				want, clause := "hold", damage
				if route == "ordinary" || route == "unsupported-variant" {
					want, clause = route, why
				}
				for _, operands := range [][]string{args, heldArgs} {
					out, code = f.run(t, "", operands...)
					assertRetireRoute(t, out, code, want, clause)
				}
				if after := mustRead(t, recordPath); after != before {
					t.Fatalf("the classifier settled or rewrote the guard: %s", after)
				}
				if damage == "interrupted rewrite" && mustRead(t, temporary) != "interrupted guard record" {
					t.Fatal("the classifier changed the interrupted rewrite")
				}
			})
		}
	}
}

// AN OBSERVED BINARY POINTER EXCLUDES EVERY ROUTE BUT ORDINARY. Recovery
// still reaches the tasks that own that pointer, with or without continuity
// flags; omitting an expectation cannot hide the pointer in front of us.
func TestTheDryRunKeepsBinaryRecoveryOnTheOrdinaryRoute(t *testing.T) {
	for _, route := range []string{"ordinary", "cancel", "continue", "unsupported-variant"} {
		t.Run(route, func(t *testing.T) {
			var f *retireFixture
			if route == "cancel" {
				request := newRequestFixture(t)
				request.reserve(t)
				f = request.retireFixture
			} else {
				f = newRetireFixture(t)
				mustHold(t, "ci-1")
			}
			switch route {
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
			out, code := f.run(t, "", "--dry-run", "--retiring-host", "control-a")
			assertRetireRoute(t, out, code, route, "")

			pointer := filepath.Join(f.guard.active(), guardPointerName)
			recovery := filepath.Join(f.guard.root, "recovery-20260911T100000-0badcafe")
			mustOK(t, os.Mkdir(recovery, 0o700))
			mustOK(t, os.Symlink(recovery, pointer))
			for _, expected := range []bool{false, true} {
				args := []string{"--dry-run", "--retiring-host", "control-a"}
				want, why := route, ""
				if route != "ordinary" {
					want, why = "hold", "binary transaction's pointer"
				}
				if expected {
					args = append(args, "--expected-holder", "ci-1")
				}
				out, code := f.run(t, "", args...)
				assertRetireRoute(t, out, code, want, why)
			}
		})
	}
}

// AND THE SAME RULE ON A NEW REQUEST: a readable row and an eligible pair
// cannot admit retirement under a binary transaction's pointer.
func TestTheDryRunKeepsABinaryPointerOffANewRequest(t *testing.T) {
	f := newRequestFixture(t)

	out, code := f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested")
	assertRetireRoute(t, out, code, "new-request", "eligible")

	pointer := filepath.Join(f.guard.active(), guardPointerName)
	recovery := filepath.Join(f.guard.root, "recovery-20260911T100000-0badcafe")

	mustOK(t, os.Mkdir(recovery, 0o700))
	mustOK(t, os.Symlink(recovery, pointer))

	out, code = f.run(t, "", "--dry-run", "--retiring-host", requestRetiring, "--requested")
	assertRetireRoute(t, out, code, "hold", "binary transaction's pointer")

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

// A FRESH HOST IS OBSERVED, NEVER COMMISSIONED BY THE OBSERVATION. It creates
// no directory, identity, authority or guard, takes no billet lock, claims
// nothing and migrates nothing. This host needs no ledger read; an existing
// SQLite ledger's read may leave the driver's own sidecars beside it.
func TestTheNewRetireObservationsLeaveBootstrapPathsAbsent(t *testing.T) {
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

	// BOOTSTRAP NEEDS NO LEDGER OPEN when identity and authority are absent.
	// A retirement request still holds because there is no controller to retire.
	for _, extra := range [][]string{nil, {"--requested"}} {
		args := append([]string{"--dry-run", "--retiring-host", "control-a"}, extra...)

		out, code := f.run(t, "", args...)
		want, why := "ordinary", ""
		if len(extra) != 0 {
			want, why = "hold", "no controller to retire"
		}
		m := assertRetireRoute(t, out, code, want, why)
		if m["config"] != "present" || m["installed_roles"] != "server" || m["identity"] != "absent" ||
			m["authority"] != "absent" || m["row_fact"] != string(retirement.RowUnreadable) {
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

// THE ACCEPTED RESIDUAL LEAVES A LIVE RESERVATION UNEXAMINED. Losing only
// deployment-id beside no authority has the bootstrap facts, so ordinary
// converge proceeds while the independent ledger read still finds the row.
func TestTheDryRunAcceptsAReservationWhoseOnlyIdentityFileWasLost(t *testing.T) {
	f := newRequestFixture(t)
	mustOK(t, os.RemoveAll(wirecert.CADir(f.stateDir)))
	mustOK(t, os.RemoveAll(wirecert.AuthorityMarkerPath(f.stateDir)))
	row := f.reserve(t)

	for _, path := range []string{wirecert.CADir(f.stateDir), wirecert.AuthorityMarkerPath(f.stateDir),
		retirement.JournalPath(), retirement.StatusPath(), retirement.StagePath()} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("the reservation left a local artefact at %s: %v", path, err)
		}
	}
	if rec := f.guard.record(t); rec.Transition != nil {
		t.Fatalf("the reservation published a guard marker: %+v", rec.Transition)
	}
	mustOK(t, os.Remove(state.DeploymentIDPath(f.stateDir)))

	for _, extra := range [][]string{nil, {"--requested"}} {
		args := append([]string{"--dry-run", "--retiring-host", requestRetiring}, extra...)
		out, code := f.run(t, "", args...)
		want, why := "ordinary", ""
		if len(extra) != 0 {
			want, why = "hold", "no controller to retire"
		}
		m := assertRetireRoute(t, out, code, want, why)
		if m["config"] != "present" || m["installed_roles"] != "server" || m["identity"] != "absent" ||
			m["authority"] != "absent" || m["row_fact"] != string(retirement.RowUnreadable) {
			t.Fatalf("the lost identity did not establish the case: %s", out)
		}
	}

	// THE LEDGER STILL HOLDS IT. This independent read uses the recorded
	// deployment, not the lost file the classifier would need to name it.
	db, err := state.OpenPostgresInspect(t.Context(), f.stateDir, f.dsn)
	mustOK(t, err)
	kept, present, readErr := db.ReadRetirement(t.Context(), f.identity)
	mustOK(t, db.Close())
	mustOK(t, readErr)
	if !present || kept.State != state.RetirementReserved || kept.TransitionID != row.TransitionID || kept.Run != requestRun {
		t.Fatalf("the reservation did not survive the lost identity: present %t, row %+v", present, kept)
	}
}

// PACKAGE PREPARATION CREATES A DIRECTORY BEFORE THE CONFIGURATION IS VALID.
// Admission rests on identity and authority observations, not that directory's
// existence; a minted identity may still name an unreadable reservation.
func TestTheDryRunJudgesPackagePreparationByIdentityAndAuthority(t *testing.T) {
	// NO INHERITED DSN MAY TURN THIS INTO A LIVE LEDGER READ. The installed
	// configuration's minted-identity case has an explicitly unreadable row.
	t.Setenv("BILLET_STATE_DSN", "")

	for _, configuration := range []string{"absent", "malformed", "present"} {
		for _, evidence := range []string{"absent", "directory", "identity", "ca", "marker", "dangling", "fifo", "unreadable"} {
			t.Run(configuration+"/"+evidence, func(t *testing.T) {
				f := newRetireFixture(t)
				dir := filepath.Join(retirement.Root, "server")
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Fatalf("the prepared location is not initially absent: %v", err)
				}

				switch configuration {
				case "absent":
					mustOK(t, os.Remove(f.cfg))
				case "malformed":
					// THE ACTUAL PACKAGE SEED IS VALID YAML BUT INVALID CONFIG:
					// zero capacity and placeholder App facts await a converge.
					writeFile(t, f.cfg, mustRead(t, "../../deploy/billet.yaml"), 0o600)
				case "present":
					f.cfg = writeRetirePostgresConfig(t, dir)
				}
				identity, authority := "absent", "absent"
				switch evidence {
				case "directory", "identity", "ca", "marker", "dangling", "fifo":
					mustOK(t, os.MkdirAll(dir, 0o700))
					switch evidence {
					case "identity":
						writeFile(t, state.DeploymentIDPath(dir), retireTestIdentity+"\n", 0o600)
						identity = "minted"
					case "ca":
						mustOK(t, os.Mkdir(wirecert.CADir(dir), 0o700))
						authority = "present"
					case "marker":
						writeFile(t, wirecert.AuthorityMarkerPath(dir), "authority existed", 0o600)
						authority = "present"
					case "dangling":
						mustOK(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), wirecert.CADir(dir)))
						authority = "present"
					case "fifo":
						mustOK(t, syscall.Mkfifo(wirecert.AuthorityMarkerPath(dir), 0o600))
						authority = "present"
					}
				case "unreadable":
					mustOK(t, os.MkdirAll(filepath.Dir(dir), 0o700))
					mustOK(t, os.Symlink(dir, dir))
					if _, err := os.Lstat(state.DeploymentIDPath(dir)); !errors.Is(err, syscall.ELOOP) {
						t.Fatalf("the identity lookup did not fail on the loop: %v", err)
					}
					identity, authority = "unreadable", "unreadable"
				}

				for _, requested := range []bool{false, true} {
					args := []string{"--dry-run", "--retiring-host", "control-a"}
					want, why := "hold", "row is unreadable"
					if authority == "present" {
						why = "identity is absent beside authority remnants"
					}
					if requested {
						args = append(args, "--requested")
					}
					if evidence == "absent" || evidence == "directory" {
						want, why = "ordinary", ""
						if requested {
							want, why = "hold", "no controller to retire"
						}
					}
					out, code := f.run(t, "", args...)
					m := assertRetireRoute(t, out, code, want, why)
					roles := ""
					if configuration == "present" {
						roles = "server"
					}
					if m["config"] != configuration || m["installed_roles"] != roles || m["identity"] != identity ||
						m["authority"] != authority || m["row_fact"] != string(retirement.RowUnreadable) || m["row"] != nil ||
						m["journal"] != nil || m["status_presence"] != "absent" || m["stage"] != "absent" || m["marker"] != nil ||
						m["state"] != stateNothingRetire {
						t.Fatalf("the identity and authority evidence did not establish the case: %s", out)
					}
				}
				paths := []string{f.guard.root, retirement.GlobalLockPath(), retirement.RetiredDir()}
				if configuration == "absent" {
					paths = append(paths, f.cfg)
				}
				if evidence == "absent" {
					paths = append(paths, dir)
				}
				for _, path := range paths {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatalf("the classifier created %s: %v", path, err)
					}
				}
			})
		}
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
