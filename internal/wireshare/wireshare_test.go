package wireshare

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

const (
	deploymentA = "deployment-a"
	deploymentB = "deployment-b"
)

// fakeStore is the identity store, in memory.
//
// IT JUDGES NOTHING, which is the rule a fake follows here: every refusal this
// package makes has to be this package's, or the test is about the fake.
type fakeStore struct {
	body []byte
	puts int
}

func (f *fakeStore) GetAuthority(context.Context) ([]byte, error) {
	if f.body == nil {
		return nil, ErrNoAuthority
	}

	return f.body, nil
}

func (f *fakeStore) PutAuthority(_ context.Context, body []byte) error {
	f.body = body
	f.puts++

	return nil
}

// withAuthority makes a state directory holding a real authority.
func withAuthority(t *testing.T, deployment string) string {
	t.Helper()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	return dir
}

// fingerprintAt is the authority a state directory holds.
func fingerprintAt(t *testing.T, dir string) string {
	t.Helper()

	authority, err := wirecert.ReadAuthority(dir)
	if err != nil {
		t.Fatalf("ReadAuthority(%s): %v", dir, err)
	}

	cert, err := wirecert.ParseAuthorityPair(
		authority.Present["ca.key"], authority.Present["ca.crt"])
	if err != nil {
		t.Fatalf("ParseAuthorityPair: %v", err)
	}

	return wirecert.FingerprintOfCert(cert)
}

// A SECOND CONTROLLER ADOPTS THE AUTHORITY THE FLEET ALREADY TRUSTS.
//
// THE FAILURE THIS PREVENTS is the whole reason the package exists: a promoted
// host whose ca directory is empty mints a NEW authority, after which every node
// in the fleet fails to verify the control plane and drops off at once — while
// the control plane itself looks perfectly healthy.
func TestAHostWithNoAuthorityAdoptsTheDeploymentsOwn(t *testing.T) {
	store := &fakeStore{}
	first := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, first, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	second := t.TempDir()

	got, err := Adopt(t.Context(), store, second, deploymentA, false)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got != AdoptedInstalled {
		t.Fatalf("Adopt = %v, want AdoptedInstalled", got)
	}

	// THE SAME AUTHORITY, not merely a working one. A host that minted its own
	// would pass every structural check and fail every node.
	if a, b := fingerprintAt(t, first), fingerprintAt(t, second); a != b {
		t.Errorf("the second host holds %s and the first holds %s", b, a)
	}

	// AND THE PRIVATE KEY IS NOT WORLD-READABLE. It arrived over a network as
	// base64 in a JSON document; nothing about the mode it lands in comes from the
	// file it was read from.
	info, err := os.Stat(filepath.Join(wirecert.CADir(second), "ca.key"))
	if err != nil {
		t.Fatalf("stat the installed key: %v", err)
	}

	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the installed CA key is mode %o; a private key must not be readable by "+
			"group or other", perm)
	}
}

// AN EMPTY STORE IS AN ORDINARY STATE AND SAYS SO.
//
// THREE ANSWERS AND NOT TWO: "nothing published" tells the caller it is the host
// that has to publish, and collapsing it into "already held" would leave a
// deployment whose store holds no authority at all.
func TestAnEmptyStoreIsReportedRatherThanRefused(t *testing.T) {
	got, err := Adopt(t.Context(), &fakeStore{}, t.TempDir(), deploymentA, false)
	if err != nil {
		t.Fatalf("Adopt against an empty store: %v", err)
	}

	if got != AdoptedNothing {
		t.Errorf("Adopt = %v, want AdoptedNothing", got)
	}
}

// A HOST THAT ALREADY HOLDS THE PUBLISHED AUTHORITY HAS NOTHING TO DO.
func TestAHostHoldingTheSameAuthorityIsLeftAlone(t *testing.T) {
	store := &fakeStore{}
	dir := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, dir, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := Adopt(t.Context(), store, dir, deploymentA, false)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if got != AdoptedAlreadyHeld {
		t.Errorf("Adopt = %v, want AdoptedAlreadyHeld", got)
	}
}

// A LOCAL AUTHORITY IS NEVER WRITTEN OVER.
//
// The file being replaced is what every node in the fleet verifies this control
// plane against, and billet cannot tell a host left behind by a rotation from one
// pointed at the wrong deployment. An operator can, which is what --force is.
func TestADifferentLocalAuthorityIsRefusedRatherThanReplaced(t *testing.T) {
	store := &fakeStore{}
	published := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, published, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// A SECOND AUTHORITY FOR THE SAME DEPLOYMENT, which is what a host that minted
	// its own before anything was published holds.
	other := withAuthority(t, deploymentA)
	before := fingerprintAt(t, other)

	_, err := Adopt(t.Context(), store, other, deploymentA, false)
	if err == nil {
		t.Fatal("an authority was written over")
	}

	// NAMING BOTH, because an operator meeting this has two hosts and has to
	// decide which one is behind.
	for _, want := range []string{before, fingerprintAt(t, published)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %s; got: %v", want, err)
		}
	}

	if after := fingerprintAt(t, other); after != before {
		t.Errorf("the local authority changed from %s to %s despite the refusal", before, after)
	}
}

