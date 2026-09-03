package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// A SEALED DEPLOYMENT SAYS SO, WITH ITS ATTRIBUTION. The operator reading this
// is usually not the one who sealed it, and their real question is whether they
// may clear it — which needs to know who took it and of what kind.
func TestStatusReportsWhoSealedTheDeploymentAndWhetherItSurvives(t *testing.T) {
	cases := []struct {
		name  string
		given state.Admission
		want  []string
		wrong []string
	}{
		{
			"open",
			state.Admission{Mode: state.AdmissionOpen},
			[]string{"admission open"},
			[]string{"not taking new work", "sealed by"},
		},
		{
			"an operator's seal",
			state.Admission{
				Mode: state.AdmissionSealed, Provenance: state.ProvenanceOperator,
				Actor: "ops@example.com", Reason: "replacing a disk", ChangedAt: "2026-08-26T06:00:00Z",
			},
			[]string{
				"not taking new work", "ops@example.com", "replacing a disk",
				"2026-08-26T06:00:00Z", "survives a restart",
			},
			[]string{"admission open"},
		},
		{
			"a shutdown's seal",
			state.Admission{
				Mode: state.AdmissionSealed, Provenance: state.ProvenanceLocalDown,
				Actor: "billet local down",
			},
			[]string{"not taking new work", "billet local up` clears it"},
			[]string{"survives a restart"},
		},
		{
			// A mode billet could not read is reported as what it is, not folded
			// into either answer.
			"unreadable",
			state.Admission{Mode: state.AdmissionUnknown},
			[]string{"unknown", "not taking new work"},
			[]string{"admission open"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := capture(t, func() { printAdmission(tc.given) })

			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the report does not mention %q:\n%s", want, out)
				}
			}
			for _, wrong := range tc.wrong {
				if strings.Contains(out, wrong) {
					t.Errorf("the report wrongly says %q:\n%s", wrong, out)
				}
			}
		})
	}
}

// `billet status` REPORTS THE SEAL, AND REPORTS IT FIRST.
//
// Driven through cmdStatus rather than through printAdmission, because the
// printer being correct says nothing about whether status calls it — the whole
// admission block could be deleted from cmdStatus and a printer test would stay
// green. First, because a sealed deployment reads entirely normally on every
// other line, and an operator who has to scroll to learn their fleet is
// deliberately idle has been told too late.
func TestStatusReportsTheSealBeforeAnythingElse(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	db, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	// Open first: the assertions below must not pass against a status command
	// that says "sealed" unconditionally.
	openOut := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfg}); err != nil {
			t.Errorf("status on an open deployment: %v", err)
		}
	})
	if !strings.HasPrefix(strings.TrimSpace(openOut), "admission open") {
		t.Errorf("status does not lead with admission on an open deployment:\n%s", openOut)
	}

	if _, err := db.Seal(ctx, state.SealRequest{
		Provenance: state.ProvenanceOperator,
		Reason:     "replacing a disk",
		Actor:      "ops@example.com",
	}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sealedOut := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfg}); err != nil {
			t.Errorf("status on a sealed deployment: %v", err)
		}
	})

	first := strings.SplitN(strings.TrimSpace(sealedOut), "\n", 2)[0]
	if !strings.Contains(first, "not taking new work") {
		t.Errorf("the first line of status is %q, want the seal:\n%s", first, sealedOut)
	}
	for _, want := range []string{"ops@example.com", "replacing a disk", "survives a restart"} {
		if !strings.Contains(sealedOut, want) {
			t.Errorf("status does not report %q:\n%s", want, sealedOut)
		}
	}
}

