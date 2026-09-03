package wirecert_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

// A ROTATION IS A SEQUENCE OF FILESYSTEM OPERATIONS, AND A CONTROL PLANE CAN
// START INSIDE ANY GAP IN IT.
//
// These tests stage each intermediate directory by hand rather than racing a
// goroutine against Rotate: the windows are two syscalls wide, so a concurrent
// test would pass whether or not the code is right and would prove nothing on
// the run where it happened not to interleave. What is staged is exactly what
// the writer leaves behind at that instant, and what is asserted is the answer
// the control plane's own read gives.

// caFile is one file in a deployment's authority directory.
func caFile(t *testing.T, dir, name string) string {
	t.Helper()

	return filepath.Join(wirecert.CADir(dir), name)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return body
}

// putFile replaces one authority file, keeping the mode wirecert requires.
func putFile(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func removeFile(t *testing.T, path string) {
	t.Helper()

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// stageMinted puts back the freshly minted pair a rotation has NOT yet renamed
// into place.
//
// THE FIXTURE IS THE WHOLE DIRECTORY, NOT THE FILES THE READER HAPPENS TO OPEN.
// Every state staged below is one a rotation is midway through, and at that
// point ca.key.new and ca.crt.new are still sitting there — rotateLocked mints
// them before it touches anything else. Leaving them out makes the fixture a
// state no writer produces, so nothing would catch a reader that started
// consulting them, and the file's claim to stage what the writer leaves behind
// would be false. `which` selects how far the renames have got.
func stageMinted(t *testing.T, dir string, keyToo bool) {
	t.Helper()

	// The new generation is whatever is currently installed as the current pair,
	// which is what these tests then rewind.
	putFile(t, caFile(t, dir, "ca.crt.new"), readFile(t, caFile(t, dir, "ca.crt")), 0o644)

	if keyToo {
		putFile(t, caFile(t, dir, "ca.key.new"), readFile(t, caFile(t, dir, "ca.key")), 0o600)
	}
}

// rotated is a deployment with a completed rotation on disk, plus both
// authorities, which is the starting point every staging below rewinds from.
func rotated(t *testing.T) (dir string, old, fresh *wirecert.CA) {
	t.Helper()

	dir = t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh, err = wirecert.Rotate(dir, rotDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	return dir, old, fresh
}

// trusts reports whether a node certificate from ca verifies against a bundle.
func trusts(t *testing.T, bundle []byte, ca *wirecert.CA) bool {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle) {
		t.Fatal("the trust bundle could not be parsed")
	}

	issued, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	leaf, err := wirecert.LeafOf(issued)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	_, err = leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})

	return err == nil
}

// verifiableBy reports whether the certificate this authority would SERVE
// verifies against the given roots — which is the question a node dialling the
// control plane is asking.
func verifiableBy(t *testing.T, presents *wirecert.CA, roots []byte) bool {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		t.Fatal("the roots could not be parsed")
	}

	bundle, err := presents.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue serving: %v", err)
	}

	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatalf("parse serving: %v", err)
	}

	_, err = leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})

	return err == nil
}

// TestAStartMidwayThroughPublishingThePreviousPairPresentsTheCurrentAuthority
// covers the first window a rotation opens.
//
// Rotate writes ca-previous.crt and then ca-previous.key, and the renames of the
// current pair have not happened yet. So the deployment at that instant is: the
// OLD authority current, its certificate copied aside, and no previous key. The
// old reader took the certificate as proof a rotation was running, went looking
// for the key, and refused to start with "read the previous authority's key".
func TestAStartMidwayThroughPublishingThePreviousPairPresentsTheCurrentAuthority(t *testing.T) {
	t.Parallel()

	dir, old, _ := rotated(t)

	// Rewind to the instant between the two previous-pair writes: the new pair is
	// minted but not renamed, the current pair is still the old one, and the
	// previous key has not been written.
	stageMinted(t, dir, true)
	putFile(t, caFile(t, dir, "ca.key"), readFile(t, caFile(t, dir, "ca-previous.key")), 0o600)
	putFile(t, caFile(t, dir, "ca.crt"), readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)
	removeFile(t, caFile(t, dir, "ca-previous.key"))

	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("a control plane starting while a rotation publishes its previous pair "+
			"refused to start: %v", err)
	}

	if authority.Presents.Fingerprint() != old.Fingerprint() {
		t.Errorf("it would present %s; nothing has been installed yet, so the authority in "+
			"force is still %s", authority.Presents.Fingerprint(), old.Fingerprint())
	}

	if !trusts(t, authority.Trust, old) {
		t.Error("the fleet's own authority is not in the trust bundle, so every node would " +
			"be refused")
	}

	if !verifiableBy(t, authority.Presents, old.CertPEM()) {
		t.Error("a node that has not renewed could not verify what this control plane serves")
	}
}

