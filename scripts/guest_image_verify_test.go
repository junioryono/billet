package scripts_test

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A VERIFIER THAT CAN SILENTLY STOP VERIFYING IS WORSE THAN NO VERIFIER.
//
// fetch_verified is the one place a vendor's bytes are checked before they are
// unpacked into an image billet publishes. It grew a second algorithm because .NET
// publishes SHA-512 only — 128 hex characters, which sha256sum reads as a malformed
// line rather than as a mismatch — and the branch that makes that safe is the one
// that REFUSES an algorithm it does not know instead of falling through to no check
// at all. None of that was executed by anything: the encoding test next door
// exercises the sums-file reader, not this.
func TestNothingIsUnpackedWithoutItsVendorsChecksum(t *testing.T) {
	t.Parallel()

	const body = "the bytes a vendor served\n"

	sha256sum := sha256.Sum256([]byte(body))
	sha512sum := sha512.Sum512([]byte(body))

	correct256 := hex.EncodeToString(sha256sum[:])
	correct512 := hex.EncodeToString(sha512sum[:])

	// A DIGEST OF THE RIGHT SHAPE AND THE WRONG VALUE. A wrong-LENGTH digest is
	// refused by the checksum tool's parser, which is a different rejection and
	// would let a real mismatch through while the test went green.
	wrong256 := strings.Repeat("0", 64)
	wrong512 := strings.Repeat("0", 128)

	for _, tc := range []struct {
		name    string
		want    string
		algo    string
		wantErr bool
	}{
		{name: "sha256 matches", want: correct256, algo: "sha256"},
		{name: "sha512 matches", want: correct512, algo: "sha512"},
		{name: "the default is sha256", want: correct256},

		{name: "sha256 mismatch", want: wrong256, algo: "sha256", wantErr: true},
		{name: "sha512 mismatch", want: wrong512, algo: "sha512", wantErr: true},

		// THE TWO THAT MAKE IT A VERIFIER RATHER THAN A DOWNLOADER.
		{name: "an unknown algorithm refuses", want: correct256, algo: "sha3", wantErr: true},
		{name: "an empty digest refuses", want: "", algo: "sha256", wantErr: true},

		// A DIGEST FOR THE OTHER ALGORITHM IS NOT A PASS. Handing sha256sum a
		// 128-character line is exactly the .NET case that started this, and it
		// must read as a failure rather than as a line to skip.
		{name: "a sha512 digest under sha256", want: correct512, algo: "sha256", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			out := filepath.Join(dir, "downloaded")

			// A FAKE curl THAT SERVES KNOWN BYTES. fetch_verified calls curl
			// unqualified, so a directory ahead of it on PATH is the whole seam --
			// and the fake honours -o, because writing to stdout would make every
			// case fail for a reason that is not the checksum.
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o700); err != nil {
				t.Fatalf("make the fake bin: %v", err)
			}

			fake := "#!/usr/bin/env bash\nwhile [ $# -gt 0 ]; do\n" +
				"  if [ \"$1\" = -o ]; then printf '%s' " +
				shellQuote(body) + " >\"$2\"; exit 0; fi\n  shift\ndone\nexit 1\n"

			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o700); err != nil {
				t.Fatalf("write the fake curl: %v", err)
			}

			args := "\"https://example.invalid/x\" " + shellQuote(out) + " " + shellQuote(tc.want)
			if tc.algo != "" {
				args += " " + shellQuote(tc.algo)
			}

			body := "#!/usr/bin/env bash\nset -uo pipefail\n" +
				guestImageFunction(t, "fetch_verified") + "\n" +
				"fetch_verified " + args + "\n"

			script := filepath.Join(dir, "run.sh")
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatalf("write the harness: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))

			combined, err := cmd.CombinedOutput()

			switch {
			case tc.wantErr && err == nil:
				t.Errorf("fetch_verified accepted the download; a vendor's bytes reach "+
					"an image billet publishes without having been checked\n%s", combined)
			case !tc.wantErr && err != nil:
				t.Errorf("fetch_verified refused a correct digest: %v\n%s", err, combined)
			}

			// AND A REFUSED DOWNLOAD LEAVES NOTHING BEHIND. A caller that ignores
			// the status — or a `set -e` that a subshell swallows — would otherwise
			// unpack the very file this refused.
			if tc.wantErr {
				if _, statErr := os.Stat(out); statErr == nil {
					t.Errorf("the download was refused and %s still exists, so anything "+
						"that unpacks by path gets the unverified bytes anyway", out)
				}
			}
		})
	}
}
