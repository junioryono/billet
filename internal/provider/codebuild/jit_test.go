package codebuild

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// A PARAMETER IS NEVER ADDRESSED FOR A BUILD BILLET CANNOT NAME.
//
// The parameter name is DERIVED from the lease, so an empty lease yields the path
// prefix itself with a trailing slash — a perfectly valid parameter name, and one
// billet never staged anything under. The teardown path reads that name out of a
// RESPONSE (the build's own environment markers), so an absent marker is exactly how
// an empty one arrives, and a build billet did not create is exactly the case where it
// is absent.
//
// This is the App-key rule applied one service over: nothing is deleted by a pathname
// unless it is known what that pathname holds.
func TestAnEmptyLeaseNameNeverAddressesAParameter(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	// A parameter standing at the bare path, which is what an empty name would
	// address. It must survive.
	bare := strings.TrimSuffix(p.cfg.JITParameterPath, "/") + "/"
	f.params[bare] = "not billet's"

	if err := p.deleteJITConfig(t.Context(), ""); !errors.Is(err, errNoParameterName) {
		t.Errorf("deleteJITConfig(\"\") = %v, want errNoParameterName", err)
	}

	if err := p.putJITConfig(t.Context(), "", theRegistration); !errors.Is(err, errNoParameterName) {
		t.Errorf("putJITConfig(\"\") = %v, want errNoParameterName", err)
	}

	if got := f.params[bare]; got != "not billet's" {
		t.Errorf("the parameter at the bare path was modified or removed: %q", got)
	}

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".DeleteParameter") ||
			strings.HasSuffix(r.target, ".PutParameter") {
			t.Errorf("an empty lease name still reached Parameter Store: %s %s", r.target, r.body)
		}
	}
}

// THE STAGED PARAMETER ASKS FOR A TIER THAT CAN HOLD A REAL REGISTRATION.
//
// THIS IS THE ONE ASSERTION THE WHOLE SUITE WAS MISSING, and its absence cost the
// first live acceptance run. A standard SSM parameter caps its value at 4096
// characters and a GitHub JIT runner configuration is larger than that, so EVERY
// launch died at staging with a bare `ValidationException` — while every test in this
// package passed, because every one of them stages a few dozen bytes. The single
// value that differs between the fake and production was the one that broke.
//
// SO THE ASSERTION IS ON THE REQUEST, NOT ON A ROUND TRIP. A fake cannot enforce a
// limit it does not have, and giving it one would be the fake reimplementing the rule
// it is used to test — the failure billet-testing names. What is checkable here is
// that billet ASKED, exactly, for a tier that is not the capped default: misspell it,
// drop it, or set it to Standard and this fails, which is the whole of what a unit
// test can honestly promise about somebody else's limit.
func TestTheStagedRegistrationAsksForATierThatCanHoldIt(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	if err := p.putJITConfig(t.Context(), "billet-abc", theRegistration); err != nil {
		t.Fatalf("putJITConfig: %v", err)
	}

	var puts int

	for _, r := range f.bodies() {
		if !strings.HasSuffix(r.target, ".PutParameter") {
			continue
		}

		puts++

		var req struct {
			Tier string `json:"Tier"`
			Type string `json:"Type"`
		}

		if err := json.Unmarshal([]byte(r.body), &req); err != nil {
			t.Fatalf("decode the PutParameter request: %v", err)
		}

		// EXACTLY. "Standard" is the default that cannot hold a registration, and
		// anything AWS does not recognise is a ValidationException at every launch.
		if req.Tier != "Intelligent-Tiering" {
			t.Errorf("the registration is staged with Tier %q; a standard parameter caps "+
				"its value at 4096 characters and a real JIT config exceeds that, so every "+
				"launch would fail at staging", req.Tier)
		}

		// And it stays a SecureString — a tier that can hold it is worth nothing if
		// the credential is staged in the clear.
		if req.Type != "SecureString" {
			t.Errorf("the registration is staged as %q, not SecureString", req.Type)
		}
	}

	// Without this the loop above is vacuous: no PutParameter, no assertions, green.
	if puts != 1 {
		t.Fatalf("putJITConfig made %d PutParameter calls, want exactly 1", puts)
	}
}

// AND A TEARDOWN OF A BUILD WITH NO LEASE MARKER STILL CONFIRMS THE STOP.
//
// The two are separate obligations, and conflating them would be wrong in either
// direction: refusing the teardown because the credential could not be tidied would
// hold capacity for compute that is provably gone, while tidying a name billet
// invented is the deletion above. So the stop is reported and the cleanup is skipped
// with a line in the log.
func TestATeardownWithNoLeaseMarkerStillReportsTheStop(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	// A build in billet's project with no markers at all, which is what a build
	// somebody started by hand looks like.
	f.builds["billet-linux:bare"] = &fakeBuild{
		id: "billet-linux:bare", status: "IN_PROGRESS", phase: "BUILD", started: time.Now(),
	}

	teardown, err := p.Destroy(t.Context(), "billet-linux:bare")
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Errorf("Destroy = %v; the build reached a terminal state, so the capacity must be "+
			"released whatever happened to the credential cleanup", teardown)
	}
}

// THE PARAMETER NAME CARRIES THE LEASE, so an operator who finds a stray one can tell
// which lease it belonged to — the same property provider.InstanceName gives compute.
func TestTheParameterNameCarriesTheLease(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("0123456789abcdef")

	got := p.jitParameterName(name)
	if !strings.HasSuffix(got, "/"+name) {
		t.Errorf("jitParameterName(%q) = %q, which does not end with the lease name", name, got)
	}

	if !strings.HasPrefix(got, p.cfg.JITParameterPath) {
		t.Errorf("jitParameterName(%q) = %q, which is outside the configured path %q",
			name, got, p.cfg.JITParameterPath)
	}

	// AND IT IS UNIQUE PER LEASE, which is what makes the no-overwrite refusal in
	// putJITConfig mean something.
	other := p.jitParameterName(provider.InstanceName("fedcba9876543210"))
	if got == other {
		t.Error("two leases share a parameter name, so one launch would refuse the other")
	}
}