// TestAStartBetweenTheRenamesRepairsTheCurrentPairFromThePreviousKey covers the
// second window a rotation opens.
//
// Rotate installs the new generation with two renames, ca.key then ca.crt. A
// reader landing between them takes the certificate first, so it sees the NEW
// key beside the OLD certificate — a pair that does not hold together on a
// deployment where nothing is wrong. The old reader refused with "ca.key is not
// the key for ca.crt", which describes a damaged authority and is not one.
func TestAStartBetweenTheRenamesRepairsTheCurrentPairFromThePreviousKey(t *testing.T) {
	t.Parallel()

	dir, old, _ := rotated(t)

	// Rewind ca.crt alone: the new key has been renamed into place, and the
	// certificate that goes with it is still sitting at ca.crt.new, which is
	// exactly what a rotation interrupted between its two renames leaves.
	stageMinted(t, dir, false)
	putFile(t, caFile(t, dir, "ca.crt"), readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)

	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("a control plane starting between a rotation's two renames refused to "+
			"start: %v", err)
	}

	if authority.Issuing.Fingerprint() != old.Fingerprint() {
		t.Errorf("it would issue from %s; the certificate on disk is %s, and that is the "+
			"authority in force until the second rename lands",
			authority.Issuing.Fingerprint(), old.Fingerprint())
	}

	// THE REPAIRED PAIR MUST ACTUALLY SIGN. A certificate parsed beside a key
	// that does not belong to it would satisfy a fingerprint comparison and
	// produce leaves that fail on the node that presented them.
	if !trusts(t, authority.Trust, authority.Issuing) {
		t.Error("a certificate this authority issues does not verify against the bundle it " +
			"hands out, so the key it was repaired with is not that certificate's key")
	}
}

// TestAStartMidwayThroughRetiringNeverPresentsAnUntrustedAuthority is the window
// an earlier analysis did not mention and the only one of the three that is not
// fail-closed.
//
// Retire removes ca-previous.key and then ca-previous.crt. Read as four separate
// walks of the directory, a retire landing between two of them left the control
// plane PRESENTING a certificate signed by the retired authority while trusting
// only the new one: it starts, it looks healthy, and no node can verify it.
func TestAStartMidwayThroughRetiringNeverPresentsAnUntrustedAuthority(t *testing.T) {
	t.Parallel()

	dir, old, fresh := rotated(t)

	removeFile(t, caFile(t, dir, "ca-previous.key"))

	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("a control plane starting midway through a retire refused to start: %v", err)
	}

	// THE ASSERTION IS THE PAIRING, not that no error came back. What must never
	// happen is presenting one authority while trusting a bundle without it.
	if !verifiableBy(t, authority.Presents, authority.Trust) {
		t.Fatal("this control plane would present a certificate its own trust bundle cannot " +
			"verify, so every node would drop out")
	}

	if authority.Presents.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("it would present %s; the previous key is gone, so the only authority it can "+
			"sign with is the current one, %s",
			authority.Presents.Fingerprint(), fresh.Fingerprint())
	}

	// AND THE OLD FLEET IS STILL RECOGNISED. The certificate half of the previous
	// pair is still on disk, and trusting one more authority is always safe.
	if !trusts(t, authority.Trust, old) {
		t.Error("a node that has not renewed is already refused, and the retire that would " +
			"justify that has not finished")
	}
}

// TestADamagedCurrentPairIsStillRefused proves the repair is a repair and not
// tolerance.
//
// It fires on exactly one signature — a certificate whose key is the previous
// authority's — and a mismatch that is not that signature has to keep producing
// the error it always did, because a control plane signing with a key that is
// not its certificate's issues leaves that fail on the node presenting them.
func TestADamagedCurrentPairIsStillRefused(t *testing.T) {
	t.Parallel()

	// A third authority's key, which belongs to neither generation.
	stranger := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(stranger, rotDeployment); err != nil {
		t.Fatalf("create the stranger: %v", err)
	}

	strangerKey := readFile(t, caFile(t, stranger, "ca.key"))

	for _, tc := range []struct {
		name   string
		rotate bool
	}{
		{name: "with no rotation running", rotate: false},
		{name: "with a rotation running", rotate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
				t.Fatalf("create: %v", err)
			}

			if tc.rotate {
				if _, err := wirecert.Rotate(dir, rotDeployment); err != nil {
					t.Fatalf("rotate: %v", err)
				}
			}

			putFile(t, caFile(t, dir, "ca.key"), strangerKey, 0o600)

			_, err := wirecert.LoadServing(dir, rotDeployment)
			if err == nil {
				t.Fatal("a key that belongs to no authority on this host was accepted")
			}

			if !strings.Contains(err.Error(), "not the key for ca.crt") {
				t.Errorf("the diagnostic no longer names the mismatch: %v", err)
			}
		})
	}
}

// TestRetireRefusesWhileThePreviousKeyIsHoldingTheAuthorityUp protects the one
// state where ca-previous.key is not a spare copy.
//
// A rotation interrupted between its two renames leaves the new key beside the
// old certificate, and the only key that matches that certificate is
// ca-previous.key. The deployment keeps working because the reader repairs from
// it; retiring would delete it and leave a control plane that cannot start and
// an authority nothing on the host can rebuild.
func TestRetireRefusesWhileThePreviousKeyIsHoldingTheAuthorityUp(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotated(t)

	stageMinted(t, dir, false)
	putFile(t, caFile(t, dir, "ca.crt"), readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)

	err := wirecert.Retire(dir, rotDeployment)
	if err == nil {
		t.Fatal("retiring removed the only key that matches the certificate on disk")
	}

	if !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("the refusal does not say what it prevents: %v", err)
	}

	// AND NOTHING WAS REMOVED. An error value is the cheapest thing a function
	// produces; what matters is that the files a start depends on are still here.
	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if _, statErr := os.Stat(caFile(t, dir, name)); statErr != nil {
			t.Errorf("%s was removed by a retire that reported a refusal: %v", name, statErr)
		}
	}

	// And the deployment still starts, which is what the refusal preserved.
	if _, err := wirecert.LoadServing(dir, rotDeployment); err != nil {
		t.Errorf("the deployment the refusal protected does not start: %v", err)
	}
}

