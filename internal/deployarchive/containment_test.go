package deployarchive

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTheStateDirectoryContainmentTestHasNoEdges exercises the arithmetic
// directly, for the inputs a realistic Target cannot stage.
//
// THE ROOT IS THE ONE THAT MATTERS AND IT IS WHY THIS TEST EXISTS. The first
// version compared `strings.HasPrefix(key, dir+separator)`, which for a state
// directory of "/" builds the prefix "//" — so every path on the machine read
// as OUTSIDE the state directory, including `/ca/ca.key.new`, which a rotation
// deletes. Config validation asks only that state_dir be non-empty, so "/" is
// reachable, and the consequence is the credential GitHub will not reissue.
//
// The table drives the predicate rather than PlanRestore because a Target
// rooted at "/" cannot be built in a test without writing to the real root.
func TestTheStateDirectoryContainmentTestHasNoEdges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		dir    string
		inside bool
	}{
		{"at the root", "/ca/ca.key.new", "/", true},
		{"the root itself", "/", "/", true},
		{"a sibling of the state dir", "/var/lib/app-private-key.pem", "/var/lib/billet", false},
		{"inside the state dir", "/var/lib/billet/ca/ca.key.new", "/var/lib/billet", true},
		{"the state dir itself", "/var/lib/billet", "/var/lib/billet", true},
		// A PREFIX IS NOT CONTAINMENT: this shares every character of the
		// directory's name and is a different directory.
		{"a name the state dir is a prefix of", "/var/lib/billet2/key.pem", "/var/lib/billet", false},
		{"reached back through a parent", "/var/lib/billet/ca/../billet.db", "/var/lib/billet", true},
		{"walking out and not back", "/var/lib/billet/../elsewhere/key.pem", "/var/lib/billet", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := appKeyInsideStateDir(tc.key, tc.dir)
			if err != nil {
				t.Fatalf("appKeyInsideStateDir(%s, %s): %v", tc.key, tc.dir, err)
			}

			if got != tc.inside {
				t.Errorf("appKeyInsideStateDir(%s, %s) = %v, want %v",
					tc.key, tc.dir, got, tc.inside)
			}
		})
	}
}

// TestContainmentAsksTheFilesystemWhenTheStringsDisagree proves the identity
// walk decides where the string comparison cannot.
//
// THE LEXICAL ANSWER IS NOT ENOUGH ON A CASE-INSENSITIVE FILESYSTEM, which is
// the macOS default and a platform billet supports as a first-class target.
// `/…/STATE/key.pem` and `/…/state` name the same directory to the kernel and
// different strings to Go, and EvalSymlinks preserves the caller's spelling of
// ordinary components — so canonicalising does not reconcile them. os.SameFile
// asks the filesystem instead.
//
// THE PLATFORM SPLIT IS STATED RATHER THAN SKIPPED. On a case-sensitive
// filesystem the differently-cased path is a different directory and the honest
// answer is "not inside"; this asserts that too, so the test is meaningful
// wherever it runs rather than quietly proving nothing on half the fleet.
func TestContainmentAsksTheFilesystemWhenTheStringsDisagree(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the state dir: %v", err)
	}

	shouted := filepath.Join(root, "STATE")

	// The probe IS the platform question: does the other spelling reach the
	// same directory?
	insensitive := false

	if info, err := os.Stat(shouted); err == nil {
		want, statErr := os.Stat(dir)
		if statErr != nil {
			t.Fatalf("stat the state dir: %v", statErr)
		}

		insensitive = os.SameFile(want, info)
	}

	got, err := appKeyInsideStateDir(filepath.Join(shouted, "app-private-key.pem"), dir)
	if err != nil {
		t.Fatalf("appKeyInsideStateDir: %v", err)
	}

	if got != insensitive {
		t.Errorf("on a filesystem where %s and %s are the same directory = %v, "+
			"appKeyInsideStateDir answered %v", shouted, dir, insensitive, got)
	}

	if insensitive {
		t.Logf("this filesystem is case-insensitive, so the identity walk was exercised")
	} else {
		t.Logf("this filesystem is case-sensitive, so %s really is a different directory; "+
			"the identity walk is exercised on macOS, where it is load-bearing", shouted)
	}
}