// `billet status` SHOWS WHAT EACH HOST SAID, AND SAYS WHAT THAT IS WORTH.
//
// The ledger cannot answer "is anything running on that box" — that is the
// compute barrier's standing limitation — and until now the only thing billet
// could say was to go and look somewhere else, for a fact the control plane had
// already been told. This shows it.
//
// THE SHAPE IS THE DEFENCE, not the wording. A host that says it is running
// something is telling you a fact worth acting on. A host that says it saw
// nothing is telling you about a moment that has already passed: it lists its
// provider and THEN posts, and a launch can be handed to it immediately after.
// So a zero must never read as clearance, must never be aggregated into a
// fleet-wide verdict, and must never touch an exit status.
func TestStatusShowsWhatEachHostReportedWithoutCallingItIdle(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	db, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	busy, err := a.RegisterNode(ctx, alloc.NodeRegistration{
		Name: "busy-host", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
	})
	if err != nil {
		t.Fatalf("register busy: %v", err)
	}

	quiet, err := a.RegisterNode(ctx, alloc.NodeRegistration{
		Name: "quiet-host", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
	})
	if err != nil {
		t.Fatalf("register quiet: %v", err)
	}

	if _, err := a.ResolveQuarantineFor(ctx, "busy-host", []string{"x", "y"}, busy); err != nil {
		t.Fatalf("busy inventory: %v", err)
	}
	if _, err := a.ResolveQuarantineFor(ctx, "quiet-host", nil, quiet); err != nil {
		t.Fatalf("quiet inventory: %v", err)
	}

	// A third host that has said nothing since it came back.
	if _, err := a.RegisterNode(ctx, alloc.NodeRegistration{
		Name: "silent-host", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register silent: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	// PER HOST, so the assertions cannot be satisfied by the right words landing
	// on the wrong machine. Independent Contains checks would pass if every line
	// said the same thing.
	for _, want := range []struct{ host, says string }{
		{"busy-host", "SAYS IT IS RUNNING 2"},
		{"quiet-host", "saw 0 billet instances when it last looked"},
		{"silent-host", "has not reported since it last reconnected"},
	} {
		if !hostLineSays(out, want.host, want.says) {
			t.Errorf("status does not say %q against %s:\n%s", want.says, want.host, out)
		}
	}

	// AND A ZERO IS NEVER RENDERED AS CLEARANCE.
	for _, forbidden := range []string{"quiet-host is idle", "no compute is running", "all hosts idle"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("status renders a snapshot as clearance (%q):\n%s", forbidden, out)
		}
	}

	// THE RECEIPT TIME IS SHOWN, because a snapshot's age is most of what makes
	// it worth anything, and an operator cannot judge staleness without it.
	if !strings.Contains(out, ", received ") {
		t.Errorf("status does not say when the report arrived:\n%s", out)
	}

	// AND IT SAYS WHOSE WORD THIS IS.
	if !strings.Contains(out, "HOST'S OWN") {
		t.Errorf("status does not say the report is the host's own word:\n%s", out)
	}
}

// hostLineSays finds the rendered line for one host and asks what it says, so a
// per-host claim cannot be satisfied by another host's line.
func hostLineSays(out, host, says string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, host) && strings.Contains(line, says) {
			return true
		}
	}

	return false
}

// A HOST THIS DEPLOYMENT CANNOT REACH IS SAID TO BE UNREACHABLE.
//
// Liveness is a different fact from anything in the report, and a stale report
// from a host that is gone reads very differently once you know it is gone.
func TestStatusSaysWhenItCannotReachAHostThatReported(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	db, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	epoch, err := a.RegisterNode(ctx, alloc.NodeRegistration{
		Name: "gone-host", Provider: config.ProviderDocker,
		VCPU: 8, Memory: 32 * config.GiB,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := a.ResolveQuarantineFor(ctx, "gone-host", []string{"z"}, epoch); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if err := a.NodeGone(ctx, "gone-host", epoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	if !hostLineSays(out, "gone-host", "cannot reach it") {
		t.Errorf("status does not say the host is unreachable:\n%s", out)
	}

	// AND IT STILL SHOWS WHAT THAT HOST SAID. A host billet cannot reach is
	// exactly the one whose last word matters most.
	if !hostLineSays(out, "gone-host", "SAYS IT IS RUNNING 1") {
		t.Errorf("status dropped the report of a host it cannot reach:\n%s", out)
	}
}

// TELEMETRY THAT CANNOT BE READ MUST NOT DECIDE THE EXIT STATUS.
//
// This section is one host's own last word; the sections around it are the
// ledger's own answers. An earlier version returned the error from the
// inventory read, so a failure to read telemetry both changed what
// `billet status` exited with and stopped `held` — which IS authoritative —
// from printing at all.
func TestStatusSurvivesTelemetryItCannotRead(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	db, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	// Only the telemetry table is unreadable; every authoritative query still
	// answers.
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TABLE node_inventory`)

		return err
	}); err != nil {
		t.Fatalf("drop node_inventory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfg}); err != nil {
			t.Errorf("status failed because it could not read telemetry: %v", err)
		}
	})

	if !strings.Contains(out, "reported  unavailable") {
		t.Errorf("status does not report that the telemetry could not be read:\n%s", out)
	}

	// AND THE AUTHORITATIVE SECTION STILL PRINTED.
	if !strings.Contains(out, "held") {
		t.Errorf("a telemetry failure displaced the sections that are authoritative:\n%s", out)
	}
}