// TestRetireRefusesWhenItCannotTellWhetherThePreviousKeyIsLoadBearing is the
// third state, and the one that decides whether a private key is deleted.
//
// "Could not tell" is never "no key here" — the rule inspectKey follows one
// credential over. A ca-previous.key billet cannot READ (restored 0644, on a
// filesystem it cannot open) may still be the only key matching ca.crt, and the
// control plane cannot start until somebody fixes the file. Reading the failure
// as "not load-bearing" would turn a chmod away from recoverable into gone.
func TestRetireRefusesWhenItCannotTellWhetherThePreviousKeyIsLoadBearing(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotated(t)

	// The interrupted rotation, and then a previous key billet will not read.
	stageMinted(t, dir, false)
	putFile(t, caFile(t, dir, "ca.crt"), readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)

	prevKey := caFile(t, dir, "ca-previous.key")
	if err := os.Chmod(prevKey, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	err := wirecert.Retire(dir, rotDeployment)
	if err == nil {
		t.Fatal("retiring removed a key billet could not read well enough to judge")
	}

	if !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("the refusal does not say that it could not tell: %v", err)
	}

	if _, statErr := os.Stat(prevKey); statErr != nil {
		t.Errorf("%s was removed anyway: %v", prevKey, statErr)
	}

	// AND THE FILE IS STILL THE KEY, which is what makes the refusal worth
	// making: one chmod recovers the deployment.
	if err := os.Chmod(prevKey, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := wirecert.LoadServing(dir, rotDeployment); err != nil {
		t.Errorf("the deployment the refusal preserved does not start after a chmod: %v", err)
	}
}

// TestRetireRefusesWheneverTheCurrentPairIsNotConclusivelyWhole covers the two
// states an earlier version of the guard let through, both of which delete the
// only key that can sign for the certificate on disk.
//
// The guard used to ask "is something missing" and take an absent current half
// as permission to proceed. But ca.key ABSENT beside a ca.crt that
// ca-previous.key matches is a deployment one `cp` from recovery, and ca.crt
// absent is a directory where billet cannot tell what the previous key belongs
// to and an operator may yet restore the certificate around it. Only a current
// pair that reads, parses AND matches proves the previous key is a spare.
func TestRetireRefusesWheneverTheCurrentPairIsNotConclusivelyWhole(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		wreck  func(t *testing.T, dir string)
		expect string
	}{
		{
			name: "the current key is gone",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				// The old certificate is current again and its key is not there.
				putFile(t, caFile(t, dir, "ca.crt"),
					readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)
				removeFile(t, caFile(t, dir, "ca.key"))
			},
			expect: "cannot start",
		},
		{
			name: "the current certificate is gone",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				removeFile(t, caFile(t, dir, "ca.crt"))
			},
			expect: "cannot read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, _, _ := rotated(t)
			tc.wreck(t, dir)

			err := wirecert.Retire(dir, rotDeployment)
			if err == nil {
				t.Fatal("retiring removed the previous pair while the current one was not whole")
			}

			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("the refusal does not say what it prevents: %v", err)
			}

			// AND NOTHING WAS REMOVED, which is the assertion that matters: an
			// error value is the cheapest thing a function produces, and this one
			// is about a private key still being on disk.
			for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
				if _, statErr := os.Stat(caFile(t, dir, name)); statErr != nil {
					t.Errorf("%s was removed by a retire that reported a refusal: %v",
						name, statErr)
				}
			}
		})
	}
}

