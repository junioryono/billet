package main

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

// EVERY PHASE PAST THE ARCHIVE IS RESUMED BY THE COMMAND ITSELF, not only by
// the driver a test can call. The host these leave holds none of what the
// request reads: the identity directory has moved to the archive, and a
// server-only host's installed configuration is gone or about to be, so the
// path that reads both from the installed configuration has nothing to read.
// Each case is an interruption a converge can actually leave, driven through
// the whole command and ending settled.
func TestARetirementPastTheArchiveIsResumedByTheCommand(t *testing.T) {
	cases := map[string]struct {
		phase   retirement.Phase
		variant retirement.Variant
		// prepare runs before the reservation, for a case that needs a node.
		prepare func(t *testing.T, f *requestFixture)
		stage   func(t *testing.T, f *requestFixture, j retirement.Journal)
		request func(t *testing.T, f *requestFixture) (string, int)
	}{
		// THE MOVE COMPLETED BEFORE ITS PHASE COULD BE WRITTEN: the journal
		// still says `stopped` and the identity is already at the archive, so
		// the phase alone would send this host down the path whose exclusion
		// and identity are inside the directory that has moved.
		"the move completed, the phase unwritten": {
			phase: retirement.PhaseStopped,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
			},
			request: serverOnlyResume,
		},
		"archived, the rewrite still to make": {
			phase: retirement.PhaseArchived,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
			},
			request: serverOnlyResume,
		},
		"archived, the rewrite completed before its phase": {
			phase: retirement.PhaseArchived,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				mustOK(t, os.Remove(f.cfg))
			},
			request: serverOnlyResume,
		},
		"the configuration is gone and the phase is written": {
			phase: retirement.PhaseConfigRewritten,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				mustOK(t, os.Remove(f.cfg))
			},
			request: serverOnlyResume,
		},

		// A RETAINED NODE AT `archived` IS THE CASE WITH NO OBSERVATION TO
		// UPDATE: the rewrite installs the staged bytes and the request's
		// configuration observation, which the ordinary path would carry, does
		// not exist on this run.
		"a retained node, archived, the rewrite still to make": {
			phase:   retirement.PhaseArchived,
			variant: retirement.VariantRetainedNode,
			prepare: retainAndRestartANode,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
			},
			request: retainedResume,
		},
		"a retained node, archived, the rewrite completed before its phase": {
			phase:   retirement.PhaseArchived,
			variant: retirement.VariantRetainedNode,
			prepare: retainAndRestartANode,
			stage: func(t *testing.T, f *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Rename(f.stateDir, j.Archive))
				writeFile(t, f.cfg, f.rendering(t), 0o600)
			},
			request: retainedResume,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)

			variant := c.variant
			if variant == "" {
				variant = retirement.VariantServerOnly
			}

			if c.prepare != nil {
				c.prepare(t, f)
			}

			f.reserve(t)

			j := plantResumedRetirement(t, f, c.phase, variant)
			c.stage(t, f, j)

			out, code := c.request(t, f)

			retiredAnswer(t, out, code)

			// THE TRANSITION FINISHED AND THE TAIL WITH IT: the journal is
			// settled and the marker the resume was held to is gone.
			done, presence, err := retirement.ReadJournal()
			mustOK(t, err)

			if presence != retirement.JournalPresent || done.Phase != retirement.PhaseDone || !done.Settled {
				t.Fatalf("the journal this resume left: %+v (presence %d)", done, presence)
			}

			if f.guard.record(t).Transition != nil {
				t.Fatal("the marker was kept over a settled retirement")
			}

			// AND THE HOST HOLDS WHAT THE VARIANT PROMISED.
			body, err := os.ReadFile(f.cfg)

			switch variant {
			case retirement.VariantServerOnly:
				if !os.IsNotExist(err) {
					t.Fatalf("a server-only retirement left a configuration: %v", err)
				}
			default:
				mustOK(t, err)

				if string(body) != f.rendering(t) {
					t.Fatalf("the installed configuration is not the stage byte for byte:\n%s", body)
				}
			}
		})
	}
}

