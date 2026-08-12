package wirecert

import (
	"os"
	"path/filepath"
	"testing"
)

const recoverDeployment = "0123456789abcdef0123456789abcdef"

// AN INTERRUPTED RENEWAL LEAVES A NODE THAT STILL STARTS.
//
// A generation is three files and three renames, and no amount of care makes
// that one operation. Between any two of them the process can die, and what is
// left is a new key beside an old certificate — a pair that verifies as nothing.
// The node cannot start, and cannot renew its way out either: renewal is
// authenticated by the certificate being renewed, and the key that certificate
// belonged to has already been overwritten. That machine has to be enrolled
// again by hand, which is exactly what renewal exists to avoid.
//
// WHITE-BOX ON PURPOSE. The interruption has to happen INSIDE Replace, between
// two of its writes, and no exported surface can be stopped there. Driving the
// same calls in the same order is what makes the staged state the real one
// rather than an approximation of it — a test that wrote the files itself would
// skip savePrevious and prove nothing.
func TestAnInterruptedRenewalStillLeavesALoadableIdentity(t *testing.T) {
	t.Parallel()

	// Where Replace can die: after each of the three writes it performs, in the
	// order it performs them.
	for _, upTo := range []int{1, 2} {
		t.Run(map[int]string{1: "after the authority", 2: "after the key"}[upTo], func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			certPath := filepath.Join(dir, "node.crt")
			keyPath := filepath.Join(dir, "node.key")
			caPath := filepath.Join(dir, "ca.crt")

			ca, err := LoadOrCreateCA(t.TempDir(), recoverDeployment)
			if err != nil {
				t.Fatalf("authority: %v", err)
			}

			first, err := ca.IssueNode("epyc-1")
			if err != nil {
				t.Fatalf("issue: %v", err)
			}

			for path, body := range map[string][]byte{
				certPath: first.CertPEM, keyPath: first.KeyPEM, caPath: first.CAPEM,
			} {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatalf("seed %s: %v", path, err)
				}
			}

			// A renewal from a DIFFERENT authority, so a half-installed generation
			// cannot accidentally verify against what was already on disk.
			newCA, err := LoadOrCreateCA(t.TempDir(), recoverDeployment)
			if err != nil {
				t.Fatalf("new authority: %v", err)
			}

			second, err := newCA.IssueNode("epyc-1")
			if err != nil {
				t.Fatalf("issue the renewal: %v", err)
			}

			// Exactly what Replace does, stopped partway.
			if err := savePrevious(keyPath, certPath, caPath); err != nil {
				t.Fatalf("save the previous generation: %v", err)
			}

			writes := []struct {
				path string
				body []byte
				mode os.FileMode
			}{
				{caPath, second.CAPEM, 0o644},
				{keyPath, second.KeyPEM, 0o600},
				{certPath, second.CertPEM, 0o644},
			}

			for i := range upTo {
				if err := writeAtomic(writes[i].path, writes[i].body, writes[i].mode); err != nil {
					t.Fatalf("write %s: %v", writes[i].path, err)
				}
			}

			// What the node does on its next start.
			r, err := NewRotating(certPath, keyPath, caPath)
			if err != nil {
				t.Fatalf("a renewal interrupted %s left a node that cannot start and cannot "+
					"renew its way out: %v", t.Name(), err)
			}

			if !r.RolledBack() {
				t.Error("the identity loaded without reporting that it came from the " +
					"superseded generation, so an operator is never told a renewal was " +
					"interrupted")
			}

			// AND THE WRECKAGE IS CLEARED, so the next renewal replaces a complete
			// generation rather than the remains of this one.
			if _, err := NewRotating(certPath, keyPath, caPath); err != nil {
				t.Fatalf("the recovered generation was not put back, so the next start fails "+
					"too: %v", err)
			}
		})
	}
}

// A COMPLETE RENEWAL KEEPS NOTHING BEHIND. The predecessor exists to survive an
// interruption; once the successor loads it is a stale copy of a private key.
func TestACompleteRenewalClearsThePreviousGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	caPath := filepath.Join(dir, "ca.crt")

	ca, err := LoadOrCreateCA(t.TempDir(), recoverDeployment)
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	first, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	for path, body := range map[string][]byte{
		certPath: first.CertPEM, keyPath: first.KeyPEM, caPath: first.CAPEM,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	r, err := NewRotating(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("rotating: %v", err)
	}

	renewed, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	if err := r.Replace(renewed.CertPEM, renewed.KeyPEM, renewed.CAPEM); err != nil {
		t.Fatalf("replace: %v", err)
	}

	for _, path := range []string{keyPath, certPath, caPath} {
		if _, err := os.Stat(path + prevSuffix); !os.IsNotExist(err) {
			t.Errorf("%s survived a renewal that completed", path+prevSuffix)
		}
	}
}

// A RENEWAL THAT FAILS PARTWAY LEAVES THE PREDECESSOR BEHIND, which is the
// whole point of keeping one: the recovery above can only work if Replace
// actually saved something before it started overwriting.
//
// The failure is staged by taking the authority's directory away after the
// identity has loaded, so the write of the new ca.crt cannot land. Everything
// before that has already run, which is exactly the window the predecessor
// exists for.
func TestAFailedRenewalKeepsThePreviousGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certPath := filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")

	// The authority lives one level down, so it can be taken away without
	// disturbing the pair beside it.
	caDir := filepath.Join(dir, "authority")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	caPath := filepath.Join(caDir, "ca.crt")

	ca, err := LoadOrCreateCA(t.TempDir(), recoverDeployment)
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	first, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	for path, body := range map[string][]byte{
		certPath: first.CertPEM, keyPath: first.KeyPEM, caPath: first.CAPEM,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	r, err := NewRotating(certPath, keyPath, caPath)
	if err != nil {
		t.Fatalf("rotating: %v", err)
	}

	renewed, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("renew: %v", err)
	}

	if err := os.RemoveAll(caDir); err != nil {
		t.Fatalf("remove the authority directory: %v", err)
	}

	if err := r.Replace(renewed.CertPEM, renewed.KeyPEM, renewed.CAPEM); err == nil {
		t.Fatal("a renewal that could not write its authority reported success")
	}

	for _, path := range []string{keyPath, certPath} {
		if _, err := os.Stat(path + prevSuffix); err != nil {
			t.Errorf("%s was not kept, so an interruption here would have left nothing to "+
				"recover from: %v", path+prevSuffix, err)
		}
	}
}
