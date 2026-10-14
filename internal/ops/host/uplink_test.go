package host

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

	body, err := os.ReadFile("../../../deploy/scripts/preremove.sh")
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

// A CEILING THE SHAPER COULD NOT HOLD IS REFUSED BEFORE ANYTHING IS SHAPED, on
// every platform, naming the flag.
func TestTheUplinkShaperRefusesACeilingItCannotHold(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"shape", "--max-up", "-1"},
		{"shape", "--max-down", "5"},
		{"shape", "--max-up", "NaN"},
		{"shape", "--max-down", "+Inf"},
	} {
		err := Uplink(t.Context(), processEnv(), args)
		if err == nil || !strings.Contains(err.Error(), args[1]+" ") {
			t.Errorf("billet uplink %s: err = %v, want a refusal naming %s", strings.Join(args, " "), err, args[1])
		}
	}

	for _, mbit := range []float64{0, 10, 400} {
		if err := uplinkCeiling("--max-up", mbit); err != nil {
			t.Errorf("a ceiling of %v Mbit/s was refused: %v", mbit, err)
		}
	}
}

// THE CEILINGS THE FLAGS NAME ARE THE SHAPER'S, each to its own direction, and
// none named is none.
func TestTheUplinkFlagsReachTheShaper(t *testing.T) {
	t.Parallel()

	opts, err := uplinkShapeOptions(processEnv(), []string{"--max-up", "400", "--max-down", "300"})
	if err != nil {
		t.Fatal(err)
	}

	if opts.MaxUp != 400 || opts.MaxDown != 300 {
		t.Fatalf("the shaper was asked for %v up and %v down, want 400 and 300", opts.MaxUp, opts.MaxDown)
	}

	opts, err = uplinkShapeOptions(processEnv(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if opts.MaxUp != 0 || opts.MaxDown != 0 {
		t.Fatalf("with no flags the shaper was asked for %v up and %v down, want neither", opts.MaxUp, opts.MaxDown)
	}
}
