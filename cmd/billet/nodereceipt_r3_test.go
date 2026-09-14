package main

import (
	"os"
	"path/filepath"
	"testing"
)

// THE EVIDENCE'S CONFIGURATION IS A FILE, NOT A SPELLING: the observer opens
// the pathname as it was given (a `..` after a symlinked component is the
// kernel's to resolve, not a lexical clean's), so the evidence a migration
// records carries whatever spelling the role handed it, and the receipt is
// held to the same FILE however the argument spells it. Another file at
// another path is still refused.
func TestTheReceiptComparesTheEvidencesConfigurationByIdentity(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	// A link beside the configuration's directory, and a spelling that goes
	// through it and back out: the kernel resolves it to the same file, a
	// lexical clean would name another directory's.
	dir := filepath.Dir(f.configPath)
	link := filepath.Join(dir, "current")
	mustOK(t, os.Symlink(dir, link))

	// Built by concatenation, never by filepath.Join, which cleans the `..`
	// away lexically and would name another directory.
	through := link + "/../" + filepath.Base(dir) + "/" + filepath.Base(f.configPath)

	for name, spelling := range map[string]string{
		"through a symlinked component": through,
		"as the migration wrote it":     f.configPath,
	} {
		t.Run(name, func(t *testing.T) {
			// The evidence is taken over the spelling, as a migration handed
			// the same argument would record it.
			ev := f.evidenceObject(t, map[string]any{"config_path": spelling})

			// And the receipt is asked with the OTHER spelling.
			other := f.configPath
			if spelling == f.configPath {
				other = through
			}

			o := f.evidence(t, ev, f.confirmationObject(nil), "--config", other)
			mustWritten(t, o)
		})
	}

	// Another file at another path is still another file.
	other := filepath.Join(f.scratch(), "elsewhere.yaml")
	writeFile(t, other, f.rendering(endpointB), 0o644)

	o := f.evidence(t, f.evidenceObject(t, map[string]any{"config_path": other}), f.confirmationObject(nil))
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)

	// And a path that cannot be examined is could-not-tell, never a
	// mismatch.
	gone := filepath.Join(f.scratch(), "gone.yaml")

	o = f.evidence(t, f.evidenceObject(t, map[string]any{"config_path": gone}), f.confirmationObject(nil))
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
}
