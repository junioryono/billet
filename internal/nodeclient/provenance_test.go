package nodeclient_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provenance"
)

// A NODE TELLS THE CONTROL PLANE WHICH MANIFEST PRODUCED IT.
//
// This is what makes a rollout able to prove anything. Without it convergence is
// read off the version string a binary was BUILT with — which two builds can
// share, and which a moved tag makes identical — so a host upgraded out of band
// converges a rollout on evidence weaker than the decision it converges.
func TestARegistrationRecordsTheManifestThatProducedTheNode(t *testing.T) {
	want := seedProvenance(t)

	stub := &registerStub{t: t, status: http.StatusOK, body: nodeapi.RegisterResponse{
		Version: nodeapi.Version, MinVersion: nodeapi.MinVersion,
		LeaseTTLSeconds: 60, PollSeconds: 30,
	}}

	c := dialStub(t, stub)

	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if stub.got.InstalledDigest != want {
		t.Errorf("the registration named manifest %q, want %q",
			stub.got.InstalledDigest, want)
	}
}

// AND A MACHINE THAT CANNOT SAY REGISTERS EXACTLY AS BEFORE.
//
// Every host installed from a package, built from source, or upgraded before any
// of this existed has no record — which on the day this ships is the whole fleet,
// including the hosts that would deliver the build able to name a manifest.
// Refusing, or failing, would take a working node out of the fleet over a field
// that authorises nothing.
func TestANodeWithNoRecordRegistersWithoutOne(t *testing.T) {
	// A path that does not exist, so nothing on this machine can say.
	original := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "absent.json")

	t.Cleanup(func() { provenance.Path = original })

	stub := &registerStub{t: t, status: http.StatusOK, body: nodeapi.RegisterResponse{
		Version: nodeapi.Version, MinVersion: nodeapi.MinVersion,
		LeaseTTLSeconds: 60, PollSeconds: 30,
	}}

	c := dialStub(t, stub)

	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("a node with no provenance record could not register: %v", err)
	}

	if stub.got.InstalledDigest != "" {
		t.Errorf("a node with no record named manifest %q", stub.got.InstalledDigest)
	}

	// AND THE REST OF THE REGISTRATION IS UNAFFECTED, which is the point: this
	// field decides how a rollout READS a host, not whether it may work.
	if stub.got.Release == "" || stub.got.Node == "" {
		t.Errorf("the registration lost other fields: %+v", stub.got)
	}
}

// seedProvenance writes a record describing the binary this test is running, so
// the client's own check accepts it, and returns the digest it should report.
//
// AGAINST THE TEST BINARY ON PURPOSE. The node proves the record still describes
// the executable before reporting it, so a record naming any other file would be
// correctly ignored — and a test that worked around that check would be asserting
// against a path the production code does not take.
func seedProvenance(t *testing.T) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("find the test binary: %v", err)
	}

	sum, err := provenance.HashFile(self)
	if err != nil {
		t.Fatalf("hash the test binary: %v", err)
	}

	original := provenance.Path
	provenance.Path = filepath.Join(t.TempDir(), "installed.json")

	t.Cleanup(func() { provenance.Path = original })

	digest := strings.Repeat("b", 64)

	if err := provenance.Write(provenance.Record{
		Version: "v0.4.0", ManifestDigest: digest, BinarySHA256: sum,
	}); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	return digest
}

// THE ANSWER IS TAKEN ONCE, WHEN THE CLIENT IS BUILT, AND HELD.
//
// Hashing the executable is a ~22MB read and a node re-registers on every
// reconnect, so asking per registration pays for one answer repeatedly. The
// stronger reason is correctness: the answer must describe the bytes THIS PROCESS
// STARTED WITH. os.Executable resolves to a path, and an updater replacing billet
// underneath a node that is draining is not hypothetical — it is the ordinary
// upgrade — so a per-registration read would report the incoming release as
// though the running node were already on it.
//
// The record is removed after the client exists, which a per-registration read
// would notice and a held one would not.
func TestTheClientTakesItsProvenanceOnceAndHoldsIt(t *testing.T) {
	want := seedProvenance(t)

	stub := &registerStub{t: t, status: http.StatusOK, body: nodeapi.RegisterResponse{
		Version: nodeapi.Version, MinVersion: nodeapi.MinVersion,
		LeaseTTLSeconds: 60, PollSeconds: 30,
	}}

	c := dialStub(t, stub)

	if err := os.Remove(provenance.Path); err != nil {
		t.Fatalf("remove the record: %v", err)
	}

	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if stub.got.InstalledDigest != want {
		t.Errorf("the registration named %q after the record was removed, want %q — the "+
			"client is reading it per registration rather than holding the answer it "+
			"started with", stub.got.InstalledDigest, want)
	}
}
