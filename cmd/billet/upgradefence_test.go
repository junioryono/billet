package main

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
)

// installedManifest describes the release this binary already is.
//
// IT DRIVES THE NO-OP PATH, which is the one a review found accepting a decision
// without recording it. Everything in it is read from the packages that define
// it, for the reason releasesource.Current is built that way: a manifest with
// numbers typed in would stop matching this build the first time either moved.
func installedManifest(t *testing.T) *releasesource.Manifest {
	t.Helper()

	return &releasesource.Manifest{
		Version:      version.Version(),
		Wire:         releasesource.Range{Min: nodeapi.MinVersion, Max: nodeapi.Version},
		LedgerSchema: state.LatestSchemaVersion(),
		Artifacts: []releasesource.Artifact{{
			Name:   "billet_" + hostOS + "_" + runtime.GOARCH + ".tar.gz",
			OS:     hostOS,
			Arch:   runtime.GOARCH,
			Kind:   releasesource.KindArchive,
			SHA256: strings.Repeat("a", 64),
			Size:   1,
		}},
	}
}

// ackReader gives a test the updater's side of the answer channel and a way to
// read what it said.
func ackReader(t *testing.T) (*upgradeAck, func() string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	t.Cleanup(func() { _ = r.Close() })
	t.Cleanup(func() { _ = w.Close() })

	ack := &upgradeAck{f: w}

	// IT ONLY READS. An earlier version called ack.close() here, which MANUFACTURES
	// a generic refusal when nothing was sent — so a test asserting that a refusal
	// was reported could pass without the code under test having reported one.
	// Closing the writer is the caller's business, exactly as it is cmdHostUpgrade's.
	return ack, func() string {
		_ = w.Close()

		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("read the updater's answer: %v", err)
		}

		return strings.TrimSpace(line)
	}
}

// heldTxLock takes the transaction lock for the length of a test, because that is
// the precondition actOnResolved is written against.
func heldTxLock(t *testing.T) *txLock {
	t.Helper()

	tx, err := takeTxLock()
	if err != nil {
		t.Fatalf("takeTxLock: %v", err)
	}

	t.Cleanup(tx.release)

	return tx
}