// TestRetireRefusesWhenTheCurrentAuthorityBELONGSToSomebodyElse is the
// difference between "this is an authority" and "this is OUR authority".
//
// A coherent CA from another deployment satisfies every structural check: it
// parses, it is a CA, and its key is its key. What it is not is one this control
// plane will start on — parseCA refuses it by subject organization — so a
// directory holding one beside this deployment's previous pair is precisely the
// case where the previous pair is the only authority here that means anything.
// Judging the current pair with parsePair rather than parseCA read that as
// healthy and deleted the other half.
func TestRetireRefusesWhenTheCurrentAuthorityBELONGSToSomebodyElse(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotated(t)

	// A whole, coherent authority minted for a different deployment.
	stranger := t.TempDir()
	if _, err := wirecert.LoadOrCreateCA(stranger, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("create the stranger: %v", err)
	}

	putFile(t, caFile(t, dir, "ca.crt"), readFile(t, caFile(t, stranger, "ca.crt")), 0o644)
	putFile(t, caFile(t, dir, "ca.key"), readFile(t, caFile(t, stranger, "ca.key")), 0o600)

	// The premise: this control plane will not start on what is now current.
	if _, err := wirecert.LoadServing(dir, rotDeployment); err == nil {
		t.Fatal("a foreign authority was accepted as this deployment's, so this test is not " +
			"staging what it claims")
	}

	if err := wirecert.Retire(dir, rotDeployment); err == nil {
		t.Error("retiring deleted this deployment's only authority because a stranger's " +
			"parsed cleanly")
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if _, err := os.Stat(caFile(t, dir, name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

// TestRotateRefusesRatherThanOverwritingALeftOverPreviousKey protects bytes
// nothing else on the host explains.
//
// Publication goes through WriteFileAtomic, which REPLACES its destination —
// unlike the O_EXCL writes it took over from, where an existing file refused the
// write by itself. A ca-previous.key left behind by an abandoned restore or an
// operator's own recovery may be the only copy of an authority key, and a
// rotation that checked only ca-previous.crt would silently write over it.
func TestRotateRefusesRatherThanOverwritingALeftOverPreviousKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A leftover key with no certificate beside it: bytes nothing else explains.
	leftover := []byte("-----BEGIN EC PRIVATE KEY-----\nsomebody's only copy\n")
	putFile(t, caFile(t, dir, "ca-previous.key"), leftover, 0o600)

	_, err := wirecert.Rotate(dir, rotDeployment)
	if err == nil {
		t.Fatal("rotating wrote over a previous key that was already there")
	}

	if got := readFile(t, caFile(t, dir, "ca-previous.key")); !bytes.Equal(got, leftover) {
		t.Errorf("the leftover key was replaced: %q", got)
	}

	// AND IT MUST NOT SEND THE OPERATOR TO A COMMAND THAT DOES NOTHING.
	// `billet ca retire` is gated on the CERTIFICATE and exits 0 saying "no
	// rotation is running" when that is absent, so the first version of this
	// refusal — which said "finish it with `billet ca retire`" for either half —
	// left an operator with two commands and no way forward.
	if strings.Contains(err.Error(), "Finish it with") {
		t.Errorf("the refusal points at `billet ca retire`, which does nothing in this "+
			"state: %v", err)
	}

	if !strings.Contains(err.Error(), "move it aside") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// TestRotateSaysWhetherALeftoverKeyIsACopyOfTheOneStillInstalled answers the
// question that decides what the operator does next.
//
// Whether those bytes matter is something billet can ANSWER rather than
// describe: a leftover byte-identical to ca.key is a copy of an authority that
// is still here. Answered from having read both files, never from reasoning
// about how the state arose — the same rule as inspectKey one credential over,
// where only what was looked at may be claimed.
func TestRotateSaysWhetherALeftoverKeyIsACopyOfTheOneStillInstalled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		isACopy  bool
		expected string
	}{
		{name: "a copy of the installed key", isACopy: true, expected: "byte-identical"},
		{name: "somebody else's key", expected: "NOT a copy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
				t.Fatalf("create: %v", err)
			}

			body := []byte("-----BEGIN EC PRIVATE KEY-----\nnot the one\n")
			if tc.isACopy {
				body = readFile(t, caFile(t, dir, "ca.key"))
			}

			putFile(t, caFile(t, dir, "ca-previous.key"), body, 0o600)

			if _, err := wirecert.Rotate(dir, rotDeployment); err == nil {
				t.Fatal("rotating wrote over the leftover key")
			} else if !strings.Contains(err.Error(), tc.expected) {
				t.Errorf("the refusal does not answer whether the leftover matters: %v", err)
			}
		})
	}
}

// TestRotateRefusesWhenItCannotTellWhetherAPreviousHalfIsThere keeps "could not
// look" out of the answer "nothing is there".
//
// The check in front of publication decides whether WriteFileAtomic is allowed
// to REPLACE a file, so reading a stat failure as "absent" is the same collapse
// inspectKey refuses one credential over — and it lands on a private key. A
// directory billet cannot search answers neither yes nor no, and only "no"
// permits the write.
//
// AN UNSEARCHABLE DIRECTORY STOPS AT THE FIRST OF THE TWO PATHS, the
// certificate, so what this reaches is that one. Both go through the same
// refuseIfPresent, which is where the collapse would be — there is no
// key-specific error handling for a second test to reach. Said out loud because
// the name would otherwise promise both.
func TestRotateRefusesWhenItCannotTellWhetherAPreviousHalfIsThere(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permission this stages")
	}

	dir := t.TempDir()

	if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	caDir := wirecert.CADir(dir)

	// No SEARCH permission, so Lstat on a child fails rather than answering.
	// Restored in cleanup or t.TempDir cannot remove the tree.
	if err := os.Chmod(caDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(caDir, 0o700); err != nil {
			t.Errorf("restore the directory mode: %v", err)
		}
	})

	if _, err := wirecert.Rotate(dir, rotDeployment); err == nil {
		t.Fatal("rotating proceeded past a previous half it could not look at")
	} else if !strings.Contains(err.Error(), "check") {
		t.Errorf("the error does not say it could not tell: %v", err)
	}
}