// AND --force MOVES THE OLD ONE ASIDE RATHER THAN DELETING IT.
//
// What is being set aside is a private key. An operator who forced the wrong
// direction has to be able to put it back, and a deletion is the one outcome
// nothing recovers from.
func TestForcingASyncKeepsTheAuthorityItReplaced(t *testing.T) {
	store := &fakeStore{}
	published := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, published, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	other := withAuthority(t, deploymentA)
	before := fingerprintAt(t, other)

	got, err := Adopt(t.Context(), store, other, deploymentA, true)
	if err != nil {
		t.Fatalf("Adopt --force: %v", err)
	}

	if got != AdoptedInstalled {
		t.Fatalf("Adopt = %v, want AdoptedInstalled", got)
	}

	if now := fingerprintAt(t, other); now != fingerprintAt(t, published) {
		t.Errorf("the host holds %s after a forced sync, want the published one", now)
	}

	// THE SUPERSEDED KEY IS STILL THERE. Nothing here reads it again, but an
	// operator who forced the wrong way round needs it to exist.
	entries, err := os.ReadDir(other)
	if err != nil {
		t.Fatalf("read the state directory: %v", err)
	}

	var superseded bool

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ca.superseded-") && e.IsDir() {
			superseded = true
		}
	}

	if !superseded {
		t.Errorf("a forced sync deleted the authority it replaced (it held %s); %s holds %v",
			before, other, entries)
	}
}

// AN AUTHORITY BELONGING TO ANOTHER DEPLOYMENT IS REFUSED.
//
// Verifying against the CA is what decides which nodes may connect, so adopting
// one that names somebody else silently re-points that decision — and a prefix
// pointed at the wrong deployment is exactly how it happens.
func TestAnAuthorityFromAnotherDeploymentIsRefused(t *testing.T) {
	store := &fakeStore{}
	theirs := withAuthority(t, deploymentB)

	if err := Publish(t.Context(), store, theirs, deploymentB); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	_, err := Adopt(t.Context(), store, t.TempDir(), deploymentA, false)
	if !errors.Is(err, wirecert.ErrForeignAuthority) {
		t.Fatalf("Adopt of another deployment's authority = %v, want ErrForeignAuthority", err)
	}
}

// A DOCUMENT THIS BUILD CANNOT FULLY INSTALL IS REFUSED RATHER THAN PARTLY READ.
//
// A CLOSED SET, the rule the archive states: a store written by a NEWER billet
// carrying a file this one does not know is the case that matters, and silently
// dropping half an authority there is how a host adopts something that does not
// hold together.
func TestADocumentCarryingAnUnknownFileIsRefused(t *testing.T) {
	store := &fakeStore{}
	dir := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, dir, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var doc document
	if err := json.Unmarshal(store.body, &doc); err != nil {
		t.Fatalf("decode the published document: %v", err)
	}

	doc.Files["ca-next.key"] = doc.Files["ca.key"]

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	store.body = body

	_, err = Adopt(t.Context(), store, t.TempDir(), deploymentA, false)
	if err == nil {
		t.Fatal("a document carrying an unknown file was installed")
	}

	if !strings.Contains(err.Error(), "ca-next.key") {
		t.Errorf("the refusal should name the file it does not know; got: %v", err)
	}
}

// AND ONE MISSING A REQUIRED FILE IS NOT A SHORTER AUTHORITY.
func TestADocumentMissingARequiredFileIsRefused(t *testing.T) {
	store := &fakeStore{}
	dir := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, dir, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var doc document
	if err := json.Unmarshal(store.body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	delete(doc.Files, "authority-created")

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	store.body = body

	if _, err := Adopt(t.Context(), store, t.TempDir(), deploymentA, false); err == nil {
		t.Fatal("a document missing the authority marker was installed")
	}
}

// A ROTATION PUBLISHES, AND A SECOND HOST PICKS IT UP WHEN ASKED.
//
// The honest limit of this design, asserted so it cannot drift into a claim of
// convergence: nothing here happens on its own. What is proved is that a rotated
// authority CAN be carried, and that a host holding the previous one is refused
// until somebody says which is right.
func TestARotatedAuthorityIsCarriedWhenItIsPublished(t *testing.T) {
	store := &fakeStore{}
	leader := withAuthority(t, deploymentA)

	if err := Publish(t.Context(), store, leader, deploymentA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	standby := t.TempDir()
	if _, err := Adopt(t.Context(), store, standby, deploymentA, false); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	if _, err := wirecert.Rotate(leader, deploymentA); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if err := Publish(t.Context(), store, leader, deploymentA); err != nil {
		t.Fatalf("Publish after a rotation: %v", err)
	}

	// THE STANDBY IS BEHIND AND IS REFUSED, which is the safe direction: billet
	// cannot tell it from a host pointed at the wrong deployment.
	if _, err := Adopt(t.Context(), store, standby, deploymentA, false); err == nil {
		t.Fatal("a rotated authority was written over the standby's without --force")
	}

	// AND THE ROTATION'S PREVIOUS PAIR TRAVELLED, which is what lets a forced
	// standby present a certificate the un-renewed fleet still verifies.
	var doc document
	if err := json.Unmarshal(store.body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if doc.Files[name] == "" {
			t.Errorf("the published authority dropped %s, so an adopting host could not "+
				"present what the un-renewed fleet still trusts", name)
		}
	}
}