// DOING NOTHING IS STILL ACTING ON A DECISION.
//
// When the requested release is already installed the updater has nothing to do
// and says so — but it used to say so BEFORE touching the fence, so a machine
// could satisfy decision 10 with its mark still at 4. A delayed decision 9 then
// passed a stale fence and DOWNGRADED it.
//
// BEHAVIOURAL, AND THE FIRST TWO ATTEMPTS AT THIS WERE NOT. One exercised
// recordDecision and checkDecision directly, which proved those work and went on
// passing with the call deleted; the second asserted the source ORDER of two
// calls, which cannot tell a call that runs from one that is skipped and broke on
// a rename that changed nothing. This runs the branch.
func TestANoOpUpgradeRecordsTheDecisionBeforeAcceptingIt(t *testing.T) {
	withUpgradeRoot(t)

	ack, answer := ackReader(t)

	err := actOnResolved(t.Context(), &config.Config{}, "", hostUpgradeTarget{generation: 10},
		ack, nil, installedManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err != nil {
		t.Fatalf("a machine already on the target refused the instruction: %v", err)
	}

	if got := answer(); got != node.AckAccepted {
		t.Errorf("the updater answered %q, want an acceptance", got)
	}

	// THE MARK MOVED, which is the whole point: this machine has now acted on 10.
	got, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if got != 10 {
		t.Errorf("the mark is %d after a no-op upgrade for decision 10, want 10", got)
	}

	// AND A LATER, OLDER INSTRUCTION IS NOW REFUSED, which is what the mark is for.
	if err := checkDecision(hostUpgradeTarget{generation: 9}); !errors.Is(err, ErrSuperseded) {
		t.Errorf("decision 9 after a no-op for 10 returned %v, want ErrSuperseded", err)
	}
}

// AND A SUPERSEDED INSTRUCTION IS REFUSED BEFORE IT CAN RECORD ANYTHING.
//
// The refusal has to come first: an instruction the fleet has replaced must not
// raise the mark, or it would rewrite the evidence that replaced it.
func TestASupersededNoOpNeitherAcceptsNorRecords(t *testing.T) {
	withUpgradeRoot(t)

	if err := recordDecision(10); err != nil {
		t.Fatalf("recordDecision: %v", err)
	}

	ack, answer := ackReader(t)

	err := actOnResolved(t.Context(), &config.Config{}, "", hostUpgradeTarget{generation: 9},
		ack, nil, installedManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if !errors.Is(err, ErrSuperseded) {
		t.Fatalf("a superseded no-op returned %v, want ErrSuperseded", err)
	}

	// THE SAME BOOKKEEPING cmdHostUpgrade DOES, rather than a helper inventing an
	// answer: the refusal a node reads is the one this error produces.
	ack.refuse(err)

	got := answer()
	if !strings.Contains(got, "refused") {
		t.Fatalf("the updater answered %q, want a refusal", got)
	}

	// AND IT CARRIES THE REASON. A node quotes this into an operator's log, so a
	// generic refusal would leave them looking for a fault that is not there.
	if !strings.Contains(got, "left behind") {
		t.Errorf("the refusal does not say the instruction was superseded: %q", got)
	}

	mark, err := readDecision()
	if err != nil {
		t.Fatalf("readDecision: %v", err)
	}

	if mark != 10 {
		t.Errorf("a superseded instruction moved the mark to %d, want it left at 10", mark)
	}
}

// THE REPAIR A BLOCKED HOST IS TOLD TO RUN ACTUALLY REPAIRS IT.
//
// A rollout blocks a host that reports the target VERSION from a different
// manifest, and its blocker tells the operator to run `billet host-upgrade`. That
// command saw the versions agree, declared there was nothing to do, and left the
// machine exactly as it was — so the rollout blocked it again on the next pass
// and the documented way out was a dead end.
//
// The fast path now requires the installed manifest to agree as well, so a host
// whose record names something else is reinstalled rather than waved through.
func TestAHostWhoseManifestDisagreesIsNotWavedThrough(t *testing.T) {
	withUpgradeRoot(t)

	seedInstalledRecord(t, strings.Repeat("e", 64))

	if !installedDisagrees(strings.Repeat("b", 64)) {
		t.Error("a host recording manifest e... was treated as agreeing with b..., so " +
			"the one repair a blocked host has does nothing")
	}

	// THE SAME MANIFEST IS NOT A DISAGREEMENT, so an ordinary no-op upgrade stays a
	// no-op.
	if installedDisagrees(strings.Repeat("e", 64)) {
		t.Error("a host recording the manifest being installed was read as disagreeing")
	}

	// AND CASE IS NOT A DISAGREEMENT EITHER. A hex digest is the same digest
	// however it was rendered; reinstalling over that would drain a host for a
	// formatting difference.
	if installedDisagrees(strings.ToUpper(strings.Repeat("e", 64))) {
		t.Error("the same digest in a different case was read as disagreeing")
	}
}

// A HOST THAT CANNOT SAY IS NOT REINSTALLED.
//
// No record at all, and a record that no longer describes the running binary, are
// both cases where billet cannot tell — and reinstalling on "cannot tell" would
// stop services and drain compute on every host in the fleet the first time
// anything asked, to fix a diagnostic.
func TestAHostThatCannotSayIsNotReinstalled(t *testing.T) {
	withUpgradeRoot(t)

	// Nothing recorded.
	if installedDisagrees(strings.Repeat("b", 64)) {
		t.Error("a host with no record was read as disagreeing")
	}

	// A record describing bytes that are not the ones running.
	seedStaleRecord(t, strings.Repeat("e", 64))

	if installedDisagrees(strings.Repeat("b", 64)) {
		t.Error("a record that no longer describes this binary was read as disagreeing; " +
			"billet cannot tell what that host is running and must not act as if it can")
	}
}

// seedInstalledRecord writes a record that DOES describe the running binary.
func seedInstalledRecord(t *testing.T, digest string) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}

	sum, err := provenance.HashFile(self)
	if err != nil {
		t.Fatalf("hash the test binary: %v", err)
	}

	withRecordAt(t, provenance.Record{
		Version: "v0.4.0", ManifestDigest: digest, BinarySHA256: sum,
	})
}

// seedStaleRecord writes a record describing some other binary.
func seedStaleRecord(t *testing.T, digest string) {
	t.Helper()

	withRecordAt(t, provenance.Record{
		Version: "v0.4.0", ManifestDigest: digest, BinarySHA256: strings.Repeat("9", 64),
	})
}