// TestRetireOnlyEverRemovesAPreviousPairItCanPROVEIsOurs keeps an unaccounted
// private key on disk.
//
// Retire drops the authority a rotation replaced, and it unlinks a private key
// to do it — so the pair it removes has to be one billet can SHOW is that
// authority, not two files sitting under those names. A healthy current pair
// proves the rotation completed and proves nothing whatever about what is under
// the previous names, which is where the earlier version stopped looking.
//
// EVERY CASE HERE HAS A HEALTHY CURRENT PAIR, deliberately: that is the state
// that used to be taken as permission, and it is the one an operator is in when
// they type the command.
func TestRetireOnlyEverRemovesAPreviousPairItCanPROVEIsOurs(t *testing.T) {
	t.Parallel()

	stranger := t.TempDir()
	if _, err := wirecert.LoadOrCreateCA(stranger, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("create the stranger: %v", err)
	}

	for _, tc := range []struct {
		name   string
		wreck  func(t *testing.T, dir string)
		expect string
	}{
		{
			name: "the previous key belongs to nothing here",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				putFile(t, caFile(t, dir, "ca-previous.key"),
					readFile(t, caFile(t, stranger, "ca.key")), 0o600)
			},
			expect: "cannot account for",
		},
		{
			name: "the previous pair is a whole authority of somebody else's",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				putFile(t, caFile(t, dir, "ca-previous.key"),
					readFile(t, caFile(t, stranger, "ca.key")), 0o600)
				putFile(t, caFile(t, dir, "ca-previous.crt"),
					readFile(t, caFile(t, stranger, "ca.crt")), 0o644)
			},
			expect: "cannot account for",
		},
		{
			name: "billet cannot read the previous key",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Chmod(caFile(t, dir, "ca-previous.key"), 0o644); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
			expect: "cannot read",
		},
		{
			name: "a previous key with no certificate at all",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				removeFile(t, caFile(t, dir, "ca-previous.crt"))
			},
			expect: "move it aside",
		},
		{
			// AND "COULD NOT READ IT" IS NOT "IT IS NOT THERE". Collapsing the
			// two told an operator there was no certificate beside their key
			// when there was one billet could not open, after which moving only
			// the key aside leaves rotate blocked by a file nobody mentioned.
			name: "billet cannot read the previous certificate",
			wreck: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Remove(caFile(t, dir, "ca-previous.crt")); err != nil {
					t.Fatalf("remove: %v", err)
				}

				if err := os.Symlink("elsewhere", caFile(t, dir, "ca-previous.crt")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
			expect: "cannot read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, _, _ := rotated(t)
			tc.wreck(t, dir)

			if err := wirecert.Retire(dir, rotDeployment); err == nil {
				t.Fatal("retiring unlinked a previous key billet cannot account for")
			} else if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("the refusal does not say why: %v", err)
			}

			// THE FILES, NOT THE ERROR VALUE. An error is the cheapest thing a
			// function produces and this one is about a private key still being
			// on disk.
			if _, err := os.Stat(caFile(t, dir, "ca-previous.key")); err != nil {
				t.Errorf("the previous key was unlinked anyway: %v", err)
			}
		})
	}
}

// TestRetireStillClearsACertificateWithNoKeyBesideIt is the direction the tests
// above would let a guard that refuses everything pass.
//
// A previous CERTIFICATE with no key beside it is public, it is what makes a
// deployment report an overlap that is not running, and clearing it is exactly
// what an operator runs this for. It arises two ways and BOTH are staged,
// because the first version of this test staged only the second while claiming
// the first — and they differ in the thing a wrong guard would key on. A
// rotation that crashed between publishing the two previous halves leaves the
// OLD authority current and both minted .new files on disk; a retire interrupted
// between its two removals leaves the NEW one current. A guard that (wrongly)
// depended on the current pair being the new authority would pass the second and
// refuse the first.
func TestRetireStillClearsACertificateWithNoKeyBesideIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, dir string, old, fresh *wirecert.CA) *wirecert.CA
	}{
		{
			name: "a rotation that crashed before publishing the previous key",
			stage: func(t *testing.T, dir string, old, _ *wirecert.CA) *wirecert.CA {
				t.Helper()
				// The minted pair is not installed yet and the old authority is
				// still the current one.
				stageMinted(t, dir, true)
				putFile(t, caFile(t, dir, "ca.key"),
					readFile(t, caFile(t, dir, "ca-previous.key")), 0o600)
				putFile(t, caFile(t, dir, "ca.crt"),
					readFile(t, caFile(t, dir, "ca-previous.crt")), 0o644)
				removeFile(t, caFile(t, dir, "ca-previous.key"))

				return old
			},
		},
		{
			name: "a retire interrupted between its two removals",
			stage: func(t *testing.T, dir string, _, fresh *wirecert.CA) *wirecert.CA {
				t.Helper()
				removeFile(t, caFile(t, dir, "ca-previous.key"))

				return fresh
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, old, fresh := rotated(t)
			inForce := tc.stage(t, dir, old, fresh)

			if err := wirecert.Retire(dir, rotDeployment); err != nil {
				t.Fatalf("retiring a leftover certificate was refused: %v", err)
			}

			if _, err := os.Stat(caFile(t, dir, "ca-previous.crt")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the leftover certificate survived: %v", err)
			}

			authority, err := wirecert.LoadServing(dir, rotDeployment)
			if err != nil {
				t.Fatalf("load serving: %v", err)
			}

			if authority.Rotating {
				t.Error("the deployment still reports an overlap after the leftover was cleared")
			}

			if authority.Presents.Fingerprint() != inForce.Fingerprint() {
				t.Errorf("it presents %s rather than %s, which is the authority installed in "+
					"this state", authority.Presents.Fingerprint(), inForce.Fingerprint())
			}
		})
	}
}

