package host

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/state"
)

// A SERVER'S START IS WAITED FOR AS LONG AS SYSTEMD WAITS FOR IT. The server
// asks for an extension before it opens the ledger, which systemd grants on top
// of up to the unit's whole start bound already spent, so a caller that gave
// up at the plain bound would call a slow, healthy start a failure; the node
// asks for nothing and keeps the plain bound.
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

	// AND THE TWO PLACES THAT START THE SERVER WAIT THAT LONG: a start bounded by
	// the plain unit bound there is the defect this guards.
	for _, file := range []string{"localup.go", "hostupgrade.go"} {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(string(body), "deploy.UnitStartTimeout+lifecycleDeadlineMargin") {
			t.Errorf("%s bounds a start by the plain unit bound; it starts the server, so it waits "+
				"unitStartDeadline(unit)", file)
		}

		if !strings.Contains(string(body), "unitStartDeadline(") {
			t.Errorf("%s no longer waits a start with unitStartDeadline", file)
		}
	}
}
