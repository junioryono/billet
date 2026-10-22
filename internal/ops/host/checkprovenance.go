package host

import (
	"errors"
	"fmt"
	"io"

	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/version"
)

// installedProvenanceDigest is provenance.Installed behind a seam, so a test can be a
// host with a record, without one, or with one naming other bytes.
var installedProvenanceDigest = provenance.Installed

// reportInstalledProvenance says whether anything on this host records which
// release manifest produced the running binary, which is how a rollout reads
// the host: a digest proves the bytes, and without one a rollout has only the
// version string two builds can share (#91). ADVISORY, in three answers: a
// record that proves these bytes, none, and one that names other bytes; a
// record that cannot be read is said to be. It decides nothing, and a build of
// a commit, which no release manifest names, says nothing.
func reportInstalledProvenance(w io.Writer, release string) {
	if !version.IsRelease(release) {
		return
	}

	digest, err := installedProvenanceDigest()

	switch {
	case err == nil:
		_, _ = fmt.Fprintf(w, "manifest %s produced this %s binary\n", shortDigest(digest), release)
	case errors.Is(err, provenance.ErrNoRecord):
		_, _ = fmt.Fprintf(w, "manifest nothing here records which release manifest produced this %s "+
			"binary, so a rollout reads its version rather than its bytes (billet status: NAMED NO "+
			"MANIFEST). install.sh and the host role record one, and so does every upgrade a rollout "+
			"makes\n", release)
	case errors.Is(err, provenance.ErrNotThisBinary):
		_, _ = fmt.Fprintf(w, "manifest THE RECORD NAMES OTHER BYTES THAN THIS BINARY: something replaced "+
			"billet without recording it, so a rollout cannot prove what this host runs: %v\n", err)
	default:
		_, _ = fmt.Fprintf(w, "manifest could not tell which release manifest produced this binary: %v\n", err)
	}
}

// shortDigest renders a digest for a line an operator reads.
func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}

	return digest[:12]
}