func withRecordAt(t *testing.T, record provenance.Record) {
	t.Helper()

	original := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")

	t.Cleanup(func() { provenance.Path = original })

	if err := provenance.Write(record); err != nil {
		t.Fatalf("write the record: %v", err)
	}
}

// unreachable makes every download fail at once, with no network.
//
// The branch under test decides whether to START a transaction. Reaching the
// staging step is the observable difference, and reaching it must not depend on a
// real release server being up.
type unreachable struct{}

func (unreachable) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no release server in this test")
}

// A HOST WHOSE MANIFEST DISAGREES IS REINSTALLED RATHER THAN WAVED THROUGH.
//
// THE BRANCH, NOT THE HELPER. Asserting on installedDisagrees alone proves the
// comparison works and says nothing about whether the fast path consults it —
// and the fast path not consulting it is the whole defect: a rollout blocks a
// host whose manifest disagrees and tells the operator to run this command, which
// saw the versions agree, declared there was nothing to do, and left the machine
// exactly as it was.
//
// Taking the transaction is observable without a network: it claims the machine
// before it downloads anything, so the claim is there whether or not the download
// could have worked.
func TestTheFastPathDoesNotWaveThroughADisagreeingHost(t *testing.T) {
	withUpgradeRoot(t)
	seedInstalledRecord(t, strings.Repeat("e", 64))

	ack, _ := ackReader(t)

	err := actOnResolved(t.Context(), &config.Config{}, "", hostUpgradeTarget{},
		ack, &releasesource.Client{HTTP: &http.Client{Transport: unreachable{}}},
		installedManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err == nil {
		t.Fatal("a host recording a different manifest was waved through as already " +
			"installed, which is exactly the state a rollout blocks and tells an " +
			"operator to fix with this command")
	}

	if _, statErr := os.Lstat(activePath()); statErr != nil {
		t.Errorf("it did not start a transaction: %v", statErr)
	}
}

// AND A HOST THAT CANNOT SAY IS LEFT ALONE.
//
// No record at all is every host in the field before one billet-driven upgrade
// has run. Turning each of their no-op upgrades into a real transaction would
// stop services and drain compute across the fleet to fix a diagnostic.
func TestTheFastPathLeavesAHostThatCannotSayAlone(t *testing.T) {
	withUpgradeRoot(t)

	// No record anywhere.
	original := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "absent.json")

	t.Cleanup(func() { provenance.Path = original })

	ack, _ := ackReader(t)

	err := actOnResolved(t.Context(), &config.Config{}, "", hostUpgradeTarget{},
		ack, &releasesource.Client{HTTP: &http.Client{Transport: unreachable{}}},
		installedManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err != nil {
		t.Fatalf("a host that cannot say was put through a transaction: %v", err)
	}

	if _, statErr := os.Lstat(activePath()); !os.IsNotExist(statErr) {
		t.Errorf("it claimed the machine anyway: %v", statErr)
	}
}

// --reinstall INSTALLS EVEN WHEN THE VERSION ALREADY MATCHES.
//
// The ordinary shortcut asks whether the installed manifest DISAGREES, and a
// record that is damaged or missing answers "cannot tell" — correctly, because
// reinstalling on cannot-tell would drain a whole fleet to fix a diagnostic. But
// the control plane may already hold a disagreeing digest from before that record
// went bad, so the host stays blocked while the command it is told to run decides
// there is nothing to do. This is the way through.
func TestReinstallInstallsEvenWhenTheVersionAlreadyMatches(t *testing.T) {
	withUpgradeRoot(t)

	// Nothing readable, so installedDisagrees cannot tell.
	original := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "absent.json")

	t.Cleanup(func() { provenance.Path = original })

	ack, _ := ackReader(t)

	err := actOnResolved(t.Context(), &config.Config{}, "",
		hostUpgradeTarget{reinstall: true}, ack,
		&releasesource.Client{HTTP: &http.Client{Transport: unreachable{}}},
		installedManifest(t), strings.Repeat("b", 64), heldTxLock(t))
	if err == nil {
		t.Fatal("--reinstall was waved through by the same-version shortcut, so a host " +
			"whose record is damaged has no way back into a rollout")
	}

	if _, statErr := os.Lstat(activePath()); statErr != nil {
		t.Errorf("--reinstall did not start a transaction: %v", statErr)
	}
}