// TestRetireRefusesAPreviousFileCarryingMoreThanItAccountsFor keeps material
// hidden in a key file from being unlinked with it.
//
// pem.Decode discards the remainder AND SKIPS FORWARD to the first BEGIN line,
// and a block carries headers of its own — three places to put a second private
// key that a proof ending in an unlink never looked at. The first version of
// this check asked about the remainder only, so of the three shapes below it
// caught one. A deletion may only be authorised by bytes that were ALL
// accounted for.
func TestRetireRefusesAPreviousFileCarryingMoreThanItAccountsFor(t *testing.T) {
	t.Parallel()

	// A whole second authority, whose key is the material being smuggled.
	stranger := t.TempDir()
	if _, err := wirecert.LoadOrCreateCA(stranger, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("create the stranger: %v", err)
	}

	for _, tc := range []struct {
		name string
		hide func(legitimate, extra []byte) []byte
	}{
		{
			name: "appended after the legitimate key",
			hide: func(legitimate, extra []byte) []byte {
				return append(append([]byte(nil), legitimate...), extra...)
			},
		},
		{
			// RAW BYTES RATHER THAN A SECOND PEM BLOCK, and the difference is the
			// whole vector: pem.Decode is SPECIFIED to skip forward to the first
			// BEGIN line, so unarmoured material in front is invisible both to it
			// and to the remainder the old check read. A PEM block here would
			// have been caught, because pem.Decode would return THAT one and
			// leave the legitimate key as the remainder.
			name: "in front of it, where pem.Decode does not look",
			hide: func(legitimate, extra []byte) []byte {
				var out []byte

				out = append(out, []byte("# "+base64.StdEncoding.EncodeToString(extra)+"\n")...)

				return append(out, legitimate...)
			},
		},
		{
			// A block's own headers are a third place, and the DER that comes out
			// of pem.Decode is identical either way. Single-line, because a PEM
			// header value cannot carry a newline.
			name: "in the block's own headers",
			hide: func(legitimate, extra []byte) []byte {
				block, _ := pem.Decode(legitimate)
				if block == nil {
					return legitimate
				}

				block.Headers = map[string]string{
					"X-Billet": base64.StdEncoding.EncodeToString(extra),
				}

				return pem.EncodeToMemory(block)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, _, _ := rotated(t)

			prevKeyPath := caFile(t, dir, "ca-previous.key")
			staged := tc.hide(readFile(t, prevKeyPath), readFile(t, caFile(t, stranger, "ca.key")))
			putFile(t, prevKeyPath, staged, 0o600)

			// THE PREMISE: everything upstream of the guard still accepts it.
			// Without this the test could be about a file that simply broke.
			if _, err := wirecert.LoadServing(dir, rotDeployment); err != nil {
				t.Fatalf("the staged file no longer loads, so this stages nothing: %v", err)
			}

			if err := wirecert.Retire(dir, rotDeployment); err == nil {
				t.Fatal("retiring unlinked a file carrying a second, unaccounted private key")
			} else if !strings.Contains(err.Error(), "more than the one") {
				t.Errorf("the refusal does not say what it found: %v", err)
			}

			if got := readFile(t, prevKeyPath); !bytes.Equal(got, staged) {
				t.Error("the file was modified by a retire that reported a refusal")
			}
		})
	}
}

// TestAnAuthorityThatDidNotSignItselfIsRefused keeps a certificate anybody
// assembled from deciding what billet may delete.
//
// parsePair proved the certificate is a CA and that the key beside it is its
// key. It never proved the certificate is the one that key PRODUCED — so a
// certificate assembled by hand, carrying a real public key and whatever subject
// its author liked, satisfied every check. That matters because decisions are
// made from a certificate's CONTENTS: which generation it says it replaced, and
// therefore whether a private key may be unlinked.
//
// Every leaf verification on the wire already depends on this holding, so
// nothing that could have worked is refused by adding it.
func TestAnAuthorityThatDidNotSignItselfIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	genuine, err := wirecert.LoadOrCreateCA(dir, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	block, _ := pem.Decode(genuine.CertPEM())
	if block == nil {
		t.Fatal("the authority is not PEM")
	}

	template, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// SOMEBODY ELSE'S SIGNATURE OVER THIS DEPLOYMENT'S PUBLIC KEY. Everything a
	// reader looks at — the subject, the public key, IsCA — is the real
	// authority's; only the signature is not.
	forgerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The parent is the real authority's certificate with the FORGER's public
	// key, so the issuer name is the real one and Go will sign with the forger's
	// key. The certificate that comes out looks self-signed and is not.
	parent := *template
	parent.PublicKey = forgerKey.Public()

	forged, err := x509.CreateCertificate(
		rand.Reader, template, &parent, template.PublicKey, forgerKey)
	if err != nil {
		t.Fatalf("forge: %v", err)
	}

	putFile(t, caFile(t, dir, "ca.crt"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: forged}), 0o644)

	_, err = wirecert.LoadServing(dir, rotDeployment)
	if err == nil {
		t.Fatal("an authority signed by somebody else was accepted")
	}

	if !strings.Contains(err.Error(), "did not sign itself") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// TestRetireRefusesASecondAuthorityMintedForTHISDeployment covers the generation
