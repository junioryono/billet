package host

import (
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/state"
)

// A SERVER'S START IS WAITED FOR AS LONG AS SYSTEMD WAITS FOR IT. The server
// asks for an extension before it opens the ledger, which systemd grants on top
// of up to the unit's whole start bound already spent, so a caller that gave
// up at the plain bound would call a slow, healthy start a failure; the node
// asks for nothing and keeps the plain bound. The two callers' own deadlines
// are observed in TestUpStartsEachUnitUnderItsOwnStartBound and
// TestTheTransactionWaitsTheServersExtendedStart.
func TestTheServersStartDeadlineCoversItsExtension(t *testing.T) {
	t.Parallel()

	server := unitStartDeadline(deploy.ServerUnitName)
	if floor := deploy.UnitStartTimeout + state.StartupBound() + deploy.UnitStartTimeout; server <= floor {
		t.Errorf("billet waits %s for the server to start, and systemd may wait %s", server, floor)
	}

	if node := unitStartDeadline(deploy.NodeUnitName); node != deploy.UnitStartTimeout+lifecycleDeadlineMargin {
		t.Errorf("billet waits %s for the node to start; its unit's bound and the margin are %s",
			node, deploy.UnitStartTimeout+lifecycleDeadlineMargin)
	}
}

// THE CANDIDATE PROBE OUTLASTS A COLD LEDGER OPEN. The host upgrade runs the
// candidate's probe with the services stopped, so its ledger open reads cold
// and may take its whole bound; a probe deadline inside that would roll back a
// healthy candidate exactly as the shared startup budget once did.
func TestTheCandidateProbeOutlastsAColdLedgerOpen(t *testing.T) {
	t.Parallel()

	if candidateProbeDeadline < state.StartupBound()+30*time.Second {
		t.Errorf("the candidate probe is given %s and its ledger open may take %s", candidateProbeDeadline,
			state.StartupBound())
	}
}
