package main

import (
	"os"
	"strings"
	"testing"
)

// A PACKAGE REMOVAL STOPS THE UPLINK SHAPER WHILE ITS CLEANUP CAN STILL RUN, and
// refuses when the record says the cleanup did not finish. Structural, because
// the script acts on /etc/systemd and /run, which no test may: the order and the
// refusal are what a removal that left CAKE on the uplink with no binary to
// clear it would have lacked.
func TestPackageRemovalStopsTheUplinkShaperAndRefusesALeftoverRecord(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("../../deploy/scripts/preremove.sh")
	if err != nil {
		t.Fatal(err)
	}

	script := string(body)
	upgrade := strings.Index(script, "upgrade | failed-upgrade | 1)")
	stop := strings.Index(script, "systemctl disable --now billet-uplink.service")
	refuse := strings.Index(script, "if [ -e /run/billet-uplink/interface ]; then")

	if upgrade < 0 || stop < upgrade || refuse < stop {
		t.Fatalf("preremove.sh: upgrade exit at %d, shaper stop at %d, leftover-record refusal at %d; "+
			"want the stop after the upgrade exit and the refusal after the stop", upgrade, stop, refuse)
	}

	if !strings.Contains(script[refuse:], "exit 1") {
		t.Fatal("preremove.sh notices a leftover record and does not refuse the removal")
	}
}
