package runnerimages_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/runnerimages"
)

func TestThePinnedFileNamesACommitAndADigest(t *testing.T) {
	t.Parallel()

	commit := runnerimages.PinnedCommit()
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Errorf("PinnedCommit() = %q, want a 40-character git object id; provenance is the "+
			"only record of which upstream revision the vendored toolset is", commit)
	}

	sum := runnerimages.PinnedSHA256()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sum) {
		t.Errorf("PinnedSHA256() = %q, want a hex sha256", sum)
	}
}

// TestTheVendoredToolsetMatchesItsPin is the check Load performs, asserted directly.
//
// IT IS THE REASON Load RETURNS AN ERROR AT ALL. This file decides what goes into an
// image that runs other people's CI, so an edit to it without an edit to pinned.txt
// must stop the build rather than quietly change every image built afterwards.
func TestTheVendoredToolsetMatchesItsPin(t *testing.T) {
	t.Parallel()

	if _, err := runnerimages.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// TestVerifyToolsetRefusesBytesThatAreNotThePinnedOnes drives the PRODUCTION
// guard with wrong input, which is the only way to know it works.
//
// THE FIRST VERSION OF THIS TEST COULD NOT FAIL. It hashed the embedded file
// itself and compared against the pin, then called Load — so deleting the
// comparison inside Load left it green, and corrupting the pin proved only that
// the test's own duplicate comparison worked. That is the shape billet-testing
// calls a test about the adjacent thing: it was ABOUT the digest guard and
// EXERCISED a copy of it.
func TestVerifyToolsetRefusesBytesThatAreNotThePinnedOnes(t *testing.T) {
	t.Parallel()

	good := runnerimages.ToolsetBytes()
	pin := runnerimages.PinnedSHA256()

	if err := runnerimages.VerifyToolset(good, pin); err != nil {
		t.Fatalf("the vendored bytes were refused against their own pin: %v", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"one byte changed", append(append([]byte{}, good[:len(good)-1]...), 'X')},
		{"truncated", good[:len(good)/2]},
		{"empty", nil},
		{"something else entirely", []byte(`{"apt":{"vital_packages":["curl"]}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := runnerimages.VerifyToolset(tc.data, pin)
			if err == nil {
				t.Fatal("bytes that are not the pinned toolset were accepted, so nothing " +
					"stops an edited manifest from deciding what every image contains")
			}

			if !strings.Contains(err.Error(), "pinned.txt") {
				t.Errorf("refused with %q, which does not tell an operator which file to "+
					"refresh", err)
			}
		})
	}
}

// TestLoadDoesNotShareItsSlices: a cached Toolset returned by value still hands
// out the package's own slices, so one caller changing an apt package would
// change it for every later caller — and the digest check, which runs over the
// bytes rather than the parsed value, would never notice.
func TestLoadDoesNotShareItsSlices(t *testing.T) {
	t.Parallel()

	first, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(first.Apt.VitalPackages) == 0 {
		t.Fatal("the toolset carries no vital packages")
	}

	original := first.Apt.VitalPackages[0]
	first.Apt.VitalPackages[0] = "rm-rf-slash"

	second, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if second.Apt.VitalPackages[0] != original {
		t.Errorf("a caller's mutation reached a later Load: got %q, want %q. The verified "+
			"toolset is shared mutable state, and the digest check cannot see a change made "+
			"after parsing", second.Apt.VitalPackages[0], original)
	}
}

func TestToolsetBytesCannotBeMutatedThroughItsCaller(t *testing.T) {
	t.Parallel()

	first := runnerimages.ToolsetBytes()
	if len(first) == 0 {
		t.Fatal("ToolsetBytes returned nothing")
	}

	first[0] = 'X'

	second := runnerimages.ToolsetBytes()
	if second[0] == 'X' {
		t.Error("ToolsetBytes handed out the package's own slice, so a caller can change what " +
			"every later digest check hashes — which makes the integrity check agree with " +
			"tampering instead of catching it")
	}
}

// TestTheAptSetCarriesWhatBilletWasMissing is the parity claim, made checkable.
//
// EVERY ONE OF THESE IS A REAL FAILURE CLASS, not a sample. billet's guest image
// installed about twenty-five packages against GitHub's ~57, and the gap is why a
// workflow that clones over ssh, verifies a signature, or reads a timezone works on
// a hosted runner and fails here.
func TestTheAptSetCarriesWhatBilletWasMissing(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := ts.AptPackages()

	for _, want := range []string{
		"openssh-client", // git clone over ssh
		"gnupg2",         // commit and package signature verification
		"locales",        // a job that sets LANG and gets a warning-shaped failure
		"tzdata",         // anything asserting a local time
		"sqlite3",
		"shellcheck",
		"pkg-config", // every native build that probes for a library
		"libssl-dev", // the same, for TLS
		"file",       // used by more scripts than anyone expects
		"xz-utils",   // tarballs the setup-* family fetches
		"zip",        // upload-artifact's own path on some workflows
		"bzip2",      // upstream calls this one vital
		"patchelf",   // python wheels that repair their own rpaths
		"aria2",      // parallel downloads in cache-warming steps
		"p7zip-full", // 7z archives
		"net-tools",  // scripts still reach for ifconfig and netstat
		"iputils-ping",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the apt set does not carry %q; a workflow relying on it fails on billet "+
				"and works on a hosted runner, which is the whole gap this closes", want)
		}
	}
}

// TestAptPackagesKeepsUpstreamOrderAndInstallsEachPackageOnce covers both properties
// in one place because they are the same loop.
//
// ORDER: upstream installs vital, then common, then cmd, and reordering them changes
// which package resolves a shared dependency first.
// DUPLICATES: rsync and sudo are in more than one upstream list, so a naive
// concatenation emits them twice.
func TestAptPackagesKeepsUpstreamOrderAndInstallsEachPackageOnce(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := ts.AptPackages()

	seen := make(map[string]int, len(got))
	for i, pkg := range got {
		if first, dup := seen[pkg]; dup {
			t.Errorf("%q appears at positions %d and %d; apt would be asked to install it "+
				"twice and a diff of this list reads as though something changed",
				pkg, first, i)
		}

		seen[pkg] = i
	}

	// THE FIRST OF EACH GROUP, IN GROUP ORDER. Asserting the whole list would make
	// this test a copy of the data rather than a statement about it.
	vitalFirst, okVital := seen[ts.Apt.VitalPackages[0]]
	commonFirst, okCommon := seen[ts.Apt.CommonPackages[0]]
	cmdFirst, okCmd := seen[ts.Apt.CmdPackages[0]]

	switch {
	case !okVital || !okCommon || !okCmd:
		t.Fatal("a group's first package is missing from AptPackages entirely")
	case vitalFirst >= commonFirst || commonFirst >= cmdFirst:
		t.Errorf("groups are emitted out of upstream order: vital at %d, common at %d, cmd "+
			"at %d; upstream installs vital first and the order decides which package "+
			"resolves a shared dependency", vitalFirst, commonFirst, cmdFirst)
	}

	if len(got) < len(ts.Apt.VitalPackages) {
		t.Errorf("AptPackages returned %d packages, fewer than the %d vital ones alone",
			len(got), len(ts.Apt.VitalPackages))
	}
}

func TestJavaHomeVarsNameEveryJDKAndADefault(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	vars := ts.JavaHomeVars("/usr/lib/jvm")

	for _, v := range ts.Java.Versions {
		name := "JAVA_HOME_" + v + "_X64"

		path, ok := vars[name]
		switch {
		case !ok:
			t.Errorf("%s is unset; setup-java reads it to find a JDK already on the machine, "+
				"so the JDK is installed and unfindable", name)
		case !strings.HasPrefix(path, "/usr/lib/jvm/"):
			t.Errorf("%s = %q, which is not under the root it was given", name, path)
		}
	}

	def, ok := vars["JAVA_HOME"]
	switch {
	case !ok:
		t.Error("JAVA_HOME is unset")
	case !strings.Contains(def, "-"+ts.Java.Default+"-"):
		t.Errorf("JAVA_HOME = %q, which does not name the default JDK %q",
			def, ts.Java.Default)
	}
}

// TestTheVendoredToolsetIsUpstreamAtThePinnedCommit is the provenance check, and it
// is the only one that can catch a vendored copy that was transcribed rather than
// downloaded.
//
// NETWORK-GATED, NOT SKIPPED BY DEFAULT WHERE IT MATTERS. `make check` runs offline
// on plenty of machines, so a hard failure there would make an ordinary local gate
// depend on egress. CI has egress, so this runs there — which is where a wrong pin
// must be caught, because everything downstream trusts this file to be what GitHub
// published.
func TestTheVendoredToolsetIsUpstreamAtThePinnedCommit(t *testing.T) {
	t.Parallel()

	if os.Getenv("BILLET_OFFLINE") != "" {
		t.Skip("BILLET_OFFLINE is set")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		runnerimages.SourceURL(), http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		// A MACHINE WITH NO EGRESS IS NOT A FAILING PIN. Reporting one as the other
		// is how a check everybody learns to ignore gets made. Only a transport
		// failure reaches here; a reachable upstream that disagrees fails below.
		t.Skipf("could not reach upstream: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upstream answered %s for %s; the pinned commit may not exist",
			resp.Status, runnerimages.SourceURL())
	}

	upstream, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("read upstream: %v", err)
	}

	sum := sha256.Sum256(upstream)
	if got := hex.EncodeToString(sum[:]); got != runnerimages.PinnedSHA256() {
		t.Errorf("upstream at %s hashes to %s and the vendored copy to %s.\n"+
			"The vendored toolset is NOT byte-for-byte what GitHub published at that "+
			"commit. Re-download it:\n  curl -fsSL %s -o internal/runnerimages/toolset-2404.json\n"+
			"then put its sha256 in pinned.txt beside the same commit.",
			runnerimages.PinnedCommit(), got, runnerimages.PinnedSHA256(),
			runnerimages.SourceURL())
	}
}