// gap.
//
// The subject proves the DEPLOYMENT, not the GENERATION. A second,
// independently minted CA carrying this deployment id parses, is a CA, and its
// key is its key — so every structural check the guard makes passes and the
// private key is unlinked exactly as the real predecessor's would be. The
// stranger tests beside this one cannot reach it: they use a DIFFERENT
// deployment id, which parseCA already refuses.
//
// What closes it is that a rotation writes the replaced authority's fingerprint
// into the certificate it installs, so the authority in force names the
// generation it took over from.
func TestRetireRefusesASecondAuthorityMintedForTHISDeployment(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotated(t)

	// Another authority for the SAME deployment. Nothing structural tells it
	// from the one this rotation actually replaced.
	twin := t.TempDir()

	twinCA, err := wirecert.LoadOrCreateCA(twin, rotDeployment)
	if err != nil {
		t.Fatalf("mint the twin: %v", err)
	}

	putFile(t, caFile(t, dir, "ca-previous.crt"), readFile(t, caFile(t, twin, "ca.crt")), 0o644)
	putFile(t, caFile(t, dir, "ca-previous.key"), readFile(t, caFile(t, twin, "ca.key")), 0o600)

	// THE PREMISE: everything the guard checked before the generation claim still
	// passes. Without this the test could be about a pair that simply does not
	// hold together.
	if _, err := wirecert.LoadServing(dir, rotDeployment); err != nil {
		t.Fatalf("the twin pair does not load, so this stages something else: %v", err)
	}

	err = wirecert.Retire(dir, rotDeployment)
	if err == nil {
		t.Fatal("retiring unlinked an authority that is not the generation this rotation " +
			"replaced")
	}

	if !strings.Contains(err.Error(), twinCA.Fingerprint()) {
		t.Errorf("the refusal does not name what it found: %v", err)
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if _, statErr := os.Stat(caFile(t, dir, name)); statErr != nil {
			t.Errorf("%s was unlinked anyway: %v", name, statErr)
		}
	}
}

// TestTheGenerationClaimIsMatchedExactlyAndNotApproximately stops a claim that
// merely resembles the previous authority from authorising its key's removal.
//
// SameFingerprint case-folds and trims, because it exists for a value somebody
// read off one console and typed into another. Base64 is case-sensitive, so it
// calls two DIFFERENT fingerprints equal — and here both sides are machine
// generated and neither has been transcribed, so there is nothing to tolerate
// and the predicate authorises unlinking a private key.
//
// AND THIS IS CONSTRUCTIBLE, which an earlier commit message got wrong: it does
// not need two public keys whose fingerprints differ only in case. It needs the
// CLAIM altered and the certificate re-signed with the authority's own key,
// which is a certificate the authority itself vouches for and which the tolerant
// comparator would have accepted.
func TestTheGenerationClaimIsMatchedExactlyAndNotApproximately(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		alter func(claim string) string
	}{
		{name: "the case changed", alter: strings.ToUpper},
		{name: "surrounded by space", alter: func(claim string) string { return " " + claim + " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir, old, _ := rotated(t)

			// THE FINGERPRINT ALTERED, NOT THE PREFIX. Changing the prefix makes
			// it an OU billet did not write, which is correctly no claim at all
			// and would prove something else entirely — the first version of this
			// test uppercased the whole string and passed for that reason.
			current := reissueWithClaim(t, dir, "billet-replaces:"+tc.alter(old.Fingerprint()))
			putFile(t, caFile(t, dir, "ca.crt"), current, 0o644)

			// THE PREMISE: the re-signed certificate is one this deployment
			// accepts. Without that this test would be about a broken authority.
			if _, err := wirecert.LoadServing(dir, rotDeployment); err != nil {
				t.Fatalf("the re-signed authority does not load, so this stages nothing: %v", err)
			}

			if err := wirecert.Retire(dir, rotDeployment); err == nil {
				t.Error("a claim that only resembles the previous authority's fingerprint " +
					"authorised unlinking its key")
			}

			for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
				if _, err := os.Stat(caFile(t, dir, name)); err != nil {
					t.Errorf("%s was unlinked: %v", name, err)
				}
			}
		})
	}
}

// reissueWithClaim re-signs the current authority carrying a different claim.
//
// WITH ITS OWN KEY, so what comes back is a certificate this deployment's
// authority genuinely vouches for — everything except the claim is as billet
// minted it, which is what makes the comparison the only thing under test.
func reissueWithClaim(t *testing.T, dir, claim string) []byte {
	t.Helper()

	block, _ := pem.Decode(readFile(t, caFile(t, dir, "ca.crt")))
	if block == nil {
		t.Fatal("the current authority is not PEM")
	}

	template, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	keyBlock, _ := pem.Decode(readFile(t, caFile(t, dir, "ca.key")))
	if keyBlock == nil {
		t.Fatal("the current key is not PEM")
	}

	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse the key: %v", err)
	}

	template.Subject.OrganizationalUnit = []string{claim}
	// CLEARED, or CreateCertificate reuses the parsed bytes and the new subject
	// never reaches the certificate — which would make this test pass against
	// anything at all.
	template.RawSubject = nil
	template.RawIssuer = nil

	reissued, err := x509.CreateCertificate(
		rand.Reader, template, template, template.PublicKey, key)
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: reissued})
}

