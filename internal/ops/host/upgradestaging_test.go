package host

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/releasesource"
)

// gatewayTimeout is a transport that answers every request 504 Gateway Timeout,
// as GitHub's release downloads did on 2026-11-09, and counts them.
type gatewayTimeout struct{ asked atomic.Int32 }

func (g *gatewayTimeout) RoundTrip(req *http.Request) (*http.Response, error) {
	g.asked.Add(1)

	return &http.Response{
		StatusCode: http.StatusGatewayTimeout, Status: "504 Gateway Timeout",
		Body: io.NopCloser(strings.NewReader("")), Request: req, Header: http.Header{},
	}, nil
}

// A CANDIDATE THAT CANNOT BE DOWNLOADED IS A REFUSAL, NOT A TAKEN JOB (#817). The
// claim is given back, nothing was accepted, and the node is answered with the
// staging failure, so the rollout backs off and retries; before, the job was
// accepted first and the failed download left a claim nothing worked on.
func TestACandidateThatCannotBeDownloadedIsRefusedAndGivesTheClaimBack(t *testing.T) {
	withUpgradeRoot(t)

	prev := runningRelease
	runningRelease = func() string { return "v0.16.0" }
	t.Cleanup(func() { runningRelease = prev })

	ack, answer := ackReader(t)
	transport := &gatewayTimeout{}
	client := &releasesource.Client{HTTP: &http.Client{Transport: transport}}

	err := actOnResolved(t.Context(), processEnv(), &config.Config{}, "", hostUpgradeTarget{generation: 10},
		ack, client, candidateManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err == nil || !strings.Contains(err.Error(), "staging v0.17.0") ||
		!strings.Contains(err.Error(), "504") {
		t.Fatalf("a candidate answered 504 returned %v, want the staging failure", err)
	}
	if transport.asked.Load() < 2 {
		t.Errorf("the archive was asked for %d time(s); a 504 is worth another try", transport.asked.Load())
	}

	ack.refuse(err)
	if got := answer(); !strings.HasPrefix(got, node.AckRefused) || !strings.Contains(got, "staging v0.17.0") {
		t.Errorf("the node was answered %q, want a refusal naming the staging failure", got)
	}
	noClaim(t)
}

// A RESUME OF A TRANSACTION THAT NEVER STAGED ABANDONS IT RATHER THAN STOPPING
// ANYTHING (#817): it has no candidate to install, and nothing was touched. On
// 2026-11-09 a resume at `claimed` stopped both services and then failed for the
// missing binary.
func TestAResumeOfATransactionThatNeverStagedAbandonsIt(t *testing.T) {
	withUpgradeRoot(t)

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.34.0", ToVersion: "v0.35.0", Step: hostupgrade.StepClaimed,
	})

	if err := resumeHostUpgrade(t.Context(), processEnv(), &config.Config{}); err != nil {
		t.Fatalf("resuming a transaction that never staged: %v", err)
	}
	if _, err := os.Lstat(activePath()); !os.IsNotExist(err) {
		t.Errorf("the claim survived: %v", err)
	}
	if _, err := os.Stat(journal.Dir); !os.IsNotExist(err) {
		t.Errorf("the recovery directory survived: %v", err)
	}
}

// A START THAT FINDS A CLAIM A DEAD UPDATER LEFT BEFORE STAGING TAKES THE HOST
// (#817): the transaction lock says nobody is working on it, and `claimed` says
// nothing was touched, so every later dispatch is not refused as an upgrade in
// progress until somebody resumes it by hand. A claim past staging is left.
func TestAStartAbandonsOnlyAClaimThatNeverStaged(t *testing.T) {
	for name, tc := range map[string]struct {
		step    hostupgrade.Step
		removed bool
	}{
		"claimed": {hostupgrade.StepClaimed, true},
		"staged":  {hostupgrade.StepStaged, false},
		"stopped": {hostupgrade.StepStopped, false},
	} {
		t.Run(name, func(t *testing.T) {
			withUpgradeRoot(t)

			journal := stageJournal(t, &hostupgrade.Journal{
				FromVersion: "v0.34.0", ToVersion: "v0.35.0", Step: tc.step,
			})
			if err := abandonUntouchedClaim(processEnv(), heldTxLock(t)); err != nil {
				t.Fatalf("abandonUntouchedClaim: %v", err)
			}

			_, claimErr := os.Lstat(activePath())
			_, dirErr := os.Stat(journal.Dir)
			if tc.removed && (!os.IsNotExist(claimErr) || !os.IsNotExist(dirErr)) {
				t.Errorf("a claim at %s was kept (claim %v, directory %v)", tc.step, claimErr, dirErr)
			}
			if !tc.removed && (claimErr != nil || dirErr != nil) {
				t.Errorf("a claim at %s was abandoned (claim %v, directory %v)", tc.step, claimErr, dirErr)
			}
		})
	}
}

// AND THE START CALLS IT BEFORE ANYTHING IT TAKES: with a dead claim at
// `claimed`, a start is not refused as an upgrade already in progress.
func TestAStartIsNotRefusedByAClaimThatNeverStaged(t *testing.T) {
	withUpgradeRoot(t)

	stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.34.0", ToVersion: "v0.35.0", Step: hostupgrade.StepClaimed,
	})

	err := startHostUpgradeHolding(t.Context(), processEnv(), &config.Config{}, "", hostUpgradeTarget{
		channel: "no-such-channel",
	}, newUpgradeAck(""), releasesource.Policy{}, heldTxLock(t))
	if errors.Is(err, ErrUpgradeInProgress) || err != nil && strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("a start beside a claim that never staged was refused as an upgrade in progress: %v", err)
	}
	if _, err := os.Lstat(activePath()); !os.IsNotExist(err) {
		t.Errorf("the dead claim survived the start: %v", err)
	}
}
