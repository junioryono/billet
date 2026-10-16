package hostupgrade

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// A ROLLBACK MOVES THE SHAPER TOO. The preserved binary goes back as a new
// file, and by then the shaper may run the candidate (it restarts itself onto a
// replaced binary), so a rollback that left it would leave the host shaped by a
// release it is not on. Last, after the restored services are proved.
func TestARollbackRestartsTheUplinkShaperOntoTheRestoredBinary(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe"}

	if err := runUnwinding(t, j, h); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed probe returned %v, want ErrRolledBack", err)
	}

	if last := h.did[len(h.did)-1]; last != "restart-uplink-rollback" {
		t.Fatalf("the rollback ended with %q, want the shaper's restart last:\n  %v", last, h.did)
	}

	if slices.Index(h.did, "restart-uplink-rollback") < slices.Index(h.did, "restore-preserved") {
		t.Fatalf("the shaper was restarted before the preserved binary was back:\n  %v", h.did)
	}
}

// A RESUMED ROLLBACK MOVES THE SHAPER TOO: the crash it resumes from may have
// come before the restart, and the resume is the only run left to do it.
func TestResumingARolledBackUpgradeRestartsTheShaper(t *testing.T) {
	j := newJournal(t)
	j.Failure = "the candidate never became ready"

	if err := j.Advance(StepRolledBack); err != nil {
		t.Fatalf("advance: %v", err)
	}

	h := &fakeHost{rollingBack: true}

	if err := run(t, j, h); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("resuming a rolled-back upgrade returned %v, want ErrRolledBack", err)
	}

	if last := h.did[len(h.did)-1]; last != "restart-uplink-rollback" {
		t.Fatalf("the resumed rollback ended with %q, want the shaper's restart:\n  %v", last, h.did)
	}
}

// A FAILED RESTART OF THE SHAPER IS REPORTED AND DECIDES NOTHING. Failing a
// committed upgrade over it, or rolling it back, would undo a good host for a
// side process; saying nothing would leave the shaper on the old release with
// no trace.
func TestAFailedUplinkRestartNeitherFailsNorRollsBackTheUpgrade(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "restart-uplink"}

	var logged bytes.Buffer

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil {
		t.Fatalf("an upgrade whose only failure was the shaper's restart returned %v", err)
	}

	if j.Step != StepCommitted {
		t.Fatalf("the journal records %s, want committed", j.Step)
	}

	for _, forbidden := range []string{"restore-ledger", "restore-preserved", "start-services-rollback"} {
		if slices.Contains(h.did, forbidden) {
			t.Fatalf("a failed shaper restart unwound the upgrade (%s):\n  %v", forbidden, h.did)
		}
	}

	for _, want := range []string{"level=WARN", "uplink shaper could not be restarted",
		"the restart-uplink step failed", "systemctl restart billet-uplink.service"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("the failed restart was not reported with %q:\n%s", want, logged.String())
		}
	}
}

// panickingHost's shaper restart panics.
type panickingHost struct{ *fakeHost }

func (h *panickingHost) RestartUplink(context.Context) error {
	h.did = append(h.did, "restart-uplink")

	panic("the shaper's restart panicked")
}

// A PANIC IN THE SHAPER'S RESTART DECIDES NOTHING EITHER: escaping, it would
// end the run after the commit and before the caller releases the claim.
func TestAPanickingUplinkRestartLeavesTheUpgradeCommitted(t *testing.T) {
	j := newJournal(t)
	h := &panickingHost{fakeHost: &fakeHost{}}

	var logged bytes.Buffer

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: slog.New(slog.NewTextHandler(&logged, nil))})
	if err != nil || j.Step != StepCommitted {
		t.Fatalf("an upgrade whose shaper restart panicked returned %v at %s; want committed", err, j.Step)
	}

	if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "panicked") {
		t.Fatalf("the panic was not reported:\n%s", logged.String())
	}
}

// AND ON A ROLLBACK, a failed restart leaves a rolled-back host rolled back
// rather than cordoned.
func TestAFailedUplinkRestartDoesNotCordonARollback(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe", failRollbackAt: "restart-uplink-rollback"}

	err := runUnwinding(t, j, h)
	if !errors.Is(err, ErrRolledBack) || errors.Is(err, ErrCordoned) {
		t.Fatalf("a rollback whose only failure was the shaper's restart returned %v, want ErrRolledBack", err)
	}

	if !slices.Contains(h.did, "restart-uplink-rollback") {
		t.Fatalf("the rollback did not try the shaper's restart:\n  %v", h.did)
	}
}

// THE CANDIDATE IS INSTALLED WHETHER OR NOT THE SERVICES COME UP, so a
// committed upgrade whose services failed still moves the shaper onto it.
func TestACommittedUpgradeWhoseServicesFailStillRestartsTheShaper(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "start-services"}

	if err := run(t, j, h); err == nil || errors.Is(err, ErrRolledBack) {
		t.Fatalf("services that did not start after the commit returned %v, want a committed failure", err)
	}

	if last := h.did[len(h.did)-1]; last != "restart-uplink" {
		t.Fatalf("the committed upgrade ended with %q, want the shaper's restart:\n  %v", last, h.did)
	}
}