// TestRetireStillFinishesARotationThatPredatesTheGenerationClaim is the
// direction that decides whether the generation claim is shippable at all.
//
// An authority minted by a billet older than the claim carries none, and so does
// the first authority any deployment ever had. Refusing there would leave an
// operator mid-rotation with no command that can finish it — on every deployment
// that rotated before this shipped. So a certificate that claims nothing falls
// back to the checks that were there before, and the next rotation records one.
//
// STAGED AS THE SAME DIRECTORY AS THE TEST ABOVE minus the claim, so the only
// difference between refusing and proceeding is the fact under test.
func TestRetireStillFinishesARotationThatPredatesTheGenerationClaim(t *testing.T) {
	t.Parallel()

	// IN THE ORDER A ROTATION REALLY HAPPENS: the predecessor existed first and
	// the current authority was minted afterwards. Nothing in the code reads a
	// certificate's dates, so staging them the other way round passes — and a
	// fixture that could not arise is not the compatibility claim this test is
	// making.
	older := t.TempDir()
	if _, err := wirecert.LoadOrCreateCA(older, rotDeployment); err != nil {
		t.Fatalf("mint the predecessor: %v", err)
	}

	dir := t.TempDir()

	// The authority that took over, carrying no claim — which is what a billet
	// older than the claim leaves behind.
	if _, err := wirecert.LoadOrCreateCA(dir, rotDeployment); err != nil {
		t.Fatalf("create: %v", err)
	}

	putFile(t, caFile(t, dir, "ca-previous.crt"), readFile(t, caFile(t, older, "ca.crt")), 0o644)
	putFile(t, caFile(t, dir, "ca-previous.key"), readFile(t, caFile(t, older, "ca.key")), 0o600)

	if err := wirecert.Retire(dir, rotDeployment); err != nil {
		t.Fatalf("an overlap left by an older billet cannot be finished: %v", err)
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if _, err := os.Stat(caFile(t, dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the retire: %v", name, err)
		}
	}
}

// TestARotationRecordsTheAuthorityItReplaced is the writer's half.
//
// The whole proof rests on the new certificate naming its predecessor, so a
// rotation that quietly stopped recording it would leave every later retire
// falling back to the checks the generation claim exists to strengthen —
// silently, because the fallback is a legitimate path.
func TestARotationRecordsTheAuthorityItReplaced(t *testing.T) {
	t.Parallel()

	dir, old, fresh := rotated(t)

	block, _ := pem.Decode(fresh.CertPEM())
	if block == nil {
		t.Fatal("the new authority is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the new authority: %v", err)
	}

	want := "billet-replaces:" + old.Fingerprint()

	if !slices.Contains(cert.Subject.OrganizationalUnit, want) {
		t.Errorf("the new authority names %v as what it replaced; it took over from %s",
			cert.Subject.OrganizationalUnit, old.Fingerprint())
	}

	// AND A FIRST AUTHORITY CLAIMS NOTHING, or every deployment's day one would
	// name a predecessor that never existed.
	first := t.TempDir()

	firstCA, err := wirecert.LoadOrCreateCA(first, rotDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	firstBlock, _ := pem.Decode(firstCA.CertPEM())
	if firstBlock == nil {
		t.Fatal("the first authority is not PEM")
	}

	firstCert, err := x509.ParseCertificate(firstBlock.Bytes)
	if err != nil {
		t.Fatalf("parse the first authority: %v", err)
	}

	for _, ou := range firstCert.Subject.OrganizationalUnit {
		if strings.HasPrefix(ou, "billet-replaces:") {
			t.Errorf("the first authority a deployment ever had claims to replace %q", ou)
		}
	}

	// And the claim does not disturb what the certificate is FOR: both
	// generations still issue node certificates the other side accepts.
	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if !trusts(t, authority.Trust, fresh) {
		t.Error("a certificate from the new authority no longer verifies against the bundle")
	}
}

// TestRetiringACompleteRotationIsNotRefused is the other direction of that
// guard: a retire that refused every deployment would pass the test above.
func TestRetiringACompleteRotationIsNotRefused(t *testing.T) {
	t.Parallel()

	dir, _, fresh := rotated(t)

	if err := wirecert.Retire(dir, rotDeployment); err != nil {
		t.Fatalf("retiring a completed rotation was refused: %v", err)
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if _, err := os.Stat(caFile(t, dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the retire: %v", name, err)
		}
	}

	authority, err := wirecert.LoadServing(dir, rotDeployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	if authority.Presents.Fingerprint() != fresh.Fingerprint() {
		t.Errorf("after retiring, the server presents %s rather than %s",
			authority.Presents.Fingerprint(), fresh.Fingerprint())
	}
}
