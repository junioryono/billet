package node

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// recordingUpgrader captures the instruction rather than acting on it.
type recordingUpgrader struct {
	got  []nodeapi.UpgradeSpec
	fail error
}

func (u *recordingUpgrader) StartUpgrade(_ context.Context, spec nodeapi.UpgradeSpec) error {
	u.got = append(u.got, spec)

	return u.fail
}

// A NODE WITH NO UPDATER REFUSES, RATHER THAN SILENTLY DOING NOTHING.
//
// The alternative is a rollout that records this host as instructed and then
// waits forever for a convergence that cannot come — a fleet that looks like it
// is mid-upgrade and is not, with nothing anywhere saying so.
func TestANodeWithNoUpgraderRefusesTheCommand(t *testing.T) {
	r := &Runner{log: quietUpgradeLogger()}

	err := r.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.4.0"})
	if !errors.Is(err, ErrNoUpgrader) {
		t.Fatalf("StartUpgrade with no updater: %v, want ErrNoUpgrader", err)
	}

	// AND THE REFUSAL NAMES THE RELEASE AND THE WAY OUT, because the operator
	// reading it is looking at one host in a rollout that has stopped.
	if !strings.Contains(err.Error(), "v0.4.0") ||
		!strings.Contains(err.Error(), "out of band") {
		t.Errorf("the refusal does not say what to do: %v", err)
	}
}

// THE INSTRUCTION REACHES THE UPDATER INTACT, including the fence.
//
// A delayed command from a superseded rollout must not install a release the
// fleet has moved past, and the generation is what lets the updater notice — so
// dropping it here would remove the only thing that can.
func TestTheUpgradeInstructionReachesTheUpdaterWhole(t *testing.T) {
	u := &recordingUpgrader{}
	r := &Runner{log: quietUpgradeLogger(), upgrader: u}

	want := nodeapi.UpgradeSpec{
		Version:        "v0.4.0",
		ManifestSHA256: strings.Repeat("a", 64),
		RolloutID:      "abc123",
		Generation:     7,
	}

	if err := r.StartUpgrade(t.Context(), want); err != nil {
		t.Fatalf("StartUpgrade: %v", err)
	}

	if len(u.got) != 1 {
		t.Fatalf("the updater was started %d times", len(u.got))
	}

	if u.got[0] != want {
		t.Errorf("the updater received %+v, want %+v", u.got[0], want)
	}
}

// AN UPDATER THAT WILL NOT START IS REPORTED, not swallowed. The control plane
// reads this to decide whether the node reached `installing` at all.
func TestAnUpdaterThatWillNotStartIsReported(t *testing.T) {
	u := &recordingUpgrader{fail: errors.New("no billet on this machine")}
	r := &Runner{log: quietUpgradeLogger(), upgrader: u}

	err := r.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.4.0"})
	if err == nil {
		t.Fatal("an updater that could not start was reported as started")
	}

	if !strings.Contains(err.Error(), "no billet on this machine") {
		t.Errorf("the failure does not carry the updater's reason: %v", err)
	}
}

// quietUpgradeLogger keeps the upgrade tests' output out of the run.
func quietUpgradeLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// AND IT REACHES THE UPDATER'S COMMAND LINE INTACT TOO, which is a separate
// claim and the one that was false.
//
// TestTheUpgradeInstructionReachesTheUpdaterWhole proves the spec arrives at the
// Upgrader INTERFACE; the real updater is a process, and the argv is where the
// instruction either survives or is quietly reduced to a version string. It was
// reduced: the digest that pins the fleet to one manifest, the rollout id and the
// generation were all built and then not passed, and every test in this file
// still passed.
func TestTheUpdaterIsGivenTheWholeInstructionOnItsCommandLine(t *testing.T) {
	got := upgradeArgs(nodeapi.UpgradeSpec{
		Version:        "v0.4.0",
		ManifestSHA256: strings.Repeat("a", 64),
		RolloutID:      "abc123",
		Generation:     7,
	}, "/etc/billet/billet.yaml")

	line := strings.Join(got, " ")

	for _, want := range []string{
		"--version v0.4.0",
		"--manifest-sha256 " + strings.Repeat("a", 64),
		"--rollout abc123",
		"--generation 7",
		"--config /etc/billet/billet.yaml",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the updater's command line does not carry %q: %s", want, line)
		}
	}
}

// AN OPERATOR'S OWN RUN CARRIES NO FENCE, and the argv says so rather than
// passing empty flags the updater would then have to interpret.
//
// An empty --manifest-sha256 is not "no digest": it is a digest that matches
// nothing, and the updater refuses on a mismatch. Omitting the flag is what makes
// "nobody pinned this" distinct from "this was pinned to nothing".
func TestAnUnfencedUpgradePassesNoFenceFlags(t *testing.T) {
	line := strings.Join(upgradeArgs(nodeapi.UpgradeSpec{Version: "v0.4.0"}, ""), " ")

	for _, unwanted := range []string{"--manifest-sha256", "--rollout", "--generation", "--config"} {
		if strings.Contains(line, unwanted) {
			t.Errorf("an unfenced upgrade passed %s: %s", unwanted, line)
		}
	}
}
