package host

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provenance"
)

func TestCheckSaysWhetherAManifestProvesTheInstalledRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release string
		digest  string
		err     error
		want    string
	}{
		{"a record of these bytes", "v0.16.9", strings.Repeat("ab", 32), nil, "manifest abababababab produced this v0.16.9 binary\n"},
		{"no record", "v0.16.9", "", provenance.ErrNoRecord, "nothing here records which release manifest produced this v0.16.9 binary"},
		{"a record of other bytes", "v0.16.9", "", provenance.ErrNotThisBinary, "THE RECORD NAMES OTHER BYTES THAN THIS BINARY"},
		{"an unreadable record", "v0.16.9", "", errors.New("provenance: installed.json could not be read"), "could not tell which release manifest produced this binary: provenance: installed.json could not be read"},
		{"a build of a commit", "v0.0.0-dev", "", provenance.ErrNoRecord, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			old := installedProvenanceDigest
			installedProvenanceDigest = func() (string, error) {
				asked = true

				return tc.digest, tc.err
			}
			t.Cleanup(func() { installedProvenanceDigest = old })

			var out bytes.Buffer
			reportInstalledProvenance(&out, tc.release)

			if tc.want == "" {
				if out.Len() != 0 || asked {
					t.Errorf("a build no manifest names was reported on (asked=%v): %q", asked, out.String())
				}

				return
			}

			if !strings.HasPrefix(out.String(), "manifest ") || !strings.Contains(out.String(), tc.want) {
				t.Errorf("got %q, want a manifest line saying %q", out.String(), tc.want)
			}
		})
	}
}

// THE CHECK ASKS, in the order an operator reads: after the host's guard and
// its restart policy, before anything that opens the ledger.
func TestCheckReportsProvenanceBeforeTheLedger(t *testing.T) {
	src, err := os.ReadFile("check.go")
	if err != nil {
		t.Fatal(err)
	}

	body := string(src)
	report := strings.Index(body, "reportInstalledProvenance(env.Stdout, version.Version())")
	guard := strings.Index(body, "checkGuard(env)")
	ledger := strings.Index(body, "integrity verified")

	if report < 0 || guard < 0 || ledger < 0 || guard >= report || report >= ledger {
		t.Errorf("billet check does not report provenance after the guard and before the ledger "+
			"(guard %d, report %d, ledger %d)", guard, report, ledger)
	}
}
