package wirecert

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A SNAPSHOT IS TWO AGREEING READS OF THE PUBLIC FILES, AND NOTHING WAS
// CREATED: the observer of a deployment's authority must never leave a lock, a
// directory or a marker behind, and a file that changes under it is
// could-not-tell rather than whichever read came last.
func TestSnapshotAuthorityReadsWithoutCreatingAndConfirms(t *testing.T) {
	stateDir := t.TempDir()
	ca, err := LoadOrCreateCA(stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	// The lock file a locking reader would create must not exist afterwards.
	lockPath := AuthorityLockPath(stateDir)
	_ = os.Remove(lockPath)
	before := listDir(t, CADir(stateDir))

	snap, err := SnapshotAuthority(stateDir)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !bytes.Equal(snap.CurrentPEM, ca.CertPEM()) {
		t.Error("the snapshot's current certificate is not the CA's")
	}
	if snap.Current == nil || FingerprintOfCert(snap.Current) != ca.Fingerprint() {
		t.Error("the parsed current certificate does not fingerprint as the CA")
	}
	if snap.Rotating() || snap.Previous != nil {
		t.Error("a deployment that never rotated reports a predecessor")
	}
	if !snap.Created {
		t.Error("the creation marker LoadOrCreateCA wrote was not seen")
	}
	if after := listDir(t, CADir(stateDir)); after != before {
		t.Errorf("the snapshot changed the CA directory:\n before %q\n after  %q", before, after)
	}
	if _, err := os.Lstat(lockPath); err == nil {
		t.Error("the snapshot created ca.lock")
	}
}

func TestSnapshotAuthorityReportsAPredecessorAsRotating(t *testing.T) {
	stateDir := t.TempDir()
	ca, err := LoadOrCreateCA(stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	// Any certificate stands in for the predecessor: the snapshot parses and
	// reports, it does not judge the relationship between the two.
	if err := os.WriteFile(AuthorityPath(stateDir, "ca-previous.crt"), ca.CertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotAuthority(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Rotating() || snap.Previous == nil || !bytes.Equal(snap.PreviousPEM, ca.CertPEM()) {
		t.Error("a present ca-previous.crt was not reported as a rotation in progress")
	}
}

func TestSnapshotAuthorityIsUnknownWhileTheFilesChange(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := LoadOrCreateCA(stateDir, "dep-1234"); err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateCA(t.TempDir(), "dep-5678")
	if err != nil {
		t.Fatal(err)
	}
	flip := false
	prev := snapshotBetweenReads
	t.Cleanup(func() { snapshotBetweenReads = prev })
	// Every attempt sees a different certificate on its second read.
	snapshotBetweenReads = func() {
		var body []byte
		if flip {
			body = other.CertPEM()
		} else {
			body = append([]byte("# changed\n"), other.CertPEM()...)
		}
		flip = !flip
		if err := os.WriteFile(AuthorityPath(stateDir, "ca.crt"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SnapshotAuthority(stateDir); !errors.Is(err, ErrAuthorityChanging) {
		t.Fatalf("a changing authority answered %v, want ErrAuthorityChanging", err)
	}
}

// ONE DISTURBED ATTEMPT IS NOT A FAILURE: the second attempt's two reads agree.
func TestSnapshotAuthorityRetriesOnceDisturbed(t *testing.T) {
	stateDir := t.TempDir()
	ca, err := LoadOrCreateCA(stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateCA(t.TempDir(), "dep-5678")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	prev := snapshotBetweenReads
	t.Cleanup(func() { snapshotBetweenReads = prev })
	snapshotBetweenReads = func() {
		calls++
		if calls == 1 {
			if err := os.WriteFile(AuthorityPath(stateDir, "ca.crt"), other.CertPEM(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err := SnapshotAuthority(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snap.CurrentPEM, other.CertPEM()) {
		t.Error("the settled snapshot is not the certificate the files hold now")
	}
	if bytes.Equal(snap.CurrentPEM, ca.CertPEM()) {
		t.Error("the snapshot kept the first read after the files changed under it")
	}
}

func TestSnapshotAuthorityOnAnEmptyStateDirIsLostAndCreatesNothing(t *testing.T) {
	stateDir := t.TempDir()
	_, err := SnapshotAuthority(stateDir)
	if !errors.Is(err, ErrAuthorityLost) {
		t.Fatalf("an empty state directory answered %v, want ErrAuthorityLost", err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the snapshot created %d entries in an empty state directory", len(entries))
	}
}

// EXACTLY ONE CERTIFICATE: a ca.crt with a second block, or with key material
// appended, is refused rather than read for its first block, because the
// report publishes what the snapshot returns.
func TestSnapshotAuthorityRefusesACertificateFileWithMoreInIt(t *testing.T) {
	for name, extra := range map[string][]byte{
		"second certificate": nil,
		"key material":       []byte("-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"),
		"trailing bytes":     []byte("not pem\n"),
		"leading bytes":      []byte("PREFIX"),
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			ca, err := LoadOrCreateCA(stateDir, "dep-1234")
			if err != nil {
				t.Fatal(err)
			}
			body := append([]byte(nil), ca.CertPEM()...)
			switch {
			case extra == nil:
				body = append(body, ca.CertPEM()...)
			case name == "leading bytes":
				body = append(append([]byte(nil), extra...), body...)
			default:
				body = append(body, extra...)
			}
			if err := os.WriteFile(AuthorityPath(stateDir, "ca.crt"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := SnapshotAuthority(stateDir); err == nil {
				t.Fatal("a ca.crt with more than one certificate in it was accepted")
			}
		})
	}
}

func TestSnapshotAuthorityRefusesASymlinkedCertificate(t *testing.T) {
	stateDir := t.TempDir()
	ca, err := LoadOrCreateCA(stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.crt")
	if err := os.WriteFile(target, ca.CertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	path := AuthorityPath(stateDir, "ca.crt")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotAuthority(stateDir); err == nil {
		t.Fatal("a symlinked ca.crt was read")
	}
}

func TestParseCertificatesReadsEveryBlockAndRefusesOtherKinds(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir(), "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(append([]byte(nil), ca.CertPEM()...), ca.CertPEM()...)
	certs, err := ParseCertificates(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(certs) != 2 {
		t.Errorf("parsed %d certificates from a two-certificate bundle", len(certs))
	}
	if _, err := ParseCertificates([]byte("nothing here")); err == nil {
		t.Error("a bundle with no certificate was accepted")
	}
	if _, err := ParseCertificates(append(append([]byte(nil), ca.CertPEM()...), []byte("trailing garbage\n")...)); err == nil {
		t.Error("a bundle with bytes after its last block was accepted")
	}
	between := append(append(append([]byte(nil), ca.CertPEM()...), []byte("between\n")...), ca.CertPEM()...)
	if _, err := ParseCertificates(between); err == nil {
		t.Error("a bundle with bytes between its blocks was accepted")
	}
	if _, err := ParseCertificates(append([]byte("prefix "), ca.CertPEM()...)); err == nil {
		t.Error("a bundle with bytes before its first block was accepted")
	}
	node, err := ca.IssueNode("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCertificates(node.KeyPEM); err == nil {
		t.Error("a key block was accepted as a certificate")
	}
}

func listDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return filepath.Join(names...)
}