// A RESUME PAST THE ARCHIVE IS STILL HELD TO THIS GUARD AND THIS JOURNAL.
// Nothing about the identity having moved relaxes that: the marker is what
// keeps the guard from being released under a transition that has stopped a
// server and moved an identity, and the archived identity is what says the
// journal describes THIS deployment's retirement.
func TestAResumePastTheArchiveIsHeldToItsGuardAndItsJournal(t *testing.T) {
	cases := map[string]struct {
		break_ func(t *testing.T, f *requestFixture, j retirement.Journal)
		reason string
	}{
		"a guard with no marker": {
			break_: func(t *testing.T, f *requestFixture, _ retirement.Journal) {
				t.Helper()

				markGuard(t, f.guard, nil, nil)
			},
			reason: retireReasonMarker,
		},
		"a marker naming another transition": {
			break_: func(t *testing.T, f *requestFixture, _ retirement.Journal) {
				t.Helper()

				markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement,
					ID: strings.Repeat("c", 32)}, nil)
			},
			reason: retireReasonMarker,
		},
		"a journal naming another transition": {
			break_: func(t *testing.T, _ *requestFixture, _ retirement.Journal) {
				t.Helper()

				j, _, err := retirement.ReadJournal()
				mustOK(t, err)

				j.Provenance.TransitionID = strings.Repeat("b", 32)
				mustOK(t, j.Write(retireNow()))
			},
			reason: retireReasonMarker,
		},
		"a journal owned by a holder this guard never took over from": {
			break_: func(t *testing.T, _ *requestFixture, _ retirement.Journal) {
				t.Helper()

				j, _, err := retirement.ReadJournal()
				mustOK(t, err)

				j.Ownership.Owner = "ci-someone-else"
				mustOK(t, j.Write(retireNow()))
			},
			reason: retireReasonJournal,
		},

		// THE ARCHIVE IS WHERE THE IDENTITY IS READ, so an archive that holds
		// none is a journal this run cannot tie to a deployment at all.
		"an archive holding no identity": {
			break_: func(t *testing.T, _ *requestFixture, j retirement.Journal) {
				t.Helper()

				mustOK(t, os.Remove(state.DeploymentIDPath(j.Archive)))
			},
			reason: retireReasonIdentity,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)

			j := plantResumedRetirement(t, f, retirement.PhaseArchived, retirement.VariantServerOnly)
			mustOK(t, os.Rename(f.stateDir, j.Archive))

			c.break_(t, f, j)

			out, code := serverOnlyResume(t, f)

			m := retireAnswer(t, out)
			if code != exitUnknown || m["reason"] != c.reason {
				t.Fatalf("the resume: %s", out)
			}

			// NOTHING WAS TOUCHED: a refused resume leaves the host where the
			// interruption left it.
			if len(f.svc.trace) != 0 {
				t.Fatalf("a refused resume touched the host: %v", f.svc.trace)
			}

			stood, presence, err := retirement.ReadJournal()
			mustOK(t, err)

			if presence != retirement.JournalPresent || stood.Phase != retirement.PhaseArchived || stood.Settled {
				t.Fatalf("the journal moved under a refused resume: %+v", stood)
			}
		})
	}
}

// plantResumedRetirement leaves the host as a converge interrupted at `phase`
// leaves it: the journal, this guard's marker, the row at intent and the
// published status that closed the authority when the server was stopped.
func plantResumedRetirement(t *testing.T, f *requestFixture, phase retirement.Phase,
	variant retirement.Variant,
) retirement.Journal {
	t.Helper()

	j := f.plantJournal(t, phase, variant)

	markGuard(t, f.guard, &guardTransition{Kind: transitionRetirement, ID: retireTestID}, nil)
	advanceRowToIntent(t, f)

	mustOK(t, retirement.WriteStatus(phase, variant, retireNow()))

	return j
}

// retainAndRestartANode is the retained variant's preparation: the node on the
// host, and the record it publishes when the transition brings it back.
func retainAndRestartANode(t *testing.T, f *requestFixture) {
	t.Helper()

	f.retainANode(t)

	record := useRegistrationRecord(t)
	f.svc.onStart = func(unit string) {
		if unit == nodeUnit {
			restartedNode(t, f, record, retainedEndpoint)
		}
	}
}

// serverOnlyResume runs the request the way a role does on a host whose
// configuration may already be gone: the digest is a placeholder, because
// nothing past the archive compares it and reading the file would fail before
// the command ran.
func serverOnlyResume(t *testing.T, f *requestFixture) (string, int) {
	t.Helper()

	return retiredRequest(t, f, requestRun)
}

func retainedResume(t *testing.T, f *requestFixture) (string, int) {
	t.Helper()

	return f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
}
