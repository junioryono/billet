package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// TestTheDeclarationMustMatchItsPinBeforeAnythingIsBuilt covers the shell's own
// digest check.
//
// THE GO SIDE CHECKING IT IS NOT ENOUGH. This script reads the same file through
// jq and is the thing that actually builds the image, so a check living only in
// Go would leave the builder trusting whatever is on disk.
func TestTheDeclarationMustMatchItsPinBeforeAnythingIsBuilt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		toolset     string
		pin         string
		wantSuccess bool
	}{
		{
			name:        "the vendored file and its pin agree",
			toolset:     string(runnerimages.ToolsetBytes()),
			pin:         runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
			wantSuccess: true,
		},
		{
			name:    "an edited declaration stops the build",
			toolset: `{"apt":{"vital_packages":["curl"],"common_packages":[],"cmd_packages":[]}}`,
			pin:     runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
		},
		{
			name:    "an empty declaration stops the build",
			toolset: "",
			pin:     runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			toolset := filepath.Join(dir, "toolset-2404.json")
			if err := os.WriteFile(toolset, []byte(tc.toolset), 0o600); err != nil {
				t.Fatalf("write the toolset: %v", err)
			}

			pin := filepath.Join(dir, "pinned.txt")
			if err := os.WriteFile(pin, []byte(tc.pin), 0o600); err != nil {
				t.Fatalf("write the pin: %v", err)
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"TOOLSET_FILE=" + toolset + "\nTOOLSET_PIN=" + pin + "\nTOOLSET_PIN_KEY=x64\n" +
				guestImageFunction(t, "verify_toolset") + "\n" +
				"verify_toolset\necho VERIFIED\n"

			out, err := runHarness(t, script)

			if got := err == nil; got != tc.wantSuccess {
				t.Fatalf("verify_toolset success = %v, want %v\n%s", got, tc.wantSuccess, out)
			}

			// THE BUILD MUST NOT CONTINUE, not merely complain. A version that
			// printed a warning and carried on would satisfy an exit-status check
			// on the happy path and build an image from an unreviewed declaration.
			if got := strings.Contains(out, "VERIFIED"); got != tc.wantSuccess {
				t.Errorf("the build continued past verification = %v, want %v\n%s",
					got, tc.wantSuccess, out)
			}
		})
	}
}

func runHarness(t *testing.T, script string) (string, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := forkSafeWriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	// A SCRIPT THIS PROCESS JUST WROTE IS RUN THROUGH bash, NOT EXECUTED.
	//
	// ETXTBSY, and it is not hypothetical: CI failed twice with
	// "fork/exec …/run.sh: text file busy". os.WriteFile closes the file before the
	// exec, so single-threaded this is safe. In a PARALLEL test binary another
	// goroutine can fork for its own exec while this file's descriptor is still
	// open for writing, the child inherits it, and the kernel refuses to execve a
	// file any process holds open for writing until that child execs or exits. Go
	// narrows the window with O_CLOEXEC and ForkLock but does not close it
	// (golang/go#22315).
	//
	// Passing the path as an ARGUMENT means the file is only ever READ, so the race
	// cannot arise. Every harness here starts `#!/usr/bin/env bash`, and
	// `bash script args` propagates the script's exit status — which is what these
	// tests assert on.
	out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()

	return string(out), err
}

// EACH MACHINE'S TOOLSET IS HELD TO ITS OWN LINE OF THE PIN, the first for x64 and the
// one named for arm64: the wrong line reads as an edit, and a line that is not there
// must not read as a declaration nobody pinned.
func TestAToolsetIsHeldToItsMachinesLineOfThePin(t *testing.T) {
	t.Parallel()

	body := []byte("{}\n")
	sum := sha256.Sum256(body)
	right := hex.EncodeToString(sum[:])
	wrong := strings.Repeat("0", 64)

	for name, tc := range map[string]struct {
		key, pin string
		want     bool
	}{
		"x64's line":                {"x64", "commit " + right + "\narm64 " + wrong + "\n", true},
		"arm64's line":              {"arm64", "commit " + wrong + "\narm64 " + right + "\n", true},
		"arm64 reading x64's":       {"arm64", "commit " + right + "\narm64 " + wrong + "\n", false},
		"x64 reading arm64's":       {"x64", "commit " + wrong + "\narm64 " + right + "\n", false},
		"arm64 with no line at all": {"arm64", "commit " + right + "\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			toolset := filepath.Join(dir, "toolset.json")
			pin := filepath.Join(dir, "pinned.txt")

			if err := os.WriteFile(toolset, body, 0o600); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(pin, []byte(tc.pin), 0o600); err != nil {
				t.Fatal(err)
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"TOOLSET_FILE=" + toolset + "\nTOOLSET_PIN=" + pin + "\nTOOLSET_PIN_KEY=" + tc.key + "\n" +
				guestImageFunction(t, "verify_toolset") + "\nverify_toolset\necho VERIFIED\n"

			out, err := runHarness(t, script)
			if got := err == nil && strings.Contains(out, "VERIFIED"); got != tc.want {
				t.Fatalf("verified = %v, want %v (%v)\n%s", got, tc.want, err, out)
			}
		})
	}
}
