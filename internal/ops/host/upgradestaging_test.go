package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// stalledRelease is a transport that never answers until its request's context
// ends, as a release server under an outage can.
type stalledRelease struct{}

func (stalledRelease) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()

	return nil, req.Context().Err()
}

// publishedArchive serves one release archive holding a billet binary, and
// returns a manifest naming it with its real digest and size.
func publishedArchive(t *testing.T) (*releasesource.Client, *releasesource.Manifest) {
	t.Helper()

	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	tarred := tar.NewWriter(zipped)
	body := []byte("#!/bin/sh\nexit 0\n")
	if err := tarred.WriteHeader(&tar.Header{Name: "billet", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarred.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarred.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())

	manifest := candidateManifest(t)
	manifest.Artifacts[0].SHA256 = hex.EncodeToString(sum[:])
	manifest.Artifacts[0].Size = int64(archive.Len())

	served := archive.Bytes()
	client := &releasesource.Client{HTTP: &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
			Body: io.NopCloser(bytes.NewReader(served)), Request: req,
		}, nil
	})}}

	return client, manifest
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// THE ANSWER'S DEADLINE BOUNDS THE STAGING TOO: a download still going when the
// node stops listening is a refusal, with the claim given back, never an
// acceptance nobody hears (#817).
func TestAStagingThatOutrunsTheAnswersDeadlineIsRefused(t *testing.T) {
	withUpgradeRoot(t)

	prev := runningRelease
	runningRelease = func() string { return "v0.16.0" }
	t.Cleanup(func() { runningRelease = prev })

	ack, answer := ackReader(t)
	ack.deadline = time.Now().Add(300 * time.Millisecond)
	client := &releasesource.Client{HTTP: &http.Client{Transport: stalledRelease{}}}

	started := time.Now()
	err := actOnResolved(t.Context(), processEnv(), &config.Config{}, "", hostUpgradeTarget{generation: 10},
		ack, client, candidateManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err == nil || !strings.Contains(err.Error(), "staging v0.17.0") {
		t.Fatalf("a staging past the answer's deadline returned %v, want the staging failure", err)
	}
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("the staging ran %s past its deadline", waited)
	}

	ack.refuse(err)
	if got := answer(); !strings.HasPrefix(got, node.AckRefused) {
		t.Errorf("the node was answered %q, want a refusal", got)
	}
	noClaim(t)
}

// THE RESOLVE SHARES THE ANSWER'S DEADLINE: a release server that never answers
// the manifest leaves the updater refusing when the node stops listening, with
// nothing claimed, never hung past it.
func TestAResolveThatOutrunsTheAnswersDeadlineIsRefused(t *testing.T) {
	withUpgradeRoot(t)

	prev := newReleaseClient
	newReleaseClient = func() *releasesource.Client {
		return &releasesource.Client{HTTP: &http.Client{Transport: stalledRelease{}}}
	}
	t.Cleanup(func() { newReleaseClient = prev })

	ack, answer := ackReader(t)
	ack.deadline = time.Now().Add(300 * time.Millisecond)

	started := time.Now()
	err := startHostUpgradeHolding(t.Context(), processEnv(), &config.Config{}, "",
		hostUpgradeTarget{pin: "v0.17.0", generation: 10}, ack, releasesource.Policy{}, heldTxLock(t))
	if err == nil {
		t.Fatal("a resolve that never answered succeeded")
	}
	if waited := time.Since(started); waited > 30*time.Second {
		t.Fatalf("the resolve ran %s past the answer's deadline", waited)
	}

	ack.refuse(err)
	if got := answer(); !strings.HasPrefix(got, node.AckRefused) {
		t.Errorf("the node was answered %q, want a refusal", got)
	}
	noClaim(t)
}

// A STAGING THAT FINISHES AFTER THE ANSWER'S DEADLINE IS STILL REFUSED: the
// deadline passes while the archive downloads, the staging completes, and the
// check before the answer gives the claim back rather than accept into a node
// that has stopped listening.
func TestAStagingThatFinishesPastTheAnswersDeadlineIsRefused(t *testing.T) {
	withUpgradeRoot(t)

	prev := runningRelease
	runningRelease = func() string { return "v0.16.0" }
	t.Cleanup(func() { runningRelease = prev })

	client, manifest := publishedArchive(t)
	ack, answer := ackReader(t)
	serve := client.HTTP.Transport
	client.HTTP.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		// THE NODE STOPS LISTENING WHILE THE ARCHIVE IS ON ITS WAY; the staging's
		// own context was set from the deadline before, so it finishes.
		ack.deadline = time.Now().Add(-time.Second)

		return serve.RoundTrip(req)
	})

	err := actOnResolved(t.Context(), processEnv(), &config.Config{}, "", hostUpgradeTarget{generation: 10},
		ack, client, manifest, strings.Repeat("b", 64), heldTxLock(t))
	if !errors.Is(err, errAnswerLate) {
		t.Fatalf("a staging that finished past the deadline returned %v, want errAnswerLate", err)
	}

	ack.refuse(err)
	if got := answer(); !strings.HasPrefix(got, node.AckRefused) {
		t.Errorf("the node was answered %q, want a refusal", got)
	}
	noClaim(t)
}

// AN ACCEPTANCE THE NODE DID NOT TAKE STOPS THE TRANSACTION BEFORE ANYTHING IS
// STOPPED (#817): the candidate staged, but nobody is listening, so the claim is
// given back rather than a drain begun that the rollout does not know about.
func TestAnAcceptanceTheNodeDidNotTakeGivesTheClaimBack(t *testing.T) {
	withUpgradeRoot(t)

	prev := runningRelease
	runningRelease = func() string { return "v0.16.0" }
	t.Cleanup(func() { runningRelease = prev })

	client, manifest := publishedArchive(t)
	ack := newUpgradeAck(filepath.Join(t.TempDir(), "nobody-listens"))

	err := actOnResolved(t.Context(), processEnv(), &config.Config{}, "", hostUpgradeTarget{generation: 10},
		ack, client, manifest, strings.Repeat("b", 64), heldTxLock(t))
	if !errors.Is(err, errAcceptanceLost) {
		t.Fatalf("an acceptance nobody took returned %v, want errAcceptanceLost", err)
	}
	noClaim(t)
}

// A RESUME ANSWERS NO NODE: one waiting on it is refused at once, through the
// command itself, and the transaction on the machine is left as it was. A
// resume that finished would drain past the node's wait, and one that abandoned
// would be heard as an acceptance of nothing.
func TestAResumeRefusesAnAnswerSocket(t *testing.T) {
	withUpgradeRoot(t)

	journal := stageJournal(t, &hostupgrade.Journal{
		FromVersion: "v0.34.0", ToVersion: "v0.35.0", Step: hostupgrade.StepClaimed,
	})
	ack, answer := ackReader(t)

	err := Upgrade(t.Context(), processEnv(), []string{"--resume", "--ack-path", ack.path})
	if err == nil || !strings.Contains(err.Error(), "accepts no --ack-path") {
		t.Fatalf("a resume given an answer socket returned %v, want it refused", err)
	}
	if got := answer(); !strings.HasPrefix(got, node.AckRefused) || !strings.Contains(got, "--ack-path") {
		t.Errorf("the node was answered %q, want a refusal naming --ack-path", got)
	}
	if _, err := os.Stat(journal.Dir); err != nil {
		t.Errorf("a refused resume touched the transaction: %v", err)
	}
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
